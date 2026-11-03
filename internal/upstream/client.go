package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

const (
	IndexURL     = "https://pypi.org/simple/"
	JSONType     = "application/vnd.pypi.simple.v1+json"
	HTMLType     = "application/vnd.pypi.simple.v1+html"
	ArtifactHost = "files.pythonhosted.org"
)

type Client struct {
	http  *http.Client
	slots *semaphore.Weighted
}

func New(timeout time.Duration, concurrency int, transport http.RoundTripper) *Client {
	if transport == nil {
		defaultTransport := http.DefaultTransport.(*http.Transport).Clone()
		defaultTransport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		defaultTransport.MaxIdleConns = concurrency * 2
		defaultTransport.MaxIdleConnsPerHost = concurrency
		defaultTransport.MaxConnsPerHost = concurrency
		defaultTransport.ResponseHeaderTimeout = timeout
		defaultTransport.MaxResponseHeaderBytes = 1 << 20
		transport = defaultTransport
	}
	return &Client{
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many upstream redirects")
				}
				if err := ValidateURL(request.URL.String()); err != nil {
					return err
				}
				if !strings.EqualFold(request.URL.Hostname(), via[0].URL.Hostname()) {
					return errors.New("upstream redirect changed resource host")
				}
				return nil
			},
		},
		slots: semaphore.NewWeighted(int64(concurrency)),
	}
}

func ValidateURL(raw string) error {
	if len(raw) > 8192 {
		return errors.New("upstream URL is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid upstream URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Opaque != "" || strings.Contains(parsed.Path, "\\") ||
		(parsed.Port() != "" && parsed.Port() != "443") ||
		(host != "pypi.org" && host != ArtifactHost) {
		return errors.New("upstream URL must use HTTPS on pypi.org or files.pythonhosted.org")
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			return errors.New("upstream URL contains a dot segment")
		}
	}
	if (host == "pypi.org" && !strings.HasPrefix(parsed.Path, "/simple/")) ||
		(host == ArtifactHost && !strings.HasPrefix(parsed.Path, "/packages/")) {
		return errors.New("upstream URL is outside the package index or artifact paths")
	}
	return nil
}

func (c *Client) Get(ctx context.Context, rawURL, etag, lastModified string) (*http.Response, error) {
	return c.request(ctx, http.MethodGet, rawURL, etag, lastModified)
}

func (c *Client) Head(ctx context.Context) (*http.Response, error) {
	return c.request(ctx, http.MethodHead, IndexURL, "", "")
}

func (c *Client) request(ctx context.Context, method, rawURL, etag, lastModified string) (*http.Response, error) {
	if err := ValidateURL(rawURL); err != nil {
		return nil, err
	}
	if err := c.slots.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		c.slots.Release(1)
		return nil, err
	}
	request.Header.Set("User-Agent", "cheeseshop")
	if strings.EqualFold(request.URL.Hostname(), "pypi.org") {
		request.Header.Set("Accept", JSONType)
	}
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		request.Header.Set("If-Modified-Since", lastModified)
	}
	response, err := c.http.Do(request)
	if err != nil {
		c.slots.Release(1)
		return nil, err
	}
	response.Body = &releaseBody{ReadCloser: response.Body, release: func() { c.slots.Release(1) }}
	return response, nil
}

func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
