package imageingest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// searchHitJSON is a minimal api.php generator=search response with one File:
// page carrying imageinfo + a CC-BY-SA extmetadata block.
const searchHitJSON = `{
  "query": {
    "pages": {
      "12345": {
        "title": "File:Rosa regina example.jpg",
        "imageinfo": [{
          "url": "https://upload.wikimedia.org/full.jpg",
          "descriptionurl": "https://commons.wikimedia.org/wiki/File:Rosa_regina_example.jpg",
          "thumburl": "https://upload.wikimedia.org/thumb/1600px-full.jpg",
          "thumbmime": "image/jpeg",
          "mime": "image/jpeg",
          "width": 4000,
          "height": 3000,
          "extmetadata": {
            "License": {"value": "cc-by-sa-4.0"},
            "LicenseShortName": {"value": "CC BY-SA 4.0"},
            "LicenseUrl": {"value": "https://creativecommons.org/licenses/by-sa/4.0/"},
            "Artist": {"value": "<a href=\"x\">Jane Doe</a>"},
            "Copyrighted": {"value": "True"}
          }
        }]
      }
    }
  }
}`

const noResultsJSON = `{"batchcomplete":"","limits":{"search":10}}`

func newCommonsTestClient(server *httptest.Server, ua string) *CommonsClient {
	return NewCommonsClient(CommonsOptions{
		APIBase:    server.URL + "/w/api.php",
		HTTPClient: server.Client(),
		UserAgent:  ua,
	})
}

func TestCommonsSearch_Hit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("iiurlwidth") != "1600" {
			t.Errorf("expected iiurlwidth=1600, got %q", r.URL.Query().Get("iiurlwidth"))
		}
		if r.URL.Query().Get("maxlag") != "5" {
			t.Errorf("expected maxlag=5, got %q", r.URL.Query().Get("maxlag"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(searchHitJSON))
	}))
	defer srv.Close()

	c := newCommonsTestClient(srv, "test-ua")
	cands, err := c.Search(context.Background(), "Rosa regina", 10)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	got := cands[0]
	if got.ThumbURL != "https://upload.wikimedia.org/thumb/1600px-full.jpg" {
		t.Errorf("ThumbURL = %q", got.ThumbURL)
	}
	if got.PageURL == "" {
		t.Errorf("expected PageURL set")
	}
	if got.Width != 4000 || got.Height != 3000 {
		t.Errorf("dims = %dx%d", got.Width, got.Height)
	}
	if !got.License.Allowed || got.License.Family != FamilyCCBYSA {
		t.Errorf("license = %+v, want allowed CC_BY_SA", got.License)
	}
	if got.License.Author != "Jane Doe" {
		t.Errorf("author = %q, want Jane Doe", got.License.Author)
	}
}

func TestCommonsSearch_NoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(noResultsJSON))
	}))
	defer srv.Close()

	c := newCommonsTestClient(srv, "test-ua")
	cands, err := c.Search(context.Background(), "Nonexistent plantus", 10)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(cands))
	}
}

func TestCommonsSearch_MissingUA_403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate Wikimedia's UA enforcement: empty UA → 403.
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(searchHitJSON))
	}))
	defer srv.Close()

	// Force an empty UA by constructing the client directly (bypass the default).
	c := &CommonsClient{
		apiBase:    srv.URL + "/w/api.php",
		httpClient: srv.Client(),
		userAgent:  "",
		maxBytes:   defaultMaxBytes,
	}
	_, err := c.Search(context.Background(), "Rosa", 10)
	if !errors.Is(err, ErrCommonsUnavailable) {
		t.Fatalf("expected ErrCommonsUnavailable on 403, got %v", err)
	}
}

func TestCommonsSearch_429ThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0") // 0s so the test stays fast
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(searchHitJSON))
	}))
	defer srv.Close()

	c := newCommonsTestClient(srv, "test-ua")
	cands, err := c.Search(context.Background(), "Rosa", 10)
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Errorf("expected a retry (>=2 calls), got %d", calls)
	}
}

// pngBytes returns a tiny valid PNG header so http.DetectContentType returns
// image/png.
func pngBytes() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0, 0, 0}
}

func TestCommonsDownload_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(pngBytes())
	}))
	defer srv.Close()

	c := newCommonsTestClient(srv, "test-ua")
	data, mime, err := c.Download(context.Background(), srv.URL+"/thumb.png")
	if err != nil {
		t.Fatalf("Download err: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
	if len(data) == 0 {
		t.Errorf("expected bytes")
	}
}

func TestCommonsDownload_UnsupportedMIME(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"))
	}))
	defer srv.Close()

	c := newCommonsTestClient(srv, "test-ua")
	_, _, err := c.Download(context.Background(), srv.URL+"/thumb.svg")
	if !errors.Is(err, ErrUnsupportedMIME) {
		t.Fatalf("expected ErrUnsupportedMIME, got %v", err)
	}
}

func TestCommonsDownload_TooLarge(t *testing.T) {
	big := make([]byte, 2048)
	copy(big, pngBytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	c := NewCommonsClient(CommonsOptions{
		APIBase:    srv.URL + "/w/api.php",
		HTTPClient: srv.Client(),
		UserAgent:  "test-ua",
		MaxBytes:   1024, // smaller than the 2048-byte body
	})
	_, _, err := c.Download(context.Background(), srv.URL+"/thumb.png")
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("5"); d != 5*time.Second {
		t.Errorf("seconds parse = %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Errorf("empty = %v", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Errorf("garbage = %v", d)
	}
}
