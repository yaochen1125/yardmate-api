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
	"unicode"

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
//
// The iNat query is scoped to Plantae + Fungi and also returns the taxon's
// kingdom, which Resolve hands back with the name (Resolution.Kingdom), so the
// request path gets the mushroom-safety kingdom without a second iNat call
// (SPEC §7 kingdom).

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

	// An INCOMPLETE result (name unresolved, or iNat did not answer) is held
	// only briefly: long enough that an iNat outage / 429 streak is not hammered
	// by every request (LRU hits included — each awaits the resolver), short
	// enough that the name and kingdom recover within minutes.
	defaultNameRetrySize = 5_000
	defaultNameRetryTTL  = 2 * time.Minute

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

// Resolution is one Resolve outcome: the display name, plus the iNat kingdom
// that the SAME iNat taxa query returned (enrichment asks iNat once per request
// for both — SPEC §7 kingdom).
type Resolution struct {
	// Name is the authoritative display name; meaningful only when OK.
	Name ResolvedName
	// OK reports whether Name could be determined. false (no sources wired, or a
	// source failed without a localized hit) → the caller keeps the row's own name.
	OK bool
	// Kingdom is iNat's normalized kingdom ("Plantae" / "Fungi") for the matched
	// taxon; nil when iNat has no exact match or did not answer. Independent of OK.
	Kingdom *string
	// INatAnswered reports that iNat responded (a match or a definitive miss).
	// false when iNat is not wired or the request failed / timed out — the only
	// case where a caller that needs the kingdom should ask iNat again.
	INatAnswered bool
}

// sourceAnswer is one source's reply. local is the name in the requested
// language ("" if the source has none); english is its English name; kingdom is
// iNat's iconic taxon ("" for Wikidata). err == nil / errNameNoMatch mean the
// source ANSWERED; any other error means it could not be asked.
type sourceAnswer struct {
	local, english string
	kingdom        string
	err            error
}

// commonNameLookup is one authoritative source.
type commonNameLookup func(ctx context.Context, sciName, lang string) sourceAnswer

var errNameNoMatch = errors.New("names: no matching taxon")

// NameResolver resolves authoritative common names with a per-(name, lang)
// cache. Nil-safe: a nil resolver resolves nothing.
type NameResolver struct {
	inat     commonNameLookup                   // may be nil
	wikidata commonNameLookup                   // may be nil
	cache    *expirable.LRU[string, Resolution] // complete results, 24 h
	retry    *expirable.LRU[string, Resolution] // incomplete results, 2 min
	sf       singleflight.Group
	timeout  time.Duration
}

// NewNameResolver wires the resolver. Either client may be nil (that source is
// then skipped); both nil → a resolver that never resolves.
func NewNameResolver(inat *proxy.INatClient, wd *WikidataClient) *NameResolver {
	r := &NameResolver{
		cache:   expirable.NewLRU[string, Resolution](defaultNameCacheSize, nil, defaultNameCacheTTL),
		retry:   expirable.NewLRU[string, Resolution](defaultNameRetrySize, nil, defaultNameRetryTTL),
		timeout: nameResolveTimeout,
	}
	if inat != nil {
		r.inat = func(ctx context.Context, sciName, lang string) sourceAnswer {
			t, err := inat.LocalizedCommonNames(ctx, sciName, inatLocale(lang))
			if errors.Is(err, proxy.ErrINatNoMatch) {
				err = errNameNoMatch
			}
			return sourceAnswer{local: t.CommonName, english: t.EnglishCommonName, kingdom: t.Kingdom, err: err}
		}
	}
	if wd != nil {
		r.wikidata = func(ctx context.Context, sciName, lang string) sourceAnswer {
			local, en, err := wd.CommonNames(ctx, sciName, lang)
			return sourceAnswer{local: local, english: en, err: err}
		}
	}
	return r
}

// Resolve returns the authoritative name (Resolution.OK=false when it could not
// be determined — the caller then keeps the row's own name) together with the
// iNat kingdom from the same query.
//
// A result is cached for 24 h only when it is complete: the name resolved AND
// iNat answered (or is not wired). Anything less — a source outage without a
// localized name, or iNat failing while Wikidata supplied one (iNat outranks
// Wikidata for the name and is the only kingdom source) — is still returned but
// held for just 2 min, so the sources are asked again soon without being
// hammered while they are down.
func (r *NameResolver) Resolve(ctx context.Context, sciName, lang string) Resolution {
	if r == nil || (r.inat == nil && r.wikidata == nil) {
		return Resolution{}
	}
	sciName = strings.TrimSpace(sciName)
	if sciName == "" {
		return Resolution{}
	}
	key := proxy.NormalizeScientificNamePrecise(sciName) + "|" + lang
	if v, ok := r.cache.Get(key); ok {
		return v
	}
	if v, ok := r.retry.Get(key); ok {
		return v
	}
	v, _, _ := r.sf.Do(key, func() (any, error) {
		// Detached from the caller: coalesced followers must not inherit the
		// leader's cancellation. Bounded by r.timeout.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.timeout)
		defer cancel()
		res := r.lookup(cctx, sciName, lang)
		if res.OK && (r.inat == nil || res.INatAnswered) {
			r.cache.Add(key, res)
		} else {
			r.retry.Add(key, res)
		}
		return res, nil
	})
	return v.(Resolution)
}

func (r *NameResolver) lookup(ctx context.Context, sciName, lang string) Resolution {
	ask := func(f commonNameLookup) <-chan sourceAnswer {
		ch := make(chan sourceAnswer, 1)
		if f == nil {
			ch <- sourceAnswer{err: errNameNoMatch} // unwired source = answered "none"
			return ch
		}
		go func() {
			a := f(ctx, sciName, lang)
			a.local, a.english = strings.TrimSpace(a.local), strings.TrimSpace(a.english)
			ch <- a
		}()
		return ch
	}
	inatCh, wdCh := ask(r.inat), ask(r.wikidata)

	answered := func(a sourceAnswer) bool { return a.err == nil || errors.Is(a.err, errNameNoMatch) }
	valid := func(n string) bool { return n != "" && !strings.EqualFold(n, sciName) }

	in := <-inatCh
	out := Resolution{INatAnswered: r.inat != nil && answered(in)}
	if out.INatAnswered {
		out.Kingdom = proxy.NormalizeKingdom(in.kingdom)
	}
	// iNat's localized name is the top-priority answer and iNat is the only
	// kingdom source, so once it has one nothing Wikidata says can change the
	// result — don't wait for a slow Wikidata (its request is cancelled when the
	// caller's deferred cancel fires; the buffered channel never blocks).
	if valid(in.local) {
		out.Name, out.OK = ResolvedName{in.local, NameSourceINat}, true
		return out
	}
	wd := <-wdCh
	switch {
	case valid(wd.local):
		out.Name, out.OK = ResolvedName{wd.local, NameSourceWikidata}, true
	case !answered(in) || !answered(wd):
		// A source may have had the localized name — don't settle for a
		// fallback on a transient failure.
	case valid(in.english):
		out.Name, out.OK = ResolvedName{in.english, NameSourceINatEnglish}, true
	case valid(wd.english):
		out.Name, out.OK = ResolvedName{wd.english, NameSourceWikidataEn}, true
	default:
		out.Name, out.OK = ResolvedName{sciName, NameSourceScientificName}, true
	}
	return out
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
	// P225 is an exact, case-sensitive literal: query the canonical spelling so
	// "trifolium  hirtum" still finds "Trifolium hirtum" (the resolver caches
	// under a normalized key, so a spelling-only miss would stick for 24 h).
	sciName = canonicalSciName(sciName)
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

// canonicalSciName rewrites a scientific name into the conventional spelling
// taxon databases store: single spaces, genus capitalized ("×" hybrid marker
// kept), every other token lowercase (epithets and rank markers) — except a
// quoted cultivar name, which is kept as written ("Rosa 'Knock Out'").
func canonicalSciName(s string) string {
	closes := func(t string) bool { return strings.HasSuffix(t, "'") || strings.HasSuffix(t, "’") }
	fields := strings.Fields(s)
	inQuote := false
	for i, f := range fields {
		if inQuote { // inside a cultivar name: keep, until the closing quote
			inQuote = !closes(f)
			continue
		}
		if rest := strings.TrimLeft(f, "'‘"); rest != f { // opening quote ('Peace' closes at once)
			inQuote = !closes(rest)
			continue
		}
		if i == 0 {
			fields[i] = capitalizeGenus(f)
		} else {
			fields[i] = strings.ToLower(f)
		}
	}
	return strings.Join(fields, " ")
}

// capitalizeGenus upper-cases the first letter of a genus token (after an
// optional "×" hybrid marker) and lower-cases the rest.
func capitalizeGenus(g string) string {
	r := []rune(strings.ToLower(g))
	for i, c := range r {
		if c != '×' {
			r[i] = unicode.ToUpper(c)
			break
		}
	}
	return string(r)
}

// sparqlString renders s as a SPARQL string literal.
func sparqlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}
