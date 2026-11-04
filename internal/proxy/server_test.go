package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btnguyen2k/cheeseshop-proxy/internal/config"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/upstream"
)

const (
	artifactURL = "https://files.pythonhosted.org/packages/fixture/Friendly_Bard-1.0.0-py3-none-any.whl"
	projectURL  = upstream.IndexURL + "friendly-bard/"
)

type fixtureReply struct {
	status int
	body   []byte
	header http.Header
	err    error
}

type fixture struct {
	t             *testing.T
	mu            sync.Mutex
	replies       map[string]fixtureReply
	calls         map[string]int
	heads         int
	authorization bool
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fixtureProject() projectDocument {
	body := []byte("PK\x03\x04unchanged-wheel-bytes\x00\xff")
	size, requires := int64(len(body)), ">=3.8"
	return projectDocument{
		Meta: apiMeta{Version: "1.4"}, Name: "friendly-bard", Versions: []string{"1.0.0"},
		Files: []distribution{{
			Filename: "Friendly_Bard-1.0.0-py3-none-any.whl", URL: artifactURL,
			Hashes: map[string]string{"sha256": checksum(body)}, RequiresPython: &requires,
			Yanked: "<retired & unsafe>", CoreMetadata: map[string]string{"sha256": checksum([]byte("core metadata"))},
			Size: &size, UploadTime: "2026-10-01T00:00:00Z",
		}},
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, replies: make(map[string]fixtureReply), calls: make(map[string]int)}
	f.setJSON(upstream.IndexURL, projectList{Meta: apiMeta{Version: "1.4"}, Projects: []projectName{
		{Name: "Friendly_Bard"}, {Name: "friendly--bard"}, {Name: "company-core"}, {Name: "outside-subset"},
	}})
	f.setJSON(projectURL, fixtureProject())
	f.replies[artifactURL] = fixtureReply{
		status: 200, body: []byte("PK\x03\x04unchanged-wheel-bytes\x00\xff"),
		header: http.Header{"Content-Type": {"application/octet-stream"}, "Etag": {`"wheel-v1"`},
			"Last-Modified": {"Thu, 01 Oct 2026 00:00:00 GMT"}},
	}
	f.replies[artifactURL+".metadata"] = fixtureReply{
		status: 200, body: []byte("core metadata"), header: http.Header{"Content-Type": {"text/plain"}},
	}
	return f
}

func (f *fixture) setJSON(rawURL string, value any) {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[rawURL] = fixtureReply{
		status: 200, body: data, header: http.Header{"Content-Type": {upstream.JSONType}, "Etag": {`"origin-index"`}},
	}
}

func (f *fixture) RoundTrip(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if request.Method == http.MethodHead {
		f.heads++
	} else {
		f.calls[request.URL.String()]++
	}
	f.authorization = f.authorization || request.Header.Get("Authorization") != ""
	reply, ok := f.replies[request.URL.String()]
	f.mu.Unlock()
	if request.Header.Get("User-Agent") != "cheeseshop" ||
		(request.URL.Host == "pypi.org" && request.Header.Get("Accept") != upstream.JSONType) {
		f.t.Error("upstream request did not use the application identity or Simple JSON")
	}
	if !ok {
		reply.status = http.StatusNotFound
	}
	if reply.err != nil {
		return nil, reply.err
	}
	var body io.ReadCloser = io.NopCloser(bytes.NewReader(reply.body))
	if request.Method == http.MethodHead {
		body = http.NoBody
	}
	return &http.Response{
		StatusCode: reply.status, Header: reply.header.Clone(), Body: body,
		ContentLength: int64(len(reply.body)), Request: request,
	}, nil
}

func (f *fixture) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int
	for _, count := range f.calls {
		total += count
	}
	return total
}

func (f *fixture) callCount(rawURL string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[rawURL]
}

func testServer(t *testing.T, f *fixture, change func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.PublicBaseURL = "https://feed.example.com/python"
	cfg.Allow = []string{"Friendly.Bard", "company-*"}
	cfg.Cache.Dir = t.TempDir()
	cfg.Cache.MetadataTTL = time.Hour
	cfg.Upstream.Timeout = 5 * time.Second
	if change != nil {
		change(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app, err := newServer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), f)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); app.Close() })
	return app
}

func request(app *Server, method, path string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = "untrusted-host.example"
	for name, values := range headers {
		req.Header[name] = values
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, req)
	return response
}

func projectResponse(t *testing.T, app *Server) (projectDocument, *httptest.ResponseRecorder) {
	t.Helper()
	result := request(app, "GET", "/python/simple/friendly-bard/", http.Header{"Accept": {upstream.JSONType}})
	var document projectDocument
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &document) != nil || len(document.Files) != 1 {
		t.Fatalf("project response: %d %s", result.Code, result.Body.String())
	}
	return document, result
}

func TestSimpleListingsAndResolverMetadata(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	root := request(app, "GET", "/python/simple/", http.Header{"Accept": {upstream.JSONType}})
	var listing projectList
	if root.Code != 200 || json.Unmarshal(root.Body.Bytes(), &listing) != nil ||
		!reflect.DeepEqual(listing.Projects, []projectName{{Name: "company-core"}, {Name: "friendly-bard"}}) {
		t.Fatalf("unfiltered or invalid root: %d %s", root.Code, root.Body.String())
	}
	html := request(app, "GET", "/python/simple/", nil)
	if html.Code != 200 || !strings.Contains(html.Body.String(), app.publicURL("/simple/company-core/")) ||
		strings.Contains(html.Body.String(), "outside-subset") {
		t.Fatalf("root HTML: %d %s", html.Code, html.Body.String())
	}
	document, result := projectResponse(t, app)
	file := document.Files[0]
	if document.Meta.Version != "1.1" || !strings.HasPrefix(file.URL, app.publicURL("/packages/friendly-bard/")) ||
		!strings.HasSuffix(file.URL, "/Friendly_Bard-1.0.0-py3-none-any.whl") ||
		file.Yanked != "<retired & unsafe>" || file.RequiresPython == nil || *file.RequiresPython != ">=3.8" ||
		file.Hashes["sha256"] != checksum(f.replies[artifactURL].body) || file.CoreMetadata == nil ||
		file.Size == nil || file.UploadTime == "" {
		t.Fatalf("resolver metadata changed: %+v", document)
	}
	html = request(app, "GET", "/python/simple/friendly-bard/", nil)
	for _, fragment := range []string{
		"#sha256=" + file.Hashes["sha256"], `data-requires-python="&gt;=3.8"`,
		`data-yanked="&lt;retired &amp; unsafe&gt;"`, `data-core-metadata="sha256=`, `data-dist-info-metadata="sha256=`,
	} {
		if !strings.Contains(html.Body.String(), fragment) {
			t.Errorf("HTML missing %q: %s", fragment, html.Body.String())
		}
	}
	if result.Header().Get("Vary") != "Accept" || result.Header().Get("ETag") == `"origin-index"` ||
		html.Header().Get("ETag") == result.Header().Get("ETag") {
		t.Fatal("rewritten representations did not get independent cache validators")
	}
	conditional := request(app, "GET", "/python/simple/friendly-bard/",
		http.Header{"Accept": {upstream.JSONType}, "If-None-Match": {result.Header().Get("ETag")}})
	if conditional.Code != 304 || conditional.Body.Len() != 0 {
		t.Fatalf("conditional project request: %d %s", conditional.Code, conditional.Body.String())
	}
}

func TestContentNegotiationAndEmptyAllowlist(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, func(cfg *config.Config) { cfg.Allow = nil })
	for _, test := range []struct {
		accept string
		want   string
	}{
		{"", upstream.HTMLType}, {"*/*", upstream.HTMLType}, {"text/html", "text/html"},
		{upstream.JSONType, upstream.JSONType},
		{"application/vnd.pypi.simple.latest+json", upstream.JSONType},
		{upstream.JSONType + ";q=1, " + upstream.HTMLType + ";q=0.1", upstream.JSONType},
		{"*/*;q=1, " + upstream.JSONType + ";q=0", upstream.HTMLType},
		{upstream.JSONType + ";q=0", ""}, {"application/json", ""}, {"text/plain", ""},
	} {
		t.Run(test.accept, func(t *testing.T) {
			result := request(app, "GET", "/python/simple/", http.Header{"Accept": {test.accept}})
			if test.want == "" {
				if result.Code != 406 {
					t.Fatalf("unsupported Accept: %d", result.Code)
				}
			} else if result.Code != 200 || result.Header().Get("Content-Type") != test.want {
				t.Fatalf("negotiation: %d %s", result.Code, result.Header().Get("Content-Type"))
			}
		})
	}
	if f.totalCalls() != 0 {
		t.Fatal("an empty allowlist fetched the upstream catalog")
	}
}

func TestExplicitAllowAllExposesTheFilteredCatalog(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, func(cfg *config.Config) { cfg.Allow = []string{"*"} })
	result := request(app, "GET", "/python/simple/", http.Header{"Accept": {upstream.JSONType}})
	var listing projectList
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &listing) != nil ||
		!reflect.DeepEqual(listing.Projects, []projectName{{Name: "company-core"}, {Name: "friendly-bard"}, {Name: "outside-subset"}}) {
		t.Fatalf("explicit allow-all catalog: %d %s", result.Code, result.Body.String())
	}
	request(app, "GET", "/python/simple/outside-subset/", nil)
	if f.callCount(upstream.IndexURL+"outside-subset/") != 1 {
		t.Fatal("explicit allow-all did not permit a project outside the named rules")
	}
}

func TestCanonicalRoutesAndReadOnlyMethods(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	for _, path := range []string{"/python/simple/Friendly_Bard", "/python/simple/friendly.bard/"} {
		result := request(app, "GET", path, nil)
		if result.Code != 308 || result.Header().Get("Location") != app.publicURL("/simple/friendly-bard/") {
			t.Fatalf("canonical route: %s: %d %v", path, result.Code, result.Header())
		}
	}
	if result := request(app, "HEAD", "/python/simple", nil); result.Code != 308 || result.Body.Len() != 0 {
		t.Fatal("root slash redirect failed")
	}
	for _, path := range []string{
		"/simple/", "/python/unsupported/index.json", "/python/simple/../friendly-bard/",
		"/python/simple/friendly-bard//", "/python/simple/friendly-bard%2fother/",
		"/python/simple/_invalid/", "/python/packages/friendly-bard/forged/file.whl",
	} {
		if result := request(app, "GET", path, nil); result.Code != 404 {
			t.Errorf("%s: %d", path, result.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		result := request(app, method, "/python/simple/", nil)
		if result.Code != 405 || result.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d", method, result.Code)
		}
	}
	if f.totalCalls() != 0 {
		t.Fatal("invalid routes contacted upstream")
	}
}

func TestExcludedAndMissingProjects(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	excluded := request(app, "GET", "/python/simple/outside-subset/", nil)
	if excluded.Code != 404 || f.totalCalls() != 0 {
		t.Fatal("excluded project was fetched upstream")
	}
	head := request(app, "HEAD", "/python/simple/outside-subset/", nil)
	if head.Code != 404 || head.Body.Len() != 0 || f.totalCalls() != 0 {
		t.Fatal("excluded HEAD request was not hidden")
	}
	missing := request(app, "GET", "/python/simple/company-missing/", nil)
	if missing.Code != 404 || missing.Body.String() != excluded.Body.String() {
		t.Fatal("missing and excluded projects have different absence responses")
	}
}

func TestArtifactCachingAndHTTPContract(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	document, _ := projectResponse(t, app)
	path := document.Files[0].URL
	want := f.replies[artifactURL].body
	get := request(app, "GET", path, http.Header{"Authorization": {"fixture-token"}})
	if get.Code != 200 || !bytes.Equal(get.Body.Bytes(), want) {
		t.Fatalf("wheel bytes changed: %d %q", get.Code, get.Body.Bytes())
	}
	head := request(app, "HEAD", path, nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatal("download HEAD contract failed")
	}
	conditional := request(app, "GET", path, http.Header{"If-None-Match": {`"wheel-v1"`}})
	if conditional.Code != 304 || conditional.Body.Len() != 0 {
		t.Fatal("conditional download failed")
	}
	partial := request(app, "GET", path, http.Header{"Range": {"bytes=1-3"}})
	if partial.Code != 206 || !bytes.Equal(partial.Body.Bytes(), want[1:4]) {
		t.Fatal("range download failed")
	}
	if f.totalCalls() != 2 || f.authorization {
		t.Fatal("cached requests fetched upstream again or forwarded credentials")
	}
	sidecar := request(app, "GET", path+".metadata", nil)
	if sidecar.Code != 200 || !bytes.Equal(sidecar.Body.Bytes(), f.replies[artifactURL+".metadata"].body) {
		t.Fatal("core metadata sidecar changed or was not proxied")
	}
}

func TestFileLinksSurviveRestartButNotPolicyRevocation(t *testing.T) {
	f := newFixture(t)
	directory := t.TempDir()
	app := testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory })
	document, _ := projectResponse(t, app)
	path := document.Files[0].URL
	if result := request(app, "GET", path, nil); result.Code != 200 {
		t.Fatal("could not warm cache")
	}
	restarted := testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory })
	calls := f.totalCalls()
	if result := request(restarted, "GET", path, nil); result.Code != 200 || f.totalCalls() != calls {
		t.Fatal("issued link did not survive a warm-cache restart")
	}
	for _, forged := range []string{
		strings.Replace(path, "/packages/friendly-bard/", "/packages/company-core/", 1),
		strings.TrimSuffix(path, document.Files[0].Filename) + "other.whl",
		strings.Replace(path, "/Friendly_Bard", "tampered/Friendly_Bard", 1),
	} {
		if result := request(restarted, "GET", forged, nil); result.Code != 404 {
			t.Fatalf("modified file link was accepted: %d", result.Code)
		}
	}
	revoked := testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory; cfg.Allow = nil })
	for _, old := range []string{path, path + ".metadata", "/python/simple/friendly-bard/"} {
		if result := request(revoked, "GET", old, nil); result.Code != 404 {
			t.Fatalf("revoked project bypassed current policy: %d", result.Code)
		}
	}
	if f.totalCalls() != calls {
		t.Fatal("forged or revoked links fetched upstream")
	}
}

func TestInvalidUpstreamMetadata(t *testing.T) {
	for _, name := range []string{
		"invalid JSON", "wrong media type", "unsupported API", "wrong project", "missing files",
		"missing versions", "missing hashes", "missing size", "invalid digest", "invalid metadata",
		"invalid yanked", "untrusted URL", "mismatched filename",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			document := fixtureProject()
			switch name {
			case "unsupported API":
				document.Meta.Version = "2.0"
			case "wrong project":
				document.Name = "outside-subset"
			case "missing files":
				document.Files = nil
			case "missing versions":
				document.Versions = nil
			case "missing hashes":
				document.Files[0].Hashes = nil
			case "missing size":
				document.Files[0].Size = nil
			case "invalid digest":
				document.Files[0].Hashes["sha256"] = "not-hex"
			case "invalid metadata":
				document.Files[0].CoreMetadata = "invalid"
			case "invalid yanked":
				document.Files[0].Yanked = 42
			case "untrusted URL":
				document.Files[0].URL = "https://example.com/file.whl"
			case "mismatched filename":
				document.Files[0].Filename = "different.whl"
			}
			f.setJSON(projectURL, document)
			reply := f.replies[projectURL]
			if name == "invalid JSON" {
				reply.body = []byte("{")
			} else if name == "wrong media type" {
				reply.header.Set("Content-Type", "application/json")
			}
			f.replies[projectURL] = reply
			app := testServer(t, f, nil)
			if result := request(app, "GET", "/python/simple/friendly-bard/", nil); result.Code != 502 {
				t.Fatalf("invalid metadata returned %d: %s", result.Code, result.Body.String())
			}
		})
	}
}

func TestLargeCatalogIsFiltered(t *testing.T) {
	f := newFixture(t)
	listing := projectList{Meta: apiMeta{Version: "1.4"}}
	for i := range 20000 {
		listing.Projects = append(listing.Projects, projectName{Name: "outside-" + strings.Repeat("x", 64) + strconv.Itoa(i)})
	}
	listing.Projects = append(listing.Projects, projectName{Name: "Friendly_Bard"})
	f.setJSON(upstream.IndexURL, listing)
	app := testServer(t, f, nil)
	result := request(app, "GET", "/python/simple/", http.Header{"Accept": {upstream.JSONType}})
	var document projectList
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &document) != nil ||
		!reflect.DeepEqual(document.Projects, []projectName{{Name: "friendly-bard"}}) {
		t.Fatalf("large catalog failed: %d %s", result.Code, result.Body.String())
	}
}

func TestErrorsAndHealthProbes(t *testing.T) {
	for _, test := range []struct {
		reply fixtureReply
		want  int
	}{
		{fixtureReply{status: 500}, 502}, {fixtureReply{status: 403}, 502},
		{fixtureReply{status: 429, header: http.Header{"Retry-After": {"30"}}}, 429},
		{fixtureReply{status: 503}, 503}, {fixtureReply{err: context.DeadlineExceeded}, 504},
	} {
		f := newFixture(t)
		f.replies[projectURL] = test.reply
		app := testServer(t, f, nil)
		result := request(app, "GET", "/python/simple/friendly-bard/", nil)
		if result.Code != test.want || (test.want == 429 && result.Header().Get("Retry-After") != "30") {
			t.Fatalf("error contract: %d %v", result.Code, result.Header())
		}
	}
	f := newFixture(t)
	app := testServer(t, f, nil)
	if result := request(app, "GET", "/healthz", nil); result.Code != 200 || f.totalCalls() != 0 {
		t.Fatal("liveness depends on upstream")
	}
	if result := request(app, "HEAD", "/readyz", nil); result.Code != 200 || result.Body.Len() != 0 || f.totalCalls() != 0 || f.heads != 1 {
		t.Fatal("readiness did not use an inexpensive HEAD request")
	}
	f.replies[upstream.IndexURL] = fixtureReply{status: 500}
	if result := request(app, "GET", "/readyz", nil); result.Code != 503 {
		t.Fatal("broken upstream reported ready")
	}
}
