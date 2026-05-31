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

const inatAPIBase = "https://api.inaturalist.org/v1"

// inatObservationLicenses filters supplementary observation photos at the API
// level (SPEC §2.4.1). CC-BY-SA is intentionally omitted here to match SPEC;
// SA images can still arrive via a taxon's default_photo (which is NOT
// license-filtered at the API and is classified by the parent). Widening this
// to include cc-by-sa is a future tweak if iNat-sourced SA galleries matter.
const inatObservationLicenses = "cc0,cc-by"

// INatClient queries iNaturalist (SPEC §2.4.1) — the cascade PRIMARY source.
// Returns RAW candidates (LicenseCode unversioned, e.g. "cc-by"; Author = the
// iNat attribution string); the parent imageingest package classifies them
// (SPEC §2.4.3). iNat photos are real wild specimens and the API is faster than
// Commons extmetadata HTML parsing.
type INatClient struct {
	fetcher
	apiBase string
}

// INatOptions configures an INatClient. Zero values fall back to defaults (UA
// is mandatory — a missing UA gets rate-clamped, SPEC §4.1).
type INatOptions struct {
	APIBase    string       // default https://api.inaturalist.org/v1
	HTTPClient *http.Client // default a 30s-timeout client
	UserAgent  string       // default DefaultUserAgent
	MaxBytes   int64        // default 25 MiB
}

// NewINatClient builds an iNaturalist client.
func NewINatClient(opts INatOptions) *INatClient {
	c := &INatClient{
		fetcher: newFetcher(opts.HTTPClient, opts.UserAgent, opts.MaxBytes),
		apiBase: opts.APIBase,
	}
	if c.apiBase == "" {
		c.apiBase = inatAPIBase
	}
	return c
}

// --- API response shapes (subset) ---

type inatPhoto struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`          // square thumbnail (size derived for download)
	MediumURL   string `json:"medium_url"`   // ≈500px
	LargeURL    string `json:"large_url"`    // ≈1024px (preferred download — SPEC §2.4.1)
	OriginalURL string `json:"original_url"` // full-res (heuristic)
	LicenseCode string `json:"license_code"` // unversioned, e.g. "cc-by"; null → "" (= ARR, unusable)
	Attribution string `json:"attribution"`  // plain-text credit string
}

type inatTaxon struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Rank         string     `json:"rank"`
	DefaultPhoto *inatPhoto `json:"default_photo"`
}

type inatTaxaResp struct {
	Results []inatTaxon `json:"results"`
}

type inatObservation struct {
	ID     int64       `json:"id"`
	Photos []inatPhoto `json:"photos"`
}

type inatObsResp struct {
	Results []inatObservation `json:"results"`
}

// Search resolves the scientific name to a taxon, then collects candidates from
// the taxon's default_photo plus its top observation photos (SPEC §2.4.1).
// Returns RAW candidates (parent classifies). (nil, nil) when no taxon matches.
func (c *INatClient) Search(ctx context.Context, scientificName string, limit int) ([]Candidate, error) {
	if limit <= 0 {
		limit = 12
	}
	taxa, err := c.searchTaxa(ctx, scientificName)
	if err != nil {
		return nil, err
	}
	// /v1/taxa?q= is a FUZZY search: it can return reordered, misspelling-tolerant,
	// or synonym-ish matches. Blindly trusting taxa[0] would attach another
	// species' photos to this slug. Mirror proxy/inat.go's PreferredCommonName:
	// only trust an exact (case-insensitive) scientific-name match; otherwise
	// return no candidates so the cascade falls through to the next source.
	want := strings.ToLower(strings.TrimSpace(scientificName))
	var top inatTaxon
	matched := false
	for _, tx := range taxa {
		if strings.ToLower(strings.TrimSpace(tx.Name)) == want {
			top = tx
			matched = true
			break
		}
	}
	if !matched {
		return nil, nil
	}

	var out []Candidate
	seen := map[string]bool{}
	add := func(p *inatPhoto) {
		if p == nil || len(out) >= limit {
			return
		}
		cand, ok := candidateFromPhoto(*p)
		if !ok || seen[cand.DedupKey] {
			return
		}
		seen[cand.DedupKey] = true
		out = append(out, cand)
	}

	// 1. Taxon default photo (not license-filtered at the API — may be any code).
	add(top.DefaultPhoto)

	// 2. Supplementary observation photos (CC0/CC-BY filtered at the API).
	if len(out) < limit {
		obs, oerr := c.searchObservations(ctx, top.ID, limit)
		if oerr != nil {
			// Observations are supplementary: only propagate the error when the
			// default photo gave us nothing, else return what we have.
			if len(out) == 0 {
				return nil, oerr
			}
		} else {
			for _, o := range obs {
				for i := range o.Photos {
					add(&o.Photos[i])
				}
				if len(out) >= limit {
					break
				}
			}
		}
	}
	return out, nil
}

// searchTaxa runs GET /taxa?q=&rank=species,genus,subspecies,variety&per_page=5.
// Ranks mirror the iOS search (Home/Search INatTaxaClient): a genus/subspecies
// entry (e.g. "Trifolium", "clovers") must resolve to its iNat taxon too, else
// the rank=species-only filter drops it, the exact-name match below finds no
// match, this source returns zero candidates, and the whole gallery falls
// through to Wikimedia — diverging from the iNat photo the user saw in search
// (hero pass-through then can never pin the forwarded photo).
func (c *INatClient) searchTaxa(ctx context.Context, name string) ([]inatTaxon, error) {
	q := url.Values{}
	q.Set("q", name)
	q.Set("rank", "species,genus,subspecies,variety")
	q.Set("per_page", "5")
	reqURL := c.apiBase + "/taxa?" + q.Encode()

	body, err := c.getWithRetry(ctx, reqURL)
	if err != nil {
		return nil, err
	}
	var resp inatTaxaResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode taxa: %v", ErrBadResponse, err)
	}
	return resp.Results, nil
}

// searchObservations runs GET /observations?taxon_id=&photo_license=cc0,cc-by
// &per_page=&order_by=votes&order=desc.
func (c *INatClient) searchObservations(ctx context.Context, taxonID int64, perPage int) ([]inatObservation, error) {
	if perPage <= 0 || perPage > 30 {
		perPage = 12
	}
	q := url.Values{}
	q.Set("taxon_id", strconv.FormatInt(taxonID, 10))
	q.Set("photo_license", inatObservationLicenses)
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("order_by", "votes")
	q.Set("order", "desc")
	reqURL := c.apiBase + "/observations?" + q.Encode()

	body, err := c.getWithRetry(ctx, reqURL)
	if err != nil {
		return nil, err
	}
	var resp inatObsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: decode observations: %v", ErrBadResponse, err)
	}
	return resp.Results, nil
}

// candidateFromPhoto builds a raw Candidate from an iNat photo. Returns ok=false
// when the photo has no usable license code (null → ARR) or no resolvable
// download URL.
func candidateFromPhoto(p inatPhoto) (Candidate, bool) {
	code := strings.ToLower(strings.TrimSpace(p.LicenseCode))
	if code == "" {
		return Candidate{}, false // null license_code = All Rights Reserved → unusable
	}
	download := firstNonEmpty(p.LargeURL, p.MediumURL, deriveLargeURL(p.URL), p.URL)
	if download == "" {
		return Candidate{}, false
	}
	return Candidate{
		Source:      SourceINaturalist,
		PageURL:     "https://www.inaturalist.org/photos/" + strconv.FormatInt(p.ID, 10),
		OriginalURL: firstNonEmpty(p.OriginalURL, p.LargeURL),
		DownloadURL: download,
		DedupKey:    "inat-photo-" + strconv.FormatInt(p.ID, 10),
		LicenseCode: code,
		Author:      strings.TrimSpace(p.Attribution),
	}, true
}

// deriveLargeURL upgrades an iNat square/small/medium rendition URL to the large
// rendition by swapping the size token in the path (iNat URLs end in
// "/<size>.<ext>"). Returns the input unchanged when no known size token is
// present.
func deriveLargeURL(u string) string {
	if u == "" {
		return ""
	}
	for _, sz := range []string{"square", "small", "medium", "thumb"} {
		token := "/" + sz + "."
		if strings.Contains(u, token) {
			return strings.Replace(u, token, "/large.", 1)
		}
	}
	return u
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
