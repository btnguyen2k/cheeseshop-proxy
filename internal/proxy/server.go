package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/btnguyen2k/cheeseshop-proxy/internal/cache"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/config"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/policy"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/upstream"
)

type Server struct {
	cfg    config.Config
	logger *slog.Logger
	allow  *policy.Allowlist
	cache  *cache.Cache
	client *upstream.Client
	links  *links
	prefix string
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	return newServer(ctx, cfg, logger, nil)
}

func newServer(ctx context.Context, cfg config.Config, logger *slog.Logger, transport http.RoundTripper) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	allow, err := policy.New(cfg.Allow)
	if err != nil {
		return nil, err
	}
	storage, err := cache.New(ctx, cache.Options{
		Dir: cfg.Cache.Dir, MaxBytes: cfg.Cache.MaxBytes, MaxObjectBytes: cfg.Cache.MaxObjectBytes,
		FetchTimeout: cfg.Upstream.Timeout,
	}, logger)
	if err != nil {
		return nil, err
	}
	linkSigner, err := loadLinks(cfg.Cache.Dir)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(cfg.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, logger: logger, allow: allow, cache: storage, links: linkSigner,
		client: upstream.New(cfg.Upstream.Timeout, cfg.Upstream.MaxConcurrent, transport),
		prefix: base.Path,
	}, nil
}

func (s *Server) Close() {
	s.client.CloseIdleConnections()
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	response := &responseLog{ResponseWriter: writer}
	start := time.Now()
	defer func() {
		if response.err != nil {
			s.logger.Warn("write response", "path", request.URL.Path, "error", response.err)
		}
		s.logger.Info("request", "method", request.Method, "path", request.URL.Path,
			"status", response.status, "bytes", response.bytes, "duration", time.Since(start).String())
	}()
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		s.writeError(response, request, http.StatusMethodNotAllowed)
		return
	}
	escaped := strings.ToLower(request.URL.RawPath)
	if strings.ContainsAny(request.URL.Path, "\\\x00\r\n") ||
		strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		s.writeError(response, request, http.StatusNotFound)
		return
	}
	switch request.URL.Path {
	case "/healthz":
		writeText(response, request, "ok\n")
		return
	case "/readyz":
		if err := s.cache.Check(); err != nil {
			s.logger.Error("cache not ready", "error", err)
			s.writeError(response, request, http.StatusServiceUnavailable)
		} else if err := s.ready(request.Context()); err != nil {
			s.logger.Error("upstream not ready", "error", err)
			s.writeError(response, request, http.StatusServiceUnavailable)
		} else {
			writeText(response, request, "ready\n")
		}
		return
	}
	var err error
	switch {
	case request.URL.Path == s.prefix+"/simple":
		http.Redirect(response, request, s.publicURL("/simple/"), http.StatusPermanentRedirect)
	case request.URL.Path == s.prefix+"/simple/":
		err = s.simple(response, request, "")
	case strings.HasPrefix(request.URL.Path, s.prefix+"/simple/"):
		name := strings.TrimPrefix(request.URL.Path, s.prefix+"/simple/")
		name = strings.TrimSuffix(name, "/")
		if !s.allow.Allows(name) {
			s.writeError(response, request, http.StatusNotFound)
		} else if canonical := s.prefix + "/simple/" + policy.Normalize(name) + "/"; request.URL.Path != canonical {
			http.Redirect(response, request, s.publicURL("/simple/"+policy.Normalize(name)+"/"), http.StatusPermanentRedirect)
		} else {
			err = s.simple(response, request, name)
		}
	case strings.HasPrefix(request.URL.Path, s.prefix+"/packages/"):
		err = s.packageContent(response, request, strings.TrimPrefix(request.URL.Path, s.prefix+"/packages/"))
	default:
		s.writeError(response, request, http.StatusNotFound)
	}
	if err != nil {
		s.respondError(response, request, err)
	}
}

func (s *Server) packageContent(writer http.ResponseWriter, request *http.Request, relative string) error {
	parts := strings.Split(relative, "/")
	if len(parts) != 3 || !s.allow.Allows(parts[0]) || parts[0] != policy.Normalize(parts[0]) {
		s.writeError(writer, request, http.StatusNotFound)
		return nil
	}
	rawURL, ok := s.links.resolve("artifact:"+parts[0], parts[1])
	if !ok {
		s.writeError(writer, request, http.StatusNotFound)
		return nil
	}
	remote, err := url.Parse(rawURL)
	if err != nil || upstream.ValidateURL(rawURL) != nil || !strings.EqualFold(remote.Hostname(), upstream.ArtifactHost) {
		s.writeError(writer, request, http.StatusNotFound)
		return nil
	}
	filename := remote.Path[strings.LastIndex(remote.Path, "/")+1:]
	if !validFilename(filename) || (parts[2] != filename && parts[2] != filename+".metadata") {
		s.writeError(writer, request, http.StatusNotFound)
		return nil
	}
	if parts[2] == filename+".metadata" {
		remote.Path += ".metadata"
		remote.RawPath = ""
	}
	return s.serveObject(writer, request, remote.String(), 0)
}

func (s *Server) ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Upstream.Timeout)
	defer cancel()
	response, err := s.client.Head(ctx)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &cache.HTTPError{StatusCode: response.StatusCode}
	}
	return validateContentType(response.Header.Get("Content-Type"))
}

func (s *Server) simple(writer http.ResponseWriter, request *http.Request, name string) error {
	writer.Header().Set("Vary", "Accept")
	contentType, ok := negotiate(request.Header.Get("Accept"))
	if !ok {
		s.writeError(writer, request, http.StatusNotAcceptable)
		return nil
	}
	if name == "" {
		document, err := s.projectList(request.Context())
		if err != nil {
			return err
		}
		if contentType == upstream.JSONType {
			return serveJSON(writer, request, document, contentType)
		}
		return serveBytes(writer, request, s.projectListHTML(document), contentType)
	}
	document, err := s.project(request.Context(), name)
	if err != nil {
		return err
	}
	if contentType == upstream.JSONType {
		return serveJSON(writer, request, document, contentType)
	}
	return serveBytes(writer, request, projectHTML(document), contentType)
}

func (s *Server) get(ctx context.Context, rawURL string, ttl time.Duration) (*cache.Object, error) {
	return s.cache.Get(ctx, rawURL, ttl, func(ctx context.Context, etag, modified string) (*http.Response, error) {
		return s.client.Get(ctx, rawURL, etag, modified)
	})
}

func (s *Server) serveObject(writer http.ResponseWriter, request *http.Request, rawURL string, ttl time.Duration) error {
	object, err := s.get(request.Context(), rawURL, ttl)
	if err != nil {
		return err
	}
	defer s.closeObject(object)
	if object.ContentType != "" {
		writer.Header().Set("Content-Type", object.ContentType)
	}
	if object.ETag != "" {
		writer.Header().Set("ETag", object.ETag)
	}
	var modified time.Time
	if parsed, err := http.ParseTime(object.LastModified); err == nil {
		modified = parsed
	}
	http.ServeContent(writer, request, request.URL.Path, modified, object)
	return nil
}

func (s *Server) closeObject(object *cache.Object) {
	if err := object.Close(); err != nil {
		s.logger.Error("close cached object", "error", err)
	}
}

func (s *Server) publicURL(suffix string) string {
	return s.cfg.PublicBaseURL + suffix
}

func (s *Server) respondError(writer http.ResponseWriter, request *http.Request, err error) {
	status := http.StatusInternalServerError
	var remote *cache.HTTPError
	var fetch *cache.FetchError
	var network net.Error
	switch {
	case errors.As(err, &remote):
		switch remote.StatusCode {
		case http.StatusNotFound:
			status = http.StatusNotFound
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			status = remote.StatusCode
			if remote.RetryAfter != "" {
				if seconds, parseErr := strconv.Atoi(remote.RetryAfter); parseErr == nil && seconds >= 0 {
					writer.Header().Set("Retry-After", remote.RetryAfter)
				} else if _, parseErr := http.ParseTime(remote.RetryAfter); parseErr == nil {
					writer.Header().Set("Retry-After", remote.RetryAfter)
				}
			}
		default:
			status = http.StatusBadGateway
		}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, cache.ErrFull):
		status = http.StatusServiceUnavailable
	case errors.Is(err, errBadGateway), errors.Is(err, cache.ErrTooLarge), errors.As(err, &fetch):
		status = http.StatusBadGateway
	}
	s.logger.Error("request failed", "path", request.URL.Path, "status", status, "error", err)
	s.writeError(writer, request, status)
}

func (s *Server) writeError(writer http.ResponseWriter, request *http.Request, status int) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := http.StatusText(status) + "\n"
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		if _, err := writer.Write([]byte(body)); err != nil {
			s.logger.Warn("write error response", "error", err)
		}
	}
}

func serveJSON(writer http.ResponseWriter, request *http.Request, value any, contentType string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	return serveBytes(writer, request, data, contentType)
}

func serveBytes(writer http.ResponseWriter, request *http.Request, data []byte, contentType string) error {
	sum := sha256.Sum256(data)
	writer.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	writer.Header().Set("Content-Type", contentType)
	http.ServeContent(writer, request, request.URL.Path, time.Time{}, bytes.NewReader(data))
	return nil
}

func writeText(writer http.ResponseWriter, request *http.Request, text string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Content-Length", strconv.Itoa(len(text)))
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write([]byte(text))
	}
}

type responseLog struct {
	http.ResponseWriter
	status int
	bytes  int
	err    error
}

func (r *responseLog) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseLog) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytes += n
	if err != nil {
		r.err = err
	}
	return n, err
}

func (r *responseLog) Unwrap() http.ResponseWriter { return r.ResponseWriter }
