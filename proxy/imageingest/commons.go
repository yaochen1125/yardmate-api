package imageingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Typed errors from the Commons client. They collapse into IngestOutcome.Status
// at the ingestor layer (SPEC §3); they are never surfaced as HTTP errors.
var (
	// ErrCommonsUnavailable — network failure / 5xx / retries exhausted.
	ErrCommonsUnavailable = errors.New("imageingest/commons: unavailable")
	// ErrCommonsBadResponse — 200 with an unparseable body.
	ErrCommonsBadResponse = errors.New("imageingest/commons: bad response")
	// ErrImageTooLarge — downloaded bytes exceeded the size cap.
	ErrImageTooLarge = errors.New("imageingest/commons: image too large")
	// ErrUnsupportedMIME — sniffed MIME not in {jpeg,png,webp}.
	ErrUnsupportedMIME = errors.New("imageingest/commons: unsupported mime")
)

const (
	defaultUserAgent   = "YardMate-ImageIngest/1.0 (https://yardmate.ai; contact@yardmate.ai)"
	defaultMaxBytes    = 25 << 20 // 25 MiB download cap (SPEC §4)
	defaultMaxRetries  = 3        // bounded Retry-After backoff (SPEC §4)
	defaultMaxRetryGap = 30 * time.Second
	commonsAPIBase     = "https://commons.wikimedia.org/w/api.php"
)

// allowedDownloadMIME are the only Content-Types stored to R2 (SPEC §2.4/§4).
var allowedDownloadMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// Candidate is one Commons File: search hit with its imageinfo + classified
// license. ThumbURL is the 1600px rendition we DOWNLOAD + store (SPEC §2.4 /
// §7 D2); URL (the original) is used ONLY for the size/photo heuristic.
type Candidate struct {
	Title    string  // "File:Example.jpg"
	PageURL  string  // Commons File: page (descriptionurl) — provenance / credits
	URL      string  // original full-res URL (heuristic signal only)
	ThumbURL string  // 1600px rendition (downloaded + stored verbatim)
	MIME     string  // advisory mime from extmetadata (re-sniffed on bytes)
	Width    int     // original pixel width (heuristic)
	Height   int     // original pixel height (heuristic)
	License  License // classified license (SPEC §2.4)
}

// CommonsClient queries Wikimedia Commons + downloads renditions. The base URL
// and *http.Client are injectable so tests use httptest (SPEC §10 hermetic).
type CommonsClient struct {
	apiBase    string
	httpClient *http.Client
	userAgent  string
	maxBytes   int64
}

// CommonsOptions configures a CommonsClient. Zero values fall back to defaults.
type CommonsOptions struct {
	APIBase    string       // default https://commons.wikimedia.org/w/api.php
	HTTPClient *http.Client // default a 30s-timeout client
	UserAgent  string       // default defaultUserAgent (MANDATORY in prod, SPEC §4)
	MaxBytes   int64        // default 25 MiB
}

// NewCommonsClient builds a client. UA is mandatory for live Wikimedia (a
// missing UA yields 403, not 429 — SPEC §9 #9); the default carries a contact.
func NewCommonsClient(opts CommonsOptions) *CommonsClient {
	c := &CommonsClient{
		apiBase:    opts.APIBase,
		httpClient: opts.HTTPClient,
		userAgent:  opts.UserAgent,
		maxBytes:   opts.MaxBytes,
	}
	if c.apiBase == "" {
		c.apiBase = commonsAPIBase
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.userAgent == "" {
		c.userAgent = defaultUserAgent
	}
	if c.maxBytes <= 0 {
		c.maxBytes = defaultMaxBytes
	}
	return c
}

// --- api.php response shapes (subset) ---

type commonsQueryResp struct {
	Query *struct {
		Pages map[string]commonsPage `json:"pages"`
	} `json:"query"`
	// Error / warning surfaces (e.g. maxlag) — we mostly rely on HTTP status,
	// but capture the error code for logging if present.
	Error *struct {
		Code string `json:"code"`
		Info string `json:"info"`
	} `json:"error"`
}

type commonsPage struct {
	Title     string             `json:"title"`
	ImageInfo []commonsImageInfo `json:"imageinfo"`
}

type commonsImageInfo struct {
	URL            string      `json:"url"`
	DescriptionURL string      `json:"descriptionurl"`
	ThumbURL       string      `json:"thumburl"`
	ThumbMIME      string      `json:"thumbmime"`
	MIME           string      `json:"mime"`
	Width          int         `json:"width"`
	Height         int         `json:"height"`
	ExtMetadata    extMetadata `json:"extmetadata"`
}

// Search runs one api.php generator=search query for the scientific name and
// returns classified candidates (SPEC §2.4). Honors maxlag + Retry-After with
// bounded retries. Candidates that fail license classification are returned
// with License.Allowed=false so the caller can distinguish "found but gated/
// rejected" from "no results" (needed for deferred_attribution, SPEC §3).
func (c *CommonsClient) Search(ctx context.Context, scientificName string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 10
	}
	q := url.Values{}
	q.Set("action", "query")
	q.Set("format", "json")
	q.Set("maxlag", "5")
	q.Set("generator", "search")
	q.Set("gsrsearch", scientificName)
	q.Set("gsrnamespace", "6") // File: namespace
	q.Set("gsrlimit", strconv.Itoa(limit))
	q.Set("prop", "imageinfo")
	q.Set("iiprop", "url|mime|size|extmetadata")
	q.Set("iiurlwidth", "1600") // render a 1600px thumburl (SPEC §7 D2)

	reqURL := c.apiBase + "?" + q.Encode()

	body, err := c.getWithRetry(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp commonsQueryResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrCommonsBadResponse, err)
	}
	if resp.Error != nil {
		// maxlag / other api errors arrive as HTTP 200 + an error block on some
		// configurations; treat as transient-unavailable.
		return nil, fmt.Errorf("%w: api error %s", ErrCommonsUnavailable, resp.Error.Code)
	}
	if resp.Query == nil {
		// No "query" block → no results (valid empty search).
		return nil, nil
	}

	out := make([]Candidate, 0, len(resp.Query.Pages))
	for _, page := range resp.Query.Pages {
		if len(page.ImageInfo) == 0 {
			continue
		}
		ii := page.ImageInfo[0]
		out = append(out, Candidate{
			Title:    page.Title,
			PageURL:  ii.DescriptionURL,
			URL:      ii.URL,
			ThumbURL: ii.ThumbURL,
			MIME:     ii.MIME,
			Width:    ii.Width,
			Height:   ii.Height,
			License:  ClassifyLicense(ii.ExtMetadata),
		})
	}
	return out, nil
}

// Download fetches the bytes at rawURL (the candidate ThumbURL), capped at
// maxBytes via io.LimitReader, then re-sniffs the MIME on the first 512 bytes
// (extmetadata mime is advisory — SPEC §4). Returns the bytes + sniffed MIME.
func (c *CommonsClient) Download(ctx context.Context, rawURL string) ([]byte, string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, "", fmt.Errorf("%w: empty url", ErrCommonsBadResponse)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: build req: %v", ErrCommonsUnavailable, err)
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: download: %v", ErrCommonsUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%w: download status %d", ErrCommonsUnavailable, resp.StatusCode)
	}

	// Read one extra byte past the cap so we can detect "exceeded" cleanly.
	limited := io.LimitReader(resp.Body, c.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", fmt.Errorf("%w: read body: %v", ErrCommonsUnavailable, err)
	}
	if int64(len(data)) > c.maxBytes {
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

// getWithRetry GETs reqURL honoring 429 / 503 Retry-After with bounded
// exponential-ish backoff (SPEC §4). Sets the mandatory User-Agent.
func (c *CommonsClient) getWithRetry(ctx context.Context, reqURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= defaultMaxRetries; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		body, retryAfter, status, err := c.doGet(ctx, reqURL)
		if err != nil {
			lastErr = err
			continue // transient network error — retry
		}
		if status == http.StatusOK {
			return body, nil
		}
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			// Honor Retry-After if present (bounded), else fall through to the
			// computed backoff on the next loop iteration.
			lastErr = fmt.Errorf("%w: status %d", ErrCommonsUnavailable, status)
			if retryAfter > 0 {
				wait := retryAfter
				if wait > defaultMaxRetryGap {
					wait = defaultMaxRetryGap
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
			}
			continue
		}
		// Other non-200 (4xx other than 429) → not retryable.
		return nil, fmt.Errorf("%w: status %d", ErrCommonsUnavailable, status)
	}
	if lastErr == nil {
		lastErr = ErrCommonsUnavailable
	}
	return nil, lastErr
}

// doGet performs a single GET, returning the body, parsed Retry-After (seconds
// or HTTP-date), the status code, and a transport error (nil on any HTTP
// response). Caller closes nothing — the body is fully read here.
func (c *CommonsClient) doGet(ctx context.Context, reqURL string) ([]byte, time.Duration, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: build req: %v", ErrCommonsUnavailable, err)
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: do: %v", ErrCommonsUnavailable, err)
	}
	defer resp.Body.Close()

	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	// Cap the api.php body too — defensive against an unexpectedly huge JSON.
	limited := io.LimitReader(resp.Body, c.maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, retryAfter, resp.StatusCode, fmt.Errorf("%w: read: %v", ErrCommonsUnavailable, err)
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
	if wait > defaultMaxRetryGap {
		wait = defaultMaxRetryGap
	}
	return wait
}
