package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestValidateURL(t *testing.T) {
	for _, rawURL := range []string{
		IndexURL, "https://pypi.org/simple/requests/", "https://PYPI.ORG:443/simple/requests/",
		"https://files.pythonhosted.org/packages/file.whl",
	} {
		if err := ValidateURL(rawURL); err != nil {
			t.Errorf("rejected %q: %v", rawURL, err)
		}
	}
	credentialURL, _ := url.Parse(IndexURL)
	credentialURL.User = url.User("fixture")
	for _, rawURL := range []string{
		"http://pypi.org/simple/", "https://pypi.org.evil.example/simple/",
		"https://evilpypi.org/simple/", "https://127.0.0.1/simple/", "https://pypi.org:444/simple/",
		credentialURL.String(), "https://pypi.org/simple/#fragment", "https://pypi.org/simple/a\\b",
		"/relative", "https://", "https://pypi.org/admin/", "https://files.pythonhosted.org/private/",
		"https://other.pythonhosted.org/packages/file.whl",
		"https://files.pythonhosted.org/packages/a/../file.whl",
	} {
		if err := ValidateURL(rawURL); err == nil {
			t.Errorf("accepted %q", rawURL)
		}
	}
}

func TestConcurrencyIncludesResponseBody(t *testing.T) {
	var calls atomic.Int32
	client := New(time.Second, 2, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Header.Get("If-None-Match") != `"etag"` || request.Header.Get("If-Modified-Since") != "date" ||
			request.Header.Get("Accept") != JSONType || request.Header.Get("User-Agent") != "cheeseshop" {
			t.Error("missing revalidation or Simple API headers")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data"))}, nil
	}))
	first, err := client.Get(context.Background(), IndexURL, `"etag"`, "date")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	second, err := client.Get(context.Background(), IndexURL, `"etag"`, "date")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := client.Get(ctx, IndexURL, "", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected concurrency wait timeout, got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("transport called %d times while all slots were occupied", calls.Load())
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := client.Get(context.Background(), IndexURL, `"etag"`, "date")
	if err != nil {
		t.Fatal(err)
	}
	third.Body.Close()
}

func TestRedirectCannotChangeResourceHost(t *testing.T) {
	for _, target := range []string{"https://example.com/package", "https://files.pythonhosted.org/packages/file.whl"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			client := New(time.Second, 1, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{
					StatusCode: http.StatusFound, Header: http.Header{"Location": {target}},
					Body: http.NoBody, Request: request,
				}, nil
			}))
			if _, err := client.Get(context.Background(), IndexURL, "", ""); err == nil {
				t.Fatal("redirect outside the PyPI index host was accepted")
			}
			if calls.Load() != 1 {
				t.Fatalf("redirect target was contacted: %d requests", calls.Load())
			}
		})
	}
}
