package proxy

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Embedded YardMate content catalog. Built once at startup; immutable.
//
// We embed instead of fetching from CDN at runtime because:
//   - identification + diagnosis paths must not depend on a third-party CDN
//     being reachable from the server box at request time;
//   - the catalog is part of the API contract — clients can't see disease
//     id "L20" unless the server agrees, so versioning it with the binary
//     keeps the two in sync;
//   - file sizes are bounded (≈10 MB total) and YardMate v1 deploys are
//     fast enough that re-deploying for a content bump is acceptable.
//
// When the catalog updates (e.g. 1522 plants → 2000), copy fresh JSON from
// the yardmate-content / yardmate-swiftui scripts source-of-truth into
// proxy/data/ and rebuild.

//go:embed data/plants_index.json
var plantsIndexRaw []byte

//go:embed data/plants_detail.json
var plantsDetailRaw []byte

//go:embed data/diseases.json
var diseasesRaw []byte

// ContentIndex provides fast in-memory lookups over the YardMate plant + disease
// catalog. Built once at startup from embedded JSON. Read-only, safe for
// concurrent use by multiple request handlers.
type ContentIndex struct {
	// scientificNameToID maps the species-level normalized scientific_name
	// (var./subsp./cv./f. rank markers stripped) to plantId.
	// e.g. "monstera deliciosa" -> "AAA1234". Used as the fuzzy fallback for
	// queries that carry no infraspecific epithet. Built first-write-wins, so
	// when several catalog entries fold to the same species key (e.g. the five
	// Brassica oleracea cultivars) this deterministically points at the first
	// one in catalog order — see scientificNameToIDPrecise for the exact match.
	scientificNameToID map[string]string

	// scientificNameToIDPrecise maps an infraspecific-preserving normalized
	// scientific_name to plantId, e.g. "brassica oleracea var. italica" ->
	// "AAA0207". Unlike scientificNameToID it does NOT strip rank markers, so
	// the five curated Brassica oleracea cultivars (acephala/botrytis/capitata/
	// gongylodes/italica, AAA0203-AAA0207) each get a distinct key instead of
	// colliding on "brassica oleracea". LookupPlantID consults this first.
	scientificNameToIDPrecise map[string]string

	// plantToCommonDiseases maps plantId to its ordered common_diseases_list
	// (catalog ids). Used by the F-option-2 异常 fallback in /v1/diagnose.
	plantToCommonDiseases map[string][]string

	// diseaseNameToID maps normalized disease catalog name to catalog id.
	// e.g. "powdery mildew" -> "L20".
	diseaseNameToID map[string]string

	// diseaseByID gives the full catalog entry by id.
	diseaseByID map[string]*DiseaseCatalog

	// fullPlantByID gives the full PlantDetail catalog entry by plantId.
	// Used by the enrichment path-1 lookup (proxy/enrichment/SPEC §2.1
	// step 2) to short-circuit Supabase + LLM for plants in the 1522 catalog.
	fullPlantByID map[string]*PlantDetail

	// commonByID gives the curated common_name by plantId (from plants_index).
	// Drives the "catalog hit -> curated common name" step of /v1/identify name
	// resolution (SPEC §2.1): a catalog hit prefers this over iNat / upstream.
	commonByID map[string]string
}

// DiseaseCatalog is the subset of diseases.json[*] fields the server consumes
// when constructing DiagnoseResult.Issues. Extra fields (treatment groups,
// homeRemedies, prevention groups, crossReferences) are intentionally not
// modeled here — the iOS client reads them from the CDN-hosted diseases.json
// for the full detail view.
type DiseaseCatalog struct {
	ID               string `json:"id"`
	Category         string `json:"category"`
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	FullName         string `json:"fullName"`
	ShortDescription string `json:"shortDescription"`
	SymptomAnalysis  string `json:"symptomAnalysis"`
}

// DiseaseNameRef is a (id, name) tuple used to feed the LLM disambiguation
// prompt when an upstream Plant.id disease name doesn't directly match a
// catalog name.
type DiseaseNameRef struct {
	ID   string
	Name string
}

// LoadContent parses the embedded JSON files and builds the lookup maps.
// Returns a *ContentIndex usable across goroutines. Call once at startup.
func LoadContent() (*ContentIndex, error) {
	var plants []struct {
		ID             string `json:"id"`
		ScientificName string `json:"scientific_name"`
		CommonName     string `json:"common_name"`
	}
	if err := json.Unmarshal(plantsIndexRaw, &plants); err != nil {
		return nil, fmt.Errorf("content: plants_index: %w", err)
	}
	// Two indexes over the same rows. Both are built first-write-wins so the
	// result is deterministic regardless of how many entries fold to one key
	// (plants_index.json order is stable). The precise index keeps the
	// infraspecific epithet and is the primary disambiguator; the species-level
	// index is the fuzzy fallback for queries with no variety. See
	// LookupPlantID for the two-tier read.
	sci := make(map[string]string, len(plants))
	sciPrecise := make(map[string]string, len(plants))
	common := make(map[string]string, len(plants))
	for _, p := range plants {
		if p.ID == "" {
			continue
		}
		if pkey := normalizeScientificNamePrecise(p.ScientificName); pkey != "" {
			if _, exists := sciPrecise[pkey]; !exists {
				sciPrecise[pkey] = p.ID
			}
		}
		key := normalizeScientificName(p.ScientificName)
		if key == "" {
			continue
		}
		if _, exists := sci[key]; !exists {
			sci[key] = p.ID
		}
		if p.CommonName != "" {
			common[p.ID] = p.CommonName
		}
	}

	// Parse the full plants_detail.json into typed PlantDetail entries.
	// One parse drives two derived maps: plantId -> common_diseases_list
	// (for /v1/diagnose F-option-2 fallback) and plantId -> *PlantDetail
	// (for the enrichment path-1 lookup).
	var details []PlantDetail
	if err := json.Unmarshal(plantsDetailRaw, &details); err != nil {
		return nil, fmt.Errorf("content: plants_detail: %w", err)
	}
	pdis := make(map[string][]string, len(details))
	fpd := make(map[string]*PlantDetail, len(details))
	for i := range details {
		p := &details[i]
		if p.ID == nil || *p.ID == "" {
			continue
		}
		pdis[*p.ID] = p.CommonDiseasesList
		fpd[*p.ID] = p
	}

	var diseaseFile struct {
		Diseases map[string]*DiseaseCatalog `json:"diseases"`
	}
	if err := json.Unmarshal(diseasesRaw, &diseaseFile); err != nil {
		return nil, fmt.Errorf("content: diseases: %w", err)
	}
	dnam := make(map[string]string, len(diseaseFile.Diseases))
	for id, dz := range diseaseFile.Diseases {
		if dz == nil {
			continue
		}
		key := normalizeDiseaseName(dz.Name)
		if key != "" {
			dnam[key] = id
		}
	}

	return &ContentIndex{
		scientificNameToID:        sci,
		scientificNameToIDPrecise: sciPrecise,
		plantToCommonDiseases:     pdis,
		diseaseNameToID:           dnam,
		diseaseByID:               diseaseFile.Diseases,
		fullPlantByID:             fpd,
		commonByID:                common,
	}, nil
}

// LookupPlantID maps a Plant.id / Pl@ntNet-reported scientific name to a
// YardMate plantId. Match is case-insensitive and tolerates the hybrid marker
// (`×` / stand-alone `x`). It is two-tier: an exact infraspecific match wins
// first (so "Brassica oleracea var. italica" resolves to its own cultivar id,
// distinct from var. acephala), then a species-level fallback strips variety /
// cultivar / subspecies suffixes (`var.`, `cv.`, `subsp.`, `f.`) so a query
// whose variety isn't separately curated still resolves to the species entry.
//
// Returns ("", false) on miss — iOS detail page must tolerate plantId=null
// (renders Plant.id-only data without YardMate cross-reference).
func (c *ContentIndex) LookupPlantID(scientificName string) (string, bool) {
	if c == nil {
		return "", false
	}
	// Tier 1: precise (infraspecific-preserving) match. Distinguishes the five
	// curated Brassica oleracea cultivars and any future multi-variety species
	// when the query carries the variety (Pl@ntNet emits e.g. "Brassica
	// oleracea var. italica" via scientificNameWithoutAuthor).
	if pkey := normalizeScientificNamePrecise(scientificName); pkey != "" {
		if id, ok := c.scientificNameToIDPrecise[pkey]; ok {
			return id, true
		}
	}
	// Tier 2: species-level fuzzy fallback. Strips var./subsp./cv./f. so a query
	// carrying a variety the catalog only stores at species level still resolves
	// (e.g. "Abelia chinensis var. ignored" -> "Abelia chinensis"), and a bare
	// genus+species query resolves deterministically to the first such entry.
	key := normalizeScientificName(scientificName)
	if key == "" {
		return "", false
	}
	if id, ok := c.scientificNameToID[key]; ok {
		return id, true
	}
	return "", false
}

// LookupCommonName returns the curated common_name for a YardMate plantId
// (from plants_index.json). Used by /v1/identify to prefer the reviewed
// catalog name over iNat / upstream when a suggestion resolves to the 1522
// catalog. Returns ("", false) on a nil index, empty id, or missing name.
func (c *ContentIndex) LookupCommonName(plantID string) (string, bool) {
	if c == nil || plantID == "" {
		return "", false
	}
	if name, ok := c.commonByID[plantID]; ok && name != "" {
		return name, true
	}
	return "", false
}

// LookupCatalogID maps a Plant.id disease name to a YardMate catalog id
// (e.g. "Powdery mildew" → "L20"). Match is case-insensitive and strips
// boilerplate suffixes ("disease", "infection"). Returns ("", false) on miss.
//
// On miss, callers should fall back to LLM disambiguation
// (VisionClient.DisambiguateDiseaseName); on LLM miss/timeout, generic
// catalog (L06 "Leaf spot") with isFallback=true.
func (c *ContentIndex) LookupCatalogID(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	key := normalizeDiseaseName(name)
	if key == "" {
		return "", false
	}
	if id, ok := c.diseaseNameToID[key]; ok {
		return id, true
	}
	return "", false
}

// CommonDiseasesFor returns the ordered common_diseases_list for a plantId
// (catalog ids), or nil if the plant is not in the index.
func (c *ContentIndex) CommonDiseasesFor(plantID string) []string {
	if c == nil {
		return nil
	}
	return c.plantToCommonDiseases[plantID]
}

// DiseaseByID returns the full catalog entry for an id, or (nil, false).
func (c *ContentIndex) DiseaseByID(id string) (*DiseaseCatalog, bool) {
	if c == nil {
		return nil, false
	}
	d, ok := c.diseaseByID[id]
	return d, ok
}

// AllDiseaseNames returns every (id, name) tuple in the catalog. Used as
// input to the LLM disambiguation prompt; small (≈70 entries) so the cost
// of regenerating it per request is negligible.
func (c *ContentIndex) AllDiseaseNames() []DiseaseNameRef {
	if c == nil {
		return nil
	}
	out := make([]DiseaseNameRef, 0, len(c.diseaseByID))
	for id, d := range c.diseaseByID {
		if d == nil {
			continue
		}
		out = append(out, DiseaseNameRef{ID: id, Name: d.Name})
	}
	return out
}

// NormalizeScientificName is the exported form of the package-private
// normalizeScientificName helper. The enrichment package uses it to compute
// the Supabase plants_pending PK in lockstep with catalog lookups — the two
// MUST share the exact same normalization rules (proxy/enrichment/SPEC §9 #1).
func NormalizeScientificName(s string) string {
	return normalizeScientificName(s)
}

// LookupFullDetail returns the full PlantDetail entry for a catalog plantId
// (e.g. "AAA0001"), or (nil, false) on miss. Used by the enrichment path-1
// lookup (proxy/enrichment/SPEC §2.1 step 2) to short-circuit Supabase + LLM
// for plants already in the curated 1522 catalog.
func (c *ContentIndex) LookupFullDetail(plantID string) (*PlantDetail, bool) {
	if c == nil || plantID == "" {
		return nil, false
	}
	p, ok := c.fullPlantByID[plantID]
	return p, ok
}

// --- normalization helpers ---

// normalizeScientificName lowercases and strips common variety / cultivar /
// subspecies suffixes for fuzzy matching. The hybrid marker (Unicode × or a
// stand-alone ASCII "x") is dropped so "Abelia x grandiflora" and
// "Abelia × grandiflora" map to the same key. Examples:
//
//	"Monstera deliciosa"          -> "monstera deliciosa"
//	"Monstera deliciosa var. X"   -> "monstera deliciosa"
//	"Abelia × grandiflora"        -> "abelia grandiflora"
//	"Abelia x grandiflora"        -> "abelia grandiflora"
//	"  Abies   nordmanniana  "    -> "abies nordmanniana"
func normalizeScientificName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "×", " ")
	for _, marker := range []string{" var.", " cv.", " subsp.", " ssp.", " f.", " forma "} {
		if i := strings.Index(s, marker); i >= 0 {
			s = s[:i]
		}
	}
	// Drop stand-alone "x" tokens — the hybrid marker in the catalog.
	// Real species names with "x" as a substring (e.g. "Buxus") survive
	// because the check requires the field to be exactly "x".
	fields := strings.Fields(s)
	out := fields[:0]
	for _, f := range fields {
		if f == "x" {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// normalizeScientificNamePrecise is like normalizeScientificName but does NOT
// strip the infraspecific rank markers (var./subsp./ssp./cv./f./forma). It still
// lowercases, trims, collapses whitespace, and drops the hybrid × / stand-alone
// "x" marker, so "Brassica oleracea var. italica" -> "brassica oleracea var.
// italica" while "Chrysanthemum × morifolium" and "Chrysanthemum x morifolium"
// still fold together. Examples:
//
//	"Brassica oleracea var. italica"  -> "brassica oleracea var. italica"
//	"Brassica oleracea var. acephala" -> "brassica oleracea var. acephala"
//	"Abelia × grandiflora"            -> "abelia grandiflora"
//
// Used ONLY for the catalog's precise scientificNameToIDPrecise index. It is
// deliberately NOT exported and NOT used for the Supabase plants_pending PK,
// which stays on normalizeScientificName — per enrichment SPEC §9 #1 the PK
// normalizer is a single source of truth and changing it needs an offline
// migration. Adding this sibling helper leaves that contract untouched.
func normalizeScientificNamePrecise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "×", " ")
	fields := strings.Fields(s)
	out := fields[:0]
	for _, f := range fields {
		if f == "x" {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// normalizeDiseaseName lowercases, trims, and strips common boilerplate
// suffixes ("disease", "infection"). Plant.id names like "Brown spot disease"
// fold to "brown spot", which then matches diseases.json L01 "Brown spot".
func normalizeDiseaseName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, suffix := range []string{" disease", " infection"} {
		s = strings.TrimSuffix(s, suffix)
	}
	return strings.Join(strings.Fields(s), " ")
}

// speciesBinomial reduces a scientific name to its "Genus species" binomial for
// iNat lookup + display collapse on /v1/identify, WITHOUT touching the catalog
// key builder (normalizeScientificName) — so the 1522 catalog keys are
// untouched. It collapses only a TRUE infraspecific epithet:
//
//   - a single quote (cultivar, e.g. "Rosa 'Knock Out'")              -> unchanged
//
//   - a hybrid marker × (after rank-marker strip)                     -> unchanged
//
//   - a rank marker (var./subsp./ssp./cv./f./forma)        -> cut to the binomial
//
//   - a bare 3rd token that is an all-lowercase Latin word (anilina,
//     horizontalis)                                        -> cut to the binomial
//
//   - anything else (author "Jacq.", "Graptoveria Fred Ives")         -> unchanged
//
//     "Triteleia ixioides anilina"      -> "Triteleia ixioides"
//     "Rosa chinensis subsp. spontanea" -> "Rosa chinensis"
//     "Brassica oleracea var. capitata" -> "Brassica oleracea"
//     "Rosa 'Knock Out'"                -> "Rosa 'Knock Out'"      (unchanged)
//     "Rosa chinensis Jacq."            -> "Rosa chinensis Jacq."  (unchanged)
func speciesBinomial(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "'") { // cultivar — never collapse
		return s
	}
	// Strip a rank marker and everything after it, preserving original case.
	// If stripping leaves less than a binomial (genus + species), bail out
	// and keep the original — a marker right after a single token is a
	// malformed input (e.g. "Eucalyptus f. xxx" with no species epithet)
	// and engines essentially never emit it; defend rather than collapse
	// to a bare genus.
	original := s
	low := strings.ToLower(s)
	for _, m := range []string{" var.", " cv.", " subsp.", " ssp.", " f.", " forma "} {
		if i := strings.Index(low, m); i >= 0 {
			stripped := strings.TrimSpace(s[:i])
			if len(strings.Fields(stripped)) < 2 {
				return original
			}
			s = stripped
			break
		}
	}
	if strings.Contains(s, "×") { // hybrid (Unicode) — never collapse
		return s
	}
	f := strings.Fields(s)
	// Stand-alone ASCII "x" is the other hybrid marker (e.g. "Abelia x
	// grandiflora", "Citrus x paradisi" — both present in plants_index.json).
	// The catalog key builder normalizeScientificName drops the "x" token; for
	// display + iNat we keep the full hybrid name as-is, because a bare
	// "Abelia grandiflora" / "Citrus paradisi" is a different epithet and a
	// collapse would corrupt the displayed scientific_name.
	for _, t := range f {
		if t == "x" {
			return s
		}
	}
	if len(f) < 3 || !isLowerLatin(f[1]) || !isLowerLatin(f[2]) {
		return strings.Join(f, " ")
	}
	return f[0] + " " + f[1]
}

// isLowerLatin reports whether w is a non-empty all-lowercase a-z word — used
// by speciesBinomial to tell a true infraspecific epithet (lowercase Latin)
// from an author citation / cultivar token (capitalised or punctuated).
func isLowerLatin(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}
