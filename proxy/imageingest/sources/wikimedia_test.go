package sources

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// wmSearchHitJSON is a minimal api.php generator=search response with one File:
// page carrying imageinfo + a CC-BY-SA extmetadata block.
const wmSearchHitJSON = `{
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

const wmNoResultsJSON = `{"batchcomplete":"","limits":{"search":10}}`

func newWMTestClient(server *httptest.Server, ua string) *WikimediaClient {
	return NewWikimediaClient(WikimediaOptions{
		APIBase:    server.URL + "/w/api.php",
		HTTPClient: server.Client(),
		UserAgent:  ua,
	})
}

func TestWikimediaSearch_Hit(t *testing.T) {
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
		_, _ = w.Write([]byte(wmSearchHitJSON))
	}))
	defer srv.Close()

	c := newWMTestClient(srv, "test-ua")
	cands, err := c.Search(context.Background(), "Rosa regina", 10)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(cands))
	}
	got := cands[0]
	if got.Source != SourceWikimediaCommons {
		t.Errorf("Source = %q", got.Source)
	}
	if got.DownloadURL != "https://upload.wikimedia.org/thumb/1600px-full.jpg" {
		t.Errorf("DownloadURL = %q", got.DownloadURL)
	}
	if got.PageURL == "" || got.DedupKey != got.PageURL {
		t.Errorf("PageURL/DedupKey = %q / %q", got.PageURL, got.DedupKey)
	}
	if got.Width != 4000 || got.Height != 3000 {
		t.Errorf("dims = %dx%d", got.Width, got.Height)
	}
	// Sources return RAW license signals — NOT classified, NOT HTML-stripped.
	if got.LicenseCode != "cc-by-sa-4.0" {
		t.Errorf("LicenseCode = %q, want raw cc-by-sa-4.0", got.LicenseCode)
	}
	if got.LicenseShortName != "CC BY-SA 4.0" {
		t.Errorf("LicenseShortName = %q", got.LicenseShortName)
	}
	if got.Author != `<a href="x">Jane Doe</a>` {
		t.Errorf("Author = %q, want raw HTML (parent strips)", got.Author)
	}
}

func TestWikimediaSearch_NoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(wmNoResultsJSON))
	}))
	defer srv.Close()

	c := newWMTestClient(srv, "test-ua")
	cands, err := c.Search(context.Background(), "Nonexistent plantus", 10)
	if err != nil {
		t.Fatalf("Search err: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(cands))
	}
}

func TestWikimediaSearch_MissingUA_403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(wmSearchHitJSON))
	}))
	defer srv.Close()

	// Force an empty UA by constructing the client directly (bypass the default).
	c := &WikimediaClient{
		fetcher: fetcher{httpClient: srv.Client(), userAgent: "", maxBytes: defaultMaxBytes},
		apiBase: srv.URL + "/w/api.php",
	}
	_, err := c.Search(context.Background(), "Rosa", 10)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable on 403, got %v", err)
	}
}

func TestWikimediaSearch_429ThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0") // 0s so the test stays fast
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(wmSearchHitJSON))
	}))
	defer srv.Close()

	c := newWMTestClient(srv, "test-ua")
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
