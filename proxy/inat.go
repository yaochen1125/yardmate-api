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
)

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
	if c == nil || c.HTTP == nil || strings.TrimSpace(sciName) == "" {
		return "", false
	}
	base := c.BaseURL
	if base == "" {
		base = defaultINatBaseURL
	}
	q := url.Values{}
	q.Set("q", sciName)
	q.Set("taxon_id", inatPlantaeTaxonID)
	q.Set("rank", "species,subspecies,variety,genus")
	q.Set("per_page", "5")
	q.Set("order_by", "observations_count")
	q.Set("order", "desc")
	q.Set("is_active", "true")
	q.Set("fields", "name,rank,preferred_common_name")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "YardMate/1.0 (server; emanon.me@gmail.com)")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", false
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}

	var body struct {
		Results []struct {
			Name                string `json:"name"`
			PreferredCommonName string `json:"preferred_common_name"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false
	}

	want := strings.ToLower(strings.TrimSpace(sciName))
	for _, r := range body.Results {
		// q is a fuzzy search — only trust an exact species-name match so iNat
		// returning a different taxon first can't poison the common name.
		if strings.ToLower(strings.TrimSpace(r.Name)) == want && r.PreferredCommonName != "" {
			return r.PreferredCommonName, true
		}
	}
	return "", false
}

// ErrINatNoMatch is returned by LocalizedCommonNames when iNat answered but has
// no taxon whose name matches the query — a DEFINITIVE "iNat does not know this
// plant", as opposed to a transport / decode failure (any other error), which
// callers must treat as "unknown" and not cache.
var ErrINatNoMatch = errors.New("inat: no matching taxon")

// LocalizedCommonNames returns iNat's preferred common name in the given iNat
// locale (e.g. "zh-CN", "ja", "de") plus its English common name, from ONE
// taxa query. local is "" when iNat has no name in that locale (iNat does NOT
// fall back to English in preferred_common_name — english_common_name carries
// that separately). A taxon matches when its name equals sciName or, for an
// infraspecific input, its species binomial.
//
// err == nil means iNat answered and a taxon matched (either name may still be
// ""); ErrINatNoMatch means iNat answered with no matching taxon; any other
// error means iNat could not be asked (timeout, non-2xx, bad JSON).
func (c *INatClient) LocalizedCommonNames(ctx context.Context, sciName, locale string) (local, english string, err error) {
	if c == nil || c.HTTP == nil {
		return "", "", errors.New("inat: client not configured")
	}
	sciName = strings.TrimSpace(sciName)
	if sciName == "" {
		return "", "", ErrINatNoMatch
	}
	base := c.BaseURL
	if base == "" {
		base = defaultINatBaseURL
	}
	q := url.Values{}
	q.Set("q", sciName)
	q.Set("taxon_id", inatPlantaeTaxonID)
	q.Set("rank", "species,subspecies,variety,genus")
	q.Set("per_page", "5")
	q.Set("order_by", "observations_count")
	q.Set("order", "desc")
	q.Set("is_active", "true")
	q.Set("fields", "name,rank,preferred_common_name,english_common_name")
	if locale != "" {
		q.Set("locale", locale)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "YardMate/1.0 (server; emanon.me@gmail.com)")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("inat: status %d", resp.StatusCode)
	}

	var body struct {
		Results []struct {
			Name                string `json:"name"`
			PreferredCommonName string `json:"preferred_common_name"`
			EnglishCommonName   string `json:"english_common_name"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", err
	}

	// q is a fuzzy search — only trust an exact name match (full name first,
	// then the species binomial of an infraspecific input).
	wants := []string{strings.ToLower(sciName)}
	if b := strings.ToLower(speciesBinomial(sciName)); b != wants[0] {
		wants = append(wants, b)
	}
	for _, want := range wants {
		for _, r := range body.Results {
			if strings.ToLower(strings.TrimSpace(r.Name)) == want {
				return strings.TrimSpace(r.PreferredCommonName), strings.TrimSpace(r.EnglishCommonName), nil
			}
		}
	}
	return "", "", ErrINatNoMatch
}
