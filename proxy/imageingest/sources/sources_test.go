package sources

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// pngBytes returns a tiny valid PNG header so http.DetectContentType returns
// image/png.
func pngBytes() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0, 0, 0}
}

func TestDownload_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(pngBytes())
	}))
	defer srv.Close()

	f := newFetcher(srv.Client(), "test-ua", 0)
	data, mime, err := f.Download(context.Background(), srv.URL+"/large.png")
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

func TestDownload_UnsupportedMIME(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	}))
	defer srv.Close()

	f := newFetcher(srv.Client(), "test-ua", 0)
	_, _, err := f.Download(context.Background(), srv.URL+"/x.svg")
	if !errors.Is(err, ErrUnsupportedMIME) {
		t.Fatalf("expected ErrUnsupportedMIME, got %v", err)
	}
}

func TestDownload_TooLarge(t *testing.T) {
	big := make([]byte, 2048)
	copy(big, pngBytes())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	f := newFetcher(srv.Client(), "test-ua", 1024) // cap < body
	_, _, err := f.Download(context.Background(), srv.URL+"/large.png")
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %v", err)
	}
}

func TestDownload_EmptyURL(t *testing.T) {
	f := newFetcher(nil, "test-ua", 0)
	_, _, err := f.Download(context.Background(), "  ")
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("expected ErrBadResponse, got %v", err)
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
	if d := parseRetryAfter("-3"); d != 0 {
		t.Errorf("negative = %v", d)
	}
}
