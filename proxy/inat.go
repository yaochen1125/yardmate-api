package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// INatClient queries the iNaturalist taxa API for a curated
// preferred_common_name, used to upgrade engine-reported common names on
// /v1/identify (e.g. Pl@ntNet "Ixia" -> iNat "Prettyface" for Triteleia
// ixioides) when the plant is OUTSIDE the 1522 catalog. Best-effort: any
// failure returns ("", false) and the caller keeps the upstream common_names —
// it MUST NOT block or fail identify.
type INatClient struct {
	HTTP    *http.Client
	BaseURL string
}

const (
	defaultINatBaseURL = "https://api.inaturalist.org/v1/taxa"
	defaultINatTimeout = 8 * time.Second
	inatPlantaeTaxonID = "47126" // Kingdom Plantae — scope the query to plants
	inatFungiTaxonID   = "47170" // Kingdom Fungi (incl. lichens)
)

// INatTaxon is the slice of an iNat taxon record LookupTaxon returns.
type INatTaxon struct {
	// CommonName is iNat's preferred_common_name ("" when iNat has none).
	CommonName string
	// Kingdom is iNat's iconic_taxon_name for the match — "Plantae" or "Fungi"
	// given the query scope (normalize via NormalizeKingdom before use).
	Kingdom string
}

// NewINatClient returns an INatClient with production defaults.
func NewINatClient() *INatClient {
	return &INatClient{
		HTTP:    &http.Client{Timeout: defaultINatTimeout},
		BaseURL: defaultINatBaseURL,
	}
}

// PreferredCommonName returns iNat's preferred_common_name for sciName. Pass a
// species-level binomial (see speciesBinomial) so the species common name is
// returned, not a subspecies one. Returns ("", false) on any error, no match,
// a name mismatch against the query, or an empty common name — the caller then
// keeps the upstream common_names.
func (c *INatClient) PreferredCommonName(ctx context.Context, sciName string) (string, bool) {
	matches, _ := c.searchTaxa(ctx, sciName, inatPlantaeTaxonID)
	for _, r := range matches {
		if r.PreferredCommonName != "" {
			return r.PreferredCommonName, true
		}
	}
	return "", false
}

// LookupTaxon resolves sciName's kingdom AND preferred common name in ONE
// request, scoped to Plantae + Fungi (PreferredCommonName is plants-only, so a
// fungus never matches there). Used by enrichment, which needs the authoritative
// kingdom for the mushroom-safety notice and would otherwise pay a second
// round-trip for it. ok=false on any error / no exact-name match — best-effort
// like PreferredCommonName; the caller must carry on without it.
//
// Only exact scientific-name matches are trusted. When several match (a
// cross-kingdom homonym), Kingdom is Fungi if ANY of them is a fungus (safety
// bias, see MergeKingdom) and CommonName comes from the most-observed match that
// has one.
func (c *INatClient) LookupTaxon(ctx context.Context, sciName string) (INatTaxon, bool) {
	t, ok, _ := c.LookupTaxonErr(ctx, sciName)
	return t, ok
}

// LookupTaxonErr is LookupTaxon for callers that must tell "iNat answered and has
// no exact match" (ok=false, err=nil — a definitive miss) from "the lookup itself
// failed" (err != nil: transport error, timeout, 429 / 5xx, undecodable body —
// retryable). The serving path does not care (both mean "carry on without it");
// the kingdom backfill does, so a rate-limited run is not misreported as a pile
// of undeterminable plants. A non-2xx status is an *INatStatusError.
func (c *INatClient) LookupTaxonErr(ctx context.Context, sciName string) (INatTaxon, bool, error) {
	matches, err := c.searchTaxa(ctx, sciName, inatPlantaeTaxonID+","+inatFungiTaxonID)
	if err != nil {
		return INatTaxon{}, false, err
	}
	if len(matches) == 0 {
		return INatTaxon{}, false, nil
	}
	var out INatTaxon
	for _, r := range matches {
		if out.CommonName == "" {
			out.CommonName = r.PreferredCommonName
		}
		if out.Kingdom == "" || strings.EqualFold(r.IconicTaxonName, KingdomFungi) {
			out.Kingdom = r.IconicTaxonName
		}
	}
	return out, true, nil
}

// INatStatusError is a non-2xx iNat response (429 = rate limited, 5xx = iNat down).
type INatStatusError struct{ Status int }

func (e *INatStatusError) Error() string { return fmt.Sprintf("inat: status %d", e.Status) }

// inatTaxonResult is one iNat /v1/taxa result row (the fields we consume).
type inatTaxonResult struct {
	Name                string `json:"name"`
	PreferredCommonName string `json:"preferred_common_name"`
	IconicTaxonName     string `json:"iconic_taxon_name"`
}

// searchTaxa runs the iNat taxa search for sciName inside taxonScope (one taxon
// id, or a comma-separated list) and returns ONLY the results whose name equals
// the query exactly, in iNat's observations-count order. A non-nil error means
// the lookup did not complete (nil client, transport, non-2xx, decode); (nil, nil)
// means iNat answered and nothing matched exactly.
func (c *INatClient) searchTaxa(ctx context.Context, sciName, taxonScope string) ([]inatTaxonResult, error) {
	if c == nil || c.HTTP == nil {
		return nil, errors.New("inat: nil client")
	}
	if strings.TrimSpace(sciName) == "" {
		return nil, nil
	}
	base := c.BaseURL
	if base == "" {
		base = defaultINatBaseURL
	}
	q := url.Values{}
	q.Set("q", sciName)
	q.Set("taxon_id", taxonScope)
	q.Set("rank", "species,subspecies,variety,genus")
	q.Set("per_page", "5")
	q.Set("order_by", "observations_count")
	q.Set("order", "desc")
	q.Set("is_active", "true")
	q.Set("fields", "name,rank,preferred_common_name,iconic_taxon_name")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "YardMate/1.0 (server; emanon.me@gmail.com)")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &INatStatusError{Status: resp.StatusCode}
	}

	var body struct {
		Results []inatTaxonResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	want := strings.ToLower(strings.TrimSpace(sciName))
	var out []inatTaxonResult
	for _, r := range body.Results {
		// q is a fuzzy search — only trust an exact species-name match so iNat
		// returning a different taxon first can't poison the common name / kingdom.
		if strings.ToLower(strings.TrimSpace(r.Name)) == want {
			out = append(out, r)
		}
	}
	return out, nil
}
