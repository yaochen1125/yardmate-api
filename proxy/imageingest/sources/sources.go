// Package sources implements the V2 on-demand image cascade sources (SPEC §2.4):
// iNaturalist (primary) + Wikimedia Commons (fallback). Each client fetches
// candidate images plus their RAW (unclassified) license signals.
//
// License CLASSIFICATION deliberately lives in the parent imageingest package
// (license.go ClassifyLicenseCode, token-membership — SPEC §2.4.3), NOT here.
// That keeps the dependency edge one-way (imageingest → sources) and avoids an
// import cycle: the ingestor classifies the raw Candidate fields this package
// returns. A source therefore never decides "allowed / gated / rejected"; it
// only reports what the upstream API said.
package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Source identifiers — mirror the imageingest ledger `source` column values
// (SPEC §6.1) so the ingestor can persist Candidate.Source directly.
const (
	SourceINaturalist      = "inaturalist"
	SourceWikimediaCommons = "wikimedia_commons"
)

// Typed errors. They collapse into IngestOutcome.Status at the ingestor layer
// (SPEC §3); they are never surfaced as HTTP errors to the iOS client.
var (
	// ErrUnavailable — network failure / 5xx / retries exhausted.
	ErrUnavailable = errors.New("imageingest/sources: unavailable")
	// ErrBadResponse — 200 with an unparseable body.
	ErrBadResponse = errors.New("imageingest/sources: bad response")
	// ErrImageTooLarge — downloaded bytes exceeded the size cap.
	ErrImageTooLarge = errors.New("imageingest/sources: image too large")
	// ErrUnsupportedMIME — sniffed MIME not in {jpeg,png,webp}.
	ErrUnsupportedMIME = errors.New("imageingest/sources: unsupported mime")
)

const (
	// DefaultUserAgent is the mandatory descriptive UA for outbound calls (SPEC
	// §4.1). A missing UA yields 403 (Wikimedia) or rate-clamp (iNat), so every
	// client falls back to this when its own UA is unset.
	DefaultUserAgent  = "YardMate-ImageIngest/2.0 (https://yardmate.ai; contact@yardmate.ai)"
	defaultMaxBytes   = 25 << 20 // 25 MiB download cap (SPEC §4.1)
	defaultMaxRetries = 3        // bounded Retry-After backoff (SPEC §4.1)
	defaultRetryGap   = 30 * time.Second
)

// allowedDownloadMIME are the only Content-Types stored to R2 (SPEC §2.6/§4.1).
var allowedDownloadMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// Candidate is one image candidate from a source, carrying RAW (unclassified)
// license signals. The parent imageingest package classifies these via
// ClassifyLicenseCode (SPEC §2.4.3) before ranking (SPEC §2.5).
type Candidate struct {
	Source      string // SourceINaturalist | SourceWikimediaCommons
	Title       string // human label / Commons "File:..." title (heuristic signal)
	PageURL     string // provenance: iNat photo page / Commons File: page → ledger source_url
	OriginalURL string // full-res original URL (heuristic size signal only)
	DownloadURL string // rendition we download + store verbatim (iNat large_url / Commons 1600px thumb)
	MIME        string // advisory mime (re-sniffed on download)
	Width       int    // pixel width (heuristic / 0 when unknown)
	Height      int    // pixel height (heuristic / 0 when unknown)
	DedupKey    string // within-gallery dedup key: iNat photo id / Commons File: page (SPEC §2.5 #4)

	// Raw license signals — the PARENT package classifies (SPEC §2.4.3):
	LicenseCode      string // machine code: iNat "cc-by" / Commons "cc-by-sa-4.0" / "cc0" / ...
	LicenseShortName string // Commons LicenseShortName (empty for iNat — derived from code)
	LicenseURL       string // Commons LicenseUrl (empty for iNat — derived from code)
	Author           string // raw attribution (parent strips HTML for Commons; iNat is plain text)
}

// fetcher is the shared outbound-HTTP base for source clients: mandatory UA,
// bounded Retry-After backoff (SPEC §4.1), and a size-capped + MIME-sniffed
// download. The rendition host differs per source but the download discipline
// is identical, so all clients embed this.
type fetcher struct {
	httpClient *http.Client
	userAgent  string
	maxBytes   int64
}

func newFetcher(client *http.Client, userAgent string, maxBytes int64) fetcher {
	f := fetcher{httpClient: client, userAgent: userAgent, maxBytes: maxBytes}
	if f.httpClient == nil {
		f.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if f.userAgent == "" {
		f.userAgent = DefaultUserAgent
	}
	if f.maxBytes <= 0 {
		f.maxBytes = defaultMaxBytes
	}
	return f
}

// Download fetches the bytes at rawURL (a source-returned rendition URL), capped
// at maxBytes via io.LimitReader, then re-sniffs the MIME on the first 512 bytes
// (the source-declared mime is advisory — SPEC §4.1). Returns bytes + sniffed
// MIME. The URL always originates from a source API response, never from a
// client (no SSRF surface — SPEC §5).
func (f *fetcher) Download(ctx context.Context, rawURL string) ([]byte, string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, "", fmt.Errorf("%w: empty url", ErrBadResponse)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: build req: %v", ErrUnavailable, err)
	}
	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: download: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%w: download status %d", ErrUnavailable, resp.StatusCode)
	}

	// Read one extra byte past the cap so we can detect "exceeded" cleanly.
	limited := io.LimitReader(resp.Body, f.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("%w: read body: %v", ErrUnavailable, err)
	}
	if int64(len(data)) > f.maxBytes {
		return nil, "", ErrImageTooLarge
	}

	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	mime := http.DetectContentType(head)
	if !allowedDownloadMIME[mime] {
		return nil, "", fmt.Errorf("%w: %s", ErrUnsupportedMIME, mime)
	}
	return data, mime, nil
}

// getWithRetry GETs reqURL honoring 429 / 503 Retry-After with bounded backoff
// (SPEC §4.1). Sets the mandatory User-Agent. Shared by all source searches.
func (f *fetcher) getWithRetry(ctx context.Context, reqURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= defaultMaxRetries; attempt++ {
		body, retryAfter, status, err := f.doGet(ctx, reqURL)
		switch {
		case err != nil:
			lastErr = err // transient network error — retry
		case status == http.StatusOK:
			return body, nil
		case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
			lastErr = fmt.Errorf("%w: status %d", ErrUnavailable, status)
		default:
			// Other non-200 (4xx other than 429) → not retryable.
			return nil, fmt.Errorf("%w: status %d", ErrUnavailable, status)
		}
		if attempt == defaultMaxRetries {
			break
		}
		// Sleep EXACTLY ONCE before the next attempt: honor a server Retry-After
		// when present (bounded), else a computed backoff — never both.
		wait := retryAfter
		if wait <= 0 {
			wait = backoffFor(attempt + 1)
		}
		if wait > defaultRetryGap {
			wait = defaultRetryGap
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	if lastErr == nil {
		lastErr = ErrUnavailable
	}
	return nil, lastErr
}

// doGet performs a single GET, returning the body, parsed Retry-After, the
// status code, and a transport error (nil on any HTTP response).
func (f *fetcher) doGet(ctx context.Context, reqURL string) ([]byte, time.Duration, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: build req: %v", ErrUnavailable, err)
	}
	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: do: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	// Cap the JSON body too — defensive against an unexpectedly huge response.
	limited := io.LimitReader(resp.Body, f.maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, retryAfter, resp.StatusCode, fmt.Errorf("%w: read: %v", ErrUnavailable, err)
	}
	return body, retryAfter, resp.StatusCode, nil
}

// parseRetryAfter parses a Retry-After header (delta-seconds or HTTP-date).
// Returns 0 when absent / unparseable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoffFor returns the wait before retry attempt n (1-based), capped.
func backoffFor(attempt int) time.Duration {
	wait := time.Duration(attempt) * time.Second
	if wait > defaultRetryGap {
		wait = defaultRetryGap
	}
	return wait
}
