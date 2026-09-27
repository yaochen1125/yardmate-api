package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// Authoritative out-of-catalog common names (SPEC §7 common_name C).
//
// The LLM invents / mistranslates common names for obscure plants, so the
// displayed common_name of every NON-catalog row is resolved from authoritative
// sources at response time, in this order:
//
//  1. iNaturalist preferred common name in the request language
//  2. Wikidata label (or a single unambiguous taxon common name, P1843) in the
//     request language
//  3. English common name (iNat english_common_name, else Wikidata en label)
//  4. the scientific name
//
// The LLM name is never used as a fallback — except when a source could not be
// asked at all (timeout / network error) and no localized name was found, in
// which case the row's own name is left untouched and nothing is cached, so the
// next request retries (a transient outage must not flip names to Latin).

// Name sources, written into PlantDetail.CommonNameSource (forensic only — iOS
// does not branch on it).
const (
	NameSourceINat           = "inaturalist"
	NameSourceWikidata       = "wikidata"
	NameSourceINatEnglish    = "inaturalist_en"
	NameSourceWikidataEn     = "wikidata_en"
	NameSourceScientificName = "scientific_name"
)

const (
	// nameResolveTimeout caps the whole resolution (both sources run in
	// parallel). The user is waiting on the detail page, so a slow source is
	// abandoned rather than awaited.
	nameResolveTimeout = 1500 * time.Millisecond

	defaultNameCacheSize = 20_000
	defaultNameCacheTTL  = 24 * time.Hour

	defaultWikidataEndpoint = "https://query.wikidata.org/sparql"
	userAgent               = "YardMate/1.0 (server; emanon.me@gmail.com)"
)

// ResolvedName is an authoritative display name for (scientific name, lang).
type ResolvedName struct {
	Name   string
	Source string // one of NameSource*
}

// Localized reports whether the name is in the request language (vs. an
// English / scientific-name fallback). Only localized names are fed to the LLM
// as the generation hint and baked into stored rows.
func (r ResolvedName) Localized() bool {
	return r.Source == NameSourceINat || r.Source == NameSourceWikidata
}

// commonNameLookup is one authoritative source. local is the name in the
// requested language ("" if the source has none); english is its English name.
// err == nil / errNameNoMatch mean the source ANSWERED; any other error means it
// could not be asked.
type commonNameLookup func(ctx context.Context, sciName, lang string) (local, english string, err error)

var errNameNoMatch = errors.New("names: no matching taxon")

// NameResolver resolves authoritative common names with a per-(name, lang)
// cache. Nil-safe: a nil resolver resolves nothing.
type NameResolver struct {
	inat     commonNameLookup // may be nil
	wikidata commonNameLookup // may be nil
	cache    *expirable.LRU[string, ResolvedName]
	sf       singleflight.Group
	timeout  time.Duration
}

// NewNameResolver wires the resolver. Either client may be nil (that source is
// then skipped); both nil → a resolver that never resolves.
func NewNameResolver(inat *proxy.INatClient, wd *WikidataClient) *NameResolver {
	r := &NameResolver{
		cache:   expirable.NewLRU[string, ResolvedName](defaultNameCacheSize, nil, defaultNameCacheTTL),
		timeout: nameResolveTimeout,
	}
	if inat != nil {
		r.inat = func(ctx context.Context, sciName, lang string) (string, string, error) {
			local, en, err := inat.LocalizedCommonNames(ctx, sciName, inatLocale(lang))
			if errors.Is(err, proxy.ErrINatNoMatch) {
				err = errNameNoMatch
			}
			return local, en, err
		}
	}
	if wd != nil {
		r.wikidata = wd.CommonNames
	}
	return r
}

// Resolve returns the authoritative name, or ok=false when it could not be
// determined (no sources wired, or a source failed without a localized hit) —
// the caller then keeps the row's own name.
func (r *NameResolver) Resolve(ctx context.Context, sciName, lang string) (ResolvedName, bool) {
	if r == nil || (r.inat == nil && r.wikidata == nil) {
		return ResolvedName{}, false
	}
	sciName = strings.TrimSpace(sciName)
	if sciName == "" {
		return ResolvedName{}, false
	}
	key := proxy.NormalizeScientificNamePrecise(sciName) + "|" + lang
	if v, ok := r.cache.Get(key); ok {
		return v, true
	}
	v, _, _ := r.sf.Do(key, func() (any, error) {
		// Detached from the caller: coalesced followers must not inherit the
		// leader's cancellation. Bounded by r.timeout.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
		defer cancel()
		res, ok := r.lookup(cctx, sciName, lang)
		if ok {
			r.cache.Add(key, res)
		}
		return resolveOutcome{res, ok}, nil
	})
	out := v.(resolveOutcome)
	return out.name, out.ok
}

type resolveOutcome struct {
	name ResolvedName
	ok   bool
}

type sourceAnswer struct {
	local, english string
	err            error
}

func (r *NameResolver) lookup(ctx context.Context, sciName, lang string) (ResolvedName, bool) {
	ask := func(f commonNameLookup) <-chan sourceAnswer {
		ch := make(chan sourceAnswer, 1)
		if f == nil {
			ch <- sourceAnswer{err: errNameNoMatch} // unwired source = answered "none"
			return ch
		}
		go func() {
			l, e, err := f(ctx, sciName, lang)
			ch <- sourceAnswer{strings.TrimSpace(l), strings.TrimSpace(e), err}
		}()
		return ch
	}
	inatCh, wdCh := ask(r.inat), ask(r.wikidata)
	in, wd := <-inatCh, <-wdCh

	answered := func(a sourceAnswer) bool { return a.err == nil || errors.Is(a.err, errNameNoMatch) }
	valid := func(n string) bool { return n != "" && !strings.EqualFold(n, sciName) }

	switch {
	case valid(in.local):
		return ResolvedName{in.local, NameSourceINat}, true
	case valid(wd.local):
		return ResolvedName{wd.local, NameSourceWikidata}, true
	case !answered(in) || !answered(wd):
		// A source may have had the localized name — don't settle for a
		// fallback on a transient failure.
		return ResolvedName{}, false
	case valid(in.english):
		return ResolvedName{in.english, NameSourceINatEnglish}, true
	case valid(wd.english):
		return ResolvedName{wd.english, NameSourceWikidataEn}, true
	default:
		return ResolvedName{sciName, NameSourceScientificName}, true
	}
}

// inatLocale maps a supported request lang to iNat's locale parameter.
func inatLocale(lang string) string {
	switch lang {
	case "zh-Hans":
		return "zh-CN"
	case "zh-Hant":
		return "zh-TW"
	default:
		return lang
	}
}

// wikidataLangs maps a supported request lang to the Wikidata language tags
// accepted for it, most preferred first. Bare "zh" is deliberately excluded:
// its script is unspecified, so it could put Simplified text on a Traditional
// row (or vice versa).
func wikidataLangs(lang string) []string {
	switch lang {
	case "zh-Hans":
		return []string{"zh-hans", "zh-cn", "zh-sg", "zh-my"}
	case "zh-Hant":
		return []string{"zh-hant", "zh-tw", "zh-hk", "zh-mo"}
	case "pt":
		return []string{"pt", "pt-br"}
	default:
		return []string{lang}
	}
}

// WikidataClient looks up a taxon (by exact P225 taxon name) on the Wikidata
// SPARQL endpoint.
type WikidataClient struct {
	HTTP     *http.Client
	Endpoint string
}

// NewWikidataClient returns a client with production defaults.
func NewWikidataClient() *WikidataClient {
	return &WikidataClient{
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		Endpoint: defaultWikidataEndpoint,
	}
}

// CommonNames returns the Wikidata name for sciName in lang, and its English
// name. For each language the item label wins; otherwise a taxon common name
// (P1843) is used only when exactly one distinct value exists (several values
// = ambiguous regional names, not trusted). Labels equal to the scientific name
// are ignored (Wikidata labels most species with their Latin name).
func (c *WikidataClient) CommonNames(ctx context.Context, sciName, lang string) (local, english string, err error) {
	if c == nil || c.HTTP == nil {
		return "", "", errors.New("wikidata: client not configured")
	}
	tags := append(wikidataLangs(lang), "en")
	quoted := make([]string, len(tags))
	for i, t := range tags {
		quoted[i] = fmt.Sprintf("%q", t)
	}
	query := fmt.Sprintf(`SELECT ?kind ?name (LANG(?name) AS ?lang) WHERE {
  ?item wdt:P225 %s .
  { ?item rdfs:label ?name . BIND("label" AS ?kind) }
  UNION
  { ?item wdt:P1843 ?name . BIND("common" AS ?kind) }
  FILTER(LANG(?name) IN (%s))
} LIMIT 200`, sparqlString(sciName), strings.Join(quoted, ","))

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultWikidataEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+url.Values{"query": {query}}.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("wikidata: status %d", resp.StatusCode)
	}
	var body struct {
		Results struct {
			Bindings []struct {
				Kind struct{ Value string } `json:"kind"`
				Name struct{ Value string } `json:"name"`
				Lang struct{ Value string } `json:"lang"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", err
	}
	if len(body.Results.Bindings) == 0 {
		return "", "", errNameNoMatch
	}

	labels := map[string]string{}
	commons := map[string]map[string]struct{}{}
	for _, b := range body.Results.Bindings {
		name := strings.TrimSpace(b.Name.Value)
		tag := strings.ToLower(b.Lang.Value)
		if name == "" || strings.EqualFold(name, sciName) {
			continue
		}
		switch b.Kind.Value {
		case "label":
			labels[tag] = name
		case "common":
			if commons[tag] == nil {
				commons[tag] = map[string]struct{}{}
			}
			commons[tag][name] = struct{}{}
		}
	}
	pick := func(tags []string) string {
		for _, t := range tags {
			if n := labels[t]; n != "" {
				return n
			}
		}
		for _, t := range tags {
			if len(commons[t]) == 1 {
				for n := range commons[t] {
					return n
				}
			}
		}
		return ""
	}
	return pick(wikidataLangs(lang)), pick([]string{"en"}), nil
}

// sparqlString renders s as a SPARQL string literal.
func sparqlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}
