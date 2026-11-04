package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/btnguyen2k/cheeseshop-proxy/internal/cache"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/policy"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/upstream"
)

var (
	errBadGateway = errors.New("invalid upstream metadata")
	hashName      = regexp.MustCompile(`^[a-z0-9_]+$`)
)

type apiMeta struct {
	Version string `json:"api-version"`
}

type projectName struct {
	Name string `json:"name"`
}

type projectList struct {
	Meta     apiMeta       `json:"meta"`
	Projects []projectName `json:"projects"`
}

type projectDocument struct {
	Meta     apiMeta        `json:"meta"`
	Name     string         `json:"name"`
	Files    []distribution `json:"files"`
	Versions []string       `json:"versions"`
}

type distribution struct {
	Filename         string            `json:"filename"`
	URL              string            `json:"url"`
	Hashes           map[string]string `json:"hashes"`
	RequiresPython   *string           `json:"requires-python,omitempty"`
	Yanked           any               `json:"yanked,omitempty"`
	CoreMetadata     any               `json:"core-metadata,omitempty"`
	DistInfoMetadata any               `json:"dist-info-metadata,omitempty"`
	Size             *int64            `json:"size,omitempty"`
	UploadTime       string            `json:"upload-time,omitempty"`
}

func validateContentType(raw string) error {
	contentType, _, err := mime.ParseMediaType(raw)
	if err != nil || contentType != upstream.JSONType {
		return fmt.Errorf("%w: expected Simple API JSON", errBadGateway)
	}
	return nil
}

func apiMinor(version string) (int, error) {
	major, minor, ok := strings.Cut(version, ".")
	number, err := strconv.Atoi(minor)
	if !ok || major != "1" || strings.Trim(minor, "0123456789") != "" || err != nil || number < 0 {
		return 0, fmt.Errorf("%w: unsupported API version %q", errBadGateway, version)
	}
	return number, nil
}

func (s *Server) metadata(ctx context.Context, rawURL string) (*cache.Object, error) {
	object, err := s.get(ctx, rawURL, s.cfg.Cache.MetadataTTL)
	if err != nil {
		return nil, err
	}
	if err := validateContentType(object.ContentType); err != nil {
		s.closeObject(object)
		return nil, err
	}
	return object, nil
}

func finishJSON(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON content", errBadGateway)
	}
	return nil
}

func (s *Server) projectList(ctx context.Context) (projectList, error) {
	result := projectList{Meta: apiMeta{Version: "1.1"}, Projects: []projectName{}}
	if len(s.cfg.Allow) == 0 {
		return result, nil
	}
	object, err := s.metadata(ctx, upstream.IndexURL)
	if err != nil {
		return result, err
	}
	defer s.closeObject(object)
	decoder := json.NewDecoder(object)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return result, fmt.Errorf("%w: invalid project list", errBadGateway)
	}
	var hasMeta, hasProjects bool
	seen := make(map[string]bool)
	// Stream the catalog so narrow allowlists do not materialize all PyPI names.
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return result, fmt.Errorf("%w: %v", errBadGateway, err)
		}
		switch key {
		case "meta":
			var meta apiMeta
			if hasMeta || decoder.Decode(&meta) != nil {
				return result, fmt.Errorf("%w: invalid project-list metadata", errBadGateway)
			}
			if _, err := apiMinor(meta.Version); err != nil {
				return result, err
			}
			hasMeta = true
		case "projects":
			token, err := decoder.Token()
			if hasProjects || err != nil || token != json.Delim('[') {
				return result, fmt.Errorf("%w: invalid projects array", errBadGateway)
			}
			for decoder.More() {
				var item projectName
				if err := decoder.Decode(&item); err != nil {
					return result, fmt.Errorf("%w: %v", errBadGateway, err)
				}
				if s.allow.Allows(item.Name) {
					name := policy.Normalize(item.Name)
					if !seen[name] {
						result.Projects = append(result.Projects, projectName{Name: name})
						seen[name] = true
					}
				}
			}
			if _, err := decoder.Token(); err != nil {
				return result, fmt.Errorf("%w: %v", errBadGateway, err)
			}
			hasProjects = true
		default:
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return result, fmt.Errorf("%w: %v", errBadGateway, err)
			}
		}
	}
	if _, err := decoder.Token(); err != nil || !hasMeta || !hasProjects {
		return result, fmt.Errorf("%w: incomplete project list", errBadGateway)
	}
	if err := finishJSON(decoder); err != nil {
		return result, err
	}
	sort.Slice(result.Projects, func(i, j int) bool { return result.Projects[i].Name < result.Projects[j].Name })
	return result, nil
}

func (s *Server) project(ctx context.Context, name string) (projectDocument, error) {
	rawURL := upstream.IndexURL + name + "/"
	object, err := s.metadata(ctx, rawURL)
	if err != nil {
		return projectDocument{}, err
	}
	defer s.closeObject(object)
	decoder := json.NewDecoder(object)
	var document projectDocument
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%w: %v", errBadGateway, err)
	}
	if err := finishJSON(decoder); err != nil {
		return document, err
	}
	minor, err := apiMinor(document.Meta.Version)
	if err != nil {
		return document, err
	}
	if !policy.ValidName(document.Name) || policy.Normalize(document.Name) != name || document.Files == nil ||
		(minor >= 1 && document.Versions == nil) {
		return document, fmt.Errorf("%w: invalid project identity, files, or versions", errBadGateway)
	}
	document.Name = name
	if document.Versions == nil {
		document.Versions = []string{}
	}
	document.Meta.Version = "1.0"
	if minor >= 1 {
		document.Meta.Version = "1.1"
	}
	base, _ := url.Parse(rawURL)
	seen := make(map[string]bool)
	for i := range document.Files {
		file := &document.Files[i]
		if !validFilename(file.Filename) || seen[file.Filename] || file.Hashes == nil ||
			(minor >= 1 && file.Size == nil) || (file.Size != nil && *file.Size < 0) {
			return document, fmt.Errorf("%w: invalid distribution", errBadGateway)
		}
		seen[file.Filename] = true
		if err := validateHashes(file.Hashes); err != nil {
			return document, err
		}
		switch value := file.Yanked.(type) {
		case nil, bool:
		case string:
			if value == "" {
				return document, fmt.Errorf("%w: empty yanked reason", errBadGateway)
			}
		default:
			return document, fmt.Errorf("%w: invalid yanked value", errBadGateway)
		}
		if file.UploadTime != "" {
			if _, err := time.Parse(time.RFC3339Nano, file.UploadTime); err != nil {
				return document, fmt.Errorf("%w: invalid upload time", errBadGateway)
			}
		}
		value := file.CoreMetadata
		if value == nil {
			value = file.DistInfoMetadata
		}
		metadata, err := metadataValue(value)
		if err != nil {
			return document, err
		}
		file.CoreMetadata, file.DistInfoMetadata = metadata, metadata
		remote, err := url.Parse(file.URL)
		if err != nil || file.URL == "" {
			return document, fmt.Errorf("%w: invalid file URL", errBadGateway)
		}
		remote = base.ResolveReference(remote)
		if remote.Fragment != "" {
			algorithm, digest, ok := strings.Cut(remote.Fragment, "=")
			if !ok || (file.Hashes[algorithm] != "" && file.Hashes[algorithm] != digest) {
				return document, fmt.Errorf("%w: invalid file hash fragment", errBadGateway)
			}
			file.Hashes[algorithm] = digest
			if err := validateHashes(file.Hashes); err != nil {
				return document, err
			}
			remote.Fragment, remote.RawFragment = "", ""
		}
		if err := upstream.ValidateURL(remote.String()); err != nil ||
			!strings.EqualFold(remote.Hostname(), upstream.ArtifactHost) ||
			remote.Path[strings.LastIndex(remote.Path, "/")+1:] != file.Filename {
			return document, fmt.Errorf("%w: untrusted or mismatched file URL", errBadGateway)
		}
		token := s.links.sign("artifact:"+name, remote.String())
		file.URL = s.publicURL("/packages/" + name + "/" + token + "/" + url.PathEscape(file.Filename))
	}
	return document, nil
}

func validFilename(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 1024 || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validateHashes(hashes map[string]string) error {
	for algorithm, digest := range hashes {
		if !hashName.MatchString(algorithm) || len(digest) == 0 || len(digest) > 512 {
			return fmt.Errorf("%w: invalid hash", errBadGateway)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("%w: invalid hash digest", errBadGateway)
		}
	}
	return nil
}

func metadataValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil, bool:
		return value, nil
	case map[string]any:
		hashes := make(map[string]string, len(typed))
		for algorithm, value := range typed {
			digest, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%w: invalid metadata hash", errBadGateway)
			}
			hashes[algorithm] = digest
		}
		return hashes, validateHashes(hashes)
	default:
		return nil, fmt.Errorf("%w: invalid core metadata value", errBadGateway)
	}
}
