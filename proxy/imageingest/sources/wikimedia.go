package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const commonsAPIBase = "https://commons.wikimedia.org/w/api.php"

// WikimediaClient queries Wikimedia Commons (SPEC §2.4.2) — the cascade FALLBACK
// source, reached only when iNaturalist returned < N eligible candidates. It
// returns RAW license signals (LicenseCode / LicenseShortName / LicenseURL /
// Author from extmetadata); the parent imageingest package classifies them.
type WikimediaClient struct {
	fetcher
	apiBase string
}

// WikimediaOptions configures a WikimediaClient. Zero values fall back to
// defaults (UA is mandatory for live Commons — a missing UA yields 403, SPEC
// §4.1 / §9 #9).
type WikimediaOptions struct {
	APIBase    string       // default https://commons.wikimedia.org/w/api.php
	HTTPClient *http.Client // default a 30s-timeout client
	UserAgent  string       // default DefaultUserAgent
	MaxBytes   int64        // default 25 MiB
}

// NewWikimediaClient builds a Commons client.
func NewWikimediaClient(opts WikimediaOptions) *WikimediaClient {
	c := &WikimediaClient{
		fetcher: newFetcher(opts.HTTPClient, opts.UserAgent, opts.MaxBytes),
		apiBase: opts.APIBase,
	}
	if c.apiBase == "" {
		c.apiBase = commonsAPIBase
	}
	return c
}

// --- api.php response shapes (subset) ---

type commonsQueryResp struct {
	Query *struct {
		Pages map[string]commonsPage `json:"pages"`
	} `json:"query"`
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
	URL            string              `json:"url"`
	DescriptionURL string              `json:"descriptionurl"`
	ThumbURL       string              `json:"thumburl"`
	ThumbMIME      string              `json:"thumbmime"`
	MIME           string              `json:"mime"`
	Width          int                 `json:"width"`
	Height         int                 `json:"height"`
	ExtMetadata    map[string]extField `json:"extmetadata"`
}

// extField is one extmetadata entry: Commons wraps each as {"value": ...}. The
// `value` is USUALLY a string, but Commons returns a BARE JSON NUMBER for some
// fields (e.g. an integer for certain files) — a fixed `string` type then makes
// json.Unmarshal fail and abort the ENTIRE search (observed live on "Helianthus
// annuus"). UnmarshalJSON coerces string / number / bool all into the string
// Value so one numeric field never sinks the whole response.
type extField struct {
	Value string
}

// UnmarshalJSON accepts {"value": <string|number|bool>, ...} and stores value as
// a string. A string is taken verbatim; any other JSON scalar is kept as its raw
// token (quotes trimmed). Other keys (e.g. "source") are ignored.
func (e *extField) UnmarshalJSON(b []byte) error {
	var raw struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw.Value) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw.Value, &s); err == nil {
		e.Value = s
		return nil
	}
	// Non-string scalar (number / bool): keep the raw token, trimming any quotes.
	e.Value = strings.Trim(string(raw.Value), `"`)
	return nil
}

func (m commonsImageInfo) ext(key string) string {
	if m.ExtMetadata == nil {
		return ""
	}
	return m.ExtMetadata[key].Value
}

// Search runs one api.php generator=search query for the scientific name and
// returns RAW candidates (SPEC §2.4.2). Honors maxlag + Retry-After with bounded
// retries. License is NOT classified here — LicenseCode etc. are passed through
// for the parent to classify (SPEC §2.4.3). Returns (nil, nil) on a valid empty
// search.
func (c *WikimediaClient) Search(ctx context.Context, scientificName string, limit int) ([]Candidate, error) {
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
	q.Set("iiurlwidth", "1600") // render a 1600px thumburl (SPEC §7 D-format)

	reqURL := c.apiBase + "?" + q.Encode()

	body, err := c.getWithRetry(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp commonsQueryResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrBadResponse, err)
	}
	if resp.Error != nil {
		// maxlag / other api errors can arrive as HTTP 200 + an error block;
		// treat as transient-unavailable.
		return nil, fmt.Errorf("%w: api error %s", ErrUnavailable, resp.Error.Code)
	}
	if resp.Query == nil {
		return nil, nil // no "query" block → valid empty search
	}

	out := make([]Candidate, 0, len(resp.Query.Pages))
	for _, page := range resp.Query.Pages {
		if len(page.ImageInfo) == 0 {
			continue
		}
		ii := page.ImageInfo[0]
		out = append(out, Candidate{
			Source:      SourceWikimediaCommons,
			Title:       page.Title,
			PageURL:     ii.DescriptionURL,
			OriginalURL: ii.URL,
			DownloadURL: ii.ThumbURL,
			MIME:        ii.MIME,
			Width:       ii.Width,
			Height:      ii.Height,
			DedupKey:    ii.DescriptionURL, // Commons File: page identifies the file (SPEC §2.5 #4)

			LicenseCode:      ii.ext("License"),
			LicenseShortName: ii.ext("LicenseShortName"),
			LicenseURL:       ii.ext("LicenseUrl"),
			Author:           ii.ext("Artist"), // raw HTML; parent strips (SPEC §2.4.3)
		})
	}
	return out, nil
}
