package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// catalogReloadReadCap bounds the CDN response body read so a runaway or
// misconfigured URL can't buffer unbounded memory. plants_index.json is ~340 KB
// today; 8 MiB is generous headroom and matches the API's other body caps.
const catalogReloadReadCap = 8 << 20

// catalogReloadHTTPTimeout caps a single conditional GET (connect + download of
// the ~340 KB index). Comfortably above a healthy fetch; a stuck CDN fails the
// poll (fail-safe: last good table kept) rather than wedging the goroutine.
const catalogReloadHTTPTimeout = 15 * time.Second

// CatalogReloader polls the CDN plants_index.json and hot-swaps the name→id
// table on a ContentIndex whenever it changes (SPEC §9). Zero-downtime: the swap
// is a single atomic pointer store (ContentIndex.ReloadCatalogNames) and never
// blocks a request. Fail-safe: any poll/parse error keeps the last good (or
// embed) table. Enabled from main only when CATALOG_HOTLOAD_ENABLED is set.
type CatalogReloader struct {
	content  *ContentIndex
	url      string
	interval time.Duration
	client   *http.Client
	// etag is the last-seen ETag, used as the conditional-GET version gate
	// (If-None-Match). Read and written ONLY by the single poll goroutine, so
	// it needs no synchronization.
	etag string
}

// NewCatalogReloader builds a reloader for the given CDN index URL + poll
// cadence. It does not start polling — call Start.
func NewCatalogReloader(content *ContentIndex, url string, interval time.Duration) *CatalogReloader {
	return &CatalogReloader{
		content:  content,
		url:      url,
		interval: interval,
		client:   &http.Client{Timeout: catalogReloadHTTPTimeout},
	}
}

// Start launches the background poll loop and returns immediately. It polls once
// right away (so a fresh catalog is picked up shortly after boot) then every
// interval until ctx is cancelled. The loop runs off the request path and never
// blocks serving. A nil / incompletely-configured reloader is a no-op.
func (r *CatalogReloader) Start(ctx context.Context) {
	if r == nil || r.content == nil || r.url == "" || r.interval <= 0 {
		return
	}
	go func() {
		// Immediate first poll — async, does not block startup or serving. The
		// embed baseline already serves until (and if) this succeeds.
		if _, err := r.pollOnce(ctx); err != nil {
			log.Printf("WARN catalog hot-load: initial poll failed: %v (serving embed baseline)", err)
		}
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := r.pollOnce(ctx); err != nil {
					log.Printf("WARN catalog hot-load: poll failed: %v (kept last good table)", err)
				}
			}
		}
	}()
}

// pollOnce performs one conditional GET and, on a changed 200 response, rebuilds
// and atomically swaps the name table. Returns (changed, err). A 304 Not
// Modified returns (false, nil) with no work. Any error leaves the current table
// untouched (SPEC §9.5 fail-safe) — the caller logs and the next tick retries.
func (r *CatalogReloader) pollOnce(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return false, err
	}
	if r.etag != "" {
		req.Header.Set("If-None-Match", r.etag)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified: // 304 — unchanged; the whole point of the ETag gate
		return false, nil
	case http.StatusOK:
		// new body — fall through to read + swap
	default:
		// Drain a little so the connection can be reused, then report.
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		return false, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, r.url)
	}

	// LimitReader at cap+1 so an over-cap body is detectable (not silently
	// truncated into a parse that might pass the guards on garbage).
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogReloadReadCap+1))
	if err != nil {
		return false, err
	}
	if len(body) > catalogReloadReadCap {
		return false, fmt.Errorf("body exceeds %d-byte read cap", catalogReloadReadCap)
	}

	etag := resp.Header.Get("ETag")
	n, err := r.content.ReloadCatalogNames(body, etag)
	if err != nil {
		// Parse error / truncation guard / empty payload — keep the current
		// table and do NOT record the ETag, so the next tick re-fetches and
		// re-attempts instead of 304-skipping a bad object.
		return false, err
	}
	r.etag = etag
	log.Printf("catalog hot-load: reloaded rows=%d etag=%q from %s", n, etag, r.url)
	return true, nil
}
