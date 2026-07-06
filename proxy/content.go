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

	// sciByID maps a curated catalog id (AAA-id) to its authoritative, VERBATIM
	// scientific_name from plants_index.json — the reverse of the name->id indexes
	// above (normalization is intentionally NOT applied: the value is fed straight
	// into the iNat/Wikimedia cascade search). imageingest's /v1/plants/catalog-images
	// reads it (via CatalogScientificNames) to derive the search name server-side
	// from catalog_id, ignoring the client-supplied name — so a tampered attested
	// client cannot pair a real id with an unrelated species to poison that plant's
	// supplementary external/ gallery (imageingest SPEC §2.8, Codex P1).
	sciByID map[string]string

	// stepByID / remedyByID are the shared treatment-step (S01–S44) and
	// home-remedy (K01–K15) pools from diseases.json `shared`. Disease enrichment
	// back-fills an out-of-catalog disease's generated step/remedy refs from these
	// (proxy/enrichment/SPEC_disease.md). In-catalog diseases don't use them
	// (iOS reads their already-denormalized steps from the CDN).
	stepByID   map[string]*SharedStep
	remedyByID map[string]*SharedRemedy
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

// SharedStep is one entry of diseases.json `shared.steps` (S01–S44): a reusable
// treatment/prevention step. Disease enrichment back-fills generated step refs
// from this pool (title/body/image), so an out-of-catalog disease's steps reuse
// the same human-reviewed copy + image as in-catalog diseases.
type SharedStep struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Image string `json:"image"`
}

// SharedRemedy is one entry of diseases.json `shared.remedies` (K01–K15): a
// reusable home remedy. Treats lists the disease ids it applies to (pool-internal;
// dropped when denormalized into an issue).
type SharedRemedy struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Recipe string   `json:"recipe"`
	Usage  string   `json:"usage"`
	Treats []string `json:"treats"`
	Image  string   `json:"image"`
}

// DiseaseNameRef is a (id, name) tuple used to feed the LLM disambiguation
// prompt when an upstream Plant.id disease name doesn't directly match a
// catalog name.
type DiseaseNameRef struct {
	ID          string
	Name        string
	Description string // short symptom hint (shortDescription) for LLM disambiguation
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
	sciByID := make(map[string]string, len(plants))
	for _, p := range plants {
		if p.ID == "" {
			continue
		}
		// Reverse index id -> verbatim scientific_name (catalog-images search name,
		// SPEC §2.8). Set before the key=="" guard below so it never depends on the
		// name normalizing to non-empty; catalog ids are unique so a plain assign is
		// deterministic.
		if s := strings.TrimSpace(p.ScientificName); s != "" {
			sciByID[p.ID] = s
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
		Shared struct {
			Steps    map[string]*SharedStep   `json:"steps"`
			Remedies map[string]*SharedRemedy `json:"remedies"`
		} `json:"shared"`
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
		sciByID:                   sciByID,
		stepByID:                  diseaseFile.Shared.Steps,
		remedyByID:                diseaseFile.Shared.Remedies,
	}, nil
}

// LookupPlantID maps a Plant.id / Pl@ntNet-reported scientific name to a
// YardMate plantId. Match is case-insensitive. It is two-tier:
//
//   - Tier 1 (precise) preserves variety / subspecies / cultivar suffixes AND a
//     stand-alone ASCII "x" — so "Brassica oleracea var. italica" resolves to
//     its own cultivar id (distinct from var. acephala), and a catalog row that
//     uses the ASCII-x hybrid form (e.g. "Chrysanthemum x morifolium", AAA0325)
//     stays addressable separately from its bare-form sibling.
//   - Tier 2 (species fallback) folds the Unicode × marker, drops a stand-alone
//     ASCII "x", and strips variety / cultivar / subspecies suffixes (`var.`,
//     `cv.`, `subsp.`, `f.`) — so a query whose variety / hybrid marker the
//     catalog doesn't carry separately still resolves to the species entry.
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

// CatalogScientificNames returns a fresh copy of the authoritative catalog-id ->
// scientific_name map for the curated 1522 (verbatim from the embedded
// plants_index.json).
//
// proxy/imageingest consumes it (injected as imageingest.Config.CatalogNames via
// main.buildImageIngestService) to derive the SERVER-SIDE search name for
// POST /v1/plants/catalog-images from catalog_id, ignoring the client-supplied
// scientific_name. This resolves the catalog-images P1: without it a tampered
// attested client could pair a real catalog id (AAA0001) with an unrelated
// species name and poison that curated plant's external/ supplementary gallery
// (imageingest SPEC §2.8). A copy is returned so the consumer can never mutate
// the shared, read-only index.
func (c *ContentIndex) CatalogScientificNames() map[string]string {
	if c == nil {
		return nil
	}
	out := make(map[string]string, len(c.sciByID))
	for id, name := range c.sciByID {
		out[id] = name
	}
	return out
}

// LookupCatalogID maps a Plant.id disease name to a YardMate catalog id
// (e.g. "Powdery mildew" → "L20"). Match is case-insensitive and strips
// boilerplate suffixes ("disease", "infection"). Returns ("", false) on miss.
//
// On miss, mapCatalogID falls back to the alias table then LLM disambiguation
// (VisionClient.DisambiguateDiseaseName); a genuine miss yields catalogId=null
// and the out-of-catalog detail is handled by disease enrichment
// (proxy/enrichment/SPEC_disease.md).
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
		out = append(out, DiseaseNameRef{ID: id, Name: d.Name, Description: diseaseRefHint(d)})
	}
	return out
}

// diseaseRefHint returns a short semantic description for a catalog entry so the
// LLM disambiguation prompt can match on meaning, not just the catalog name
// (e.g. "drought stress" → L07 "Underwatering yellowing"). Prefers
// shortDescription, falls back to symptomAnalysis, truncated to keep the
// ~70-entry prompt bounded.
func diseaseRefHint(d *DiseaseCatalog) string {
	s := strings.TrimSpace(d.ShortDescription)
	if s == "" {
		s = strings.TrimSpace(d.SymptomAnalysis)
	}
	const maxRunes = 140
	if r := []rune(s); len(r) > maxRunes {
		s = strings.TrimSpace(string(r[:maxRunes]))
	}
	return s
}

// diseaseNameAliases maps a disease name to a catalog id when the name is a TRUE
// synonym / spelling variant of an existing catalog disease that exact/fuzzy
// match misses (e.g. "overwatering" → L08 "Waterlogging"). Keys MUST be in
// normalizeDiseaseName form (lowercased, space-collapsed, no " disease"/
// " infection" suffix). Scope (synonyms only, NOT causal names) is documented
// on the map literal below.
var diseaseNameAliases = map[string]string{
	// TRUE synonyms / spelling variants of an EXISTING catalog disease only.
	// Causal / environmental names (drought stress, nutrient deficiency, sunburn,
	// frost…) are intentionally NOT remapped here — they keep their own name and
	// route to disease enrichment's O-series (proxy/enrichment/SPEC_disease.md),
	// rather than being collapsed into a symptom-named catalog entry.
	"overwatering":  "L08", // = Waterlogging (same condition)
	"over-watering": "L08",
	"over watering": "L08",
	"waterlogged":   "L08",
	"water logging": "L08",
	"botrytis":      "L23", // catalog name is "Gray mold (botrytis)"
	"gray mold":     "L23",
	"grey mold":     "L23",
	"gray mould":    "L23",
	"grey mould":    "L23",
	"sooty mould":   "L21", // = "Sooty mold" (spelling)
}

// LookupDiseaseAlias resolves a synonym / spelling-variant disease name to a
// catalog id via diseaseNameAliases, validating the target still exists so a
// stale alias can't return a dangling id.
func (c *ContentIndex) LookupDiseaseAlias(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	key := normalizeDiseaseName(name)
	if key == "" {
		return "", false
	}
	id, ok := diseaseNameAliases[key]
	if !ok {
		return "", false
	}
	if _, exists := c.diseaseByID[id]; !exists {
		return "", false
	}
	return id, true
}

// NormalizeScientificName is the exported form of the package-private
// normalizeScientificName helper. The enrichment package uses it to compute
// the Supabase plants_pending PK in lockstep with catalog lookups — the two
// MUST share the exact same normalization rules (proxy/enrichment/SPEC §9 #1).
func NormalizeScientificName(s string) string {
	return normalizeScientificName(s)
}

// NormalizeScientificNamePrecise is the exported, infraspecific-preserving
// normalizer (var./subsp./cv./f. NOT stripped). The enrichment in-process LRU
// cache keys on it instead of NormalizeScientificName so that sibling varieties
// — the five Brassica oleracea cultivars (AAA0203-AAA0207) being the live case —
// do not alias to a single cache entry. The cache MUST be at least as
// fine-grained as the catalog's precise index (scientificNameToIDPrecise);
// otherwise a Step-0 cache hit for one variety is served to the next variety
// queried within the TTL, masking the per-variety plantId LookupPlantID resolves.
//
// It is deliberately NOT used for the Supabase plants_pending PK, which stays on
// the species-level NormalizeScientificName (the PK source-of-truth — SPEC §9 #1,
// and the intentional cross-variety row sharing of SPEC §8 / §7 "single unique
// key"). Cache keyspace = precise; Supabase keyspace = species.
func NormalizeScientificNamePrecise(s string) string {
	return normalizeScientificNamePrecise(s)
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
// strip the infraspecific rank markers (var./subsp./ssp./cv./f./forma) AND does
// NOT drop a stand-alone ASCII "x" token. It still lowercases, trims, collapses
// whitespace, and replaces the Unicode × with a space. Examples:
//
//	"Brassica oleracea var. italica"   -> "brassica oleracea var. italica"
//	"Brassica oleracea var. acephala"  -> "brassica oleracea var. acephala"
//	"Chrysanthemum morifolium"         -> "chrysanthemum morifolium"
//	"Chrysanthemum x morifolium"       -> "chrysanthemum x morifolium"  (x kept)
//	"Chrysanthemum × morifolium"       -> "chrysanthemum morifolium"
//	"Abelia × grandiflora"             -> "abelia grandiflora"
//
// Why "x" is kept here but dropped in normalizeScientificName: the catalog
// stores some pairs of rows that differ ONLY by the hybrid marker — e.g.
// AAA0324 "Chrysanthemum morifolium" and AAA0325 "Chrysanthemum x morifolium".
// The precise index must preserve that distinction so each row is reachable
// via its own scientific_name. Inputs using the conventional Unicode × still
// collapse to the bare-form row (× -> space, then strings.Fields drops the
// empty token); the species-level fallback (normalizeScientificName) continues
// to drop both forms so a query without any hybrid marker still resolves.
//
// Two consumers: the catalog's precise scientificNameToIDPrecise index, and
// (via the exported NormalizeScientificNamePrecise wrapper) the enrichment
// in-process cache key. It is deliberately NOT used for the Supabase
// plants_pending PK, which stays on normalizeScientificName — per enrichment
// SPEC §9 #1 the PK normalizer is a single source of truth and changing it
// needs an offline migration. This sibling helper leaves that contract untouched.
func normalizeScientificNamePrecise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "×", " ")
	return strings.Join(strings.Fields(s), " ")
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

// NormalizeDiseaseName is the exported wrapper over the disease-name normalizer.
// Disease enrichment uses it for the diseases_pending PK + LRU cache key so they
// stay the single source of truth shared with catalog lookup (SPEC_disease §7).
func NormalizeDiseaseName(s string) string { return normalizeDiseaseName(s) }

// StepByID returns the shared treatment step (S01–S44) for an id, or (nil,false).
func (c *ContentIndex) StepByID(id string) (*SharedStep, bool) {
	if c == nil {
		return nil, false
	}
	s, ok := c.stepByID[id]
	return s, ok
}

// RemedyByID returns the shared home remedy (K01–K15) for an id, or (nil,false).
func (c *ContentIndex) RemedyByID(id string) (*SharedRemedy, bool) {
	if c == nil {
		return nil, false
	}
	r, ok := c.remedyByID[id]
	return r, ok
}

// AllStepRefs returns every shared step as (id, title) — for the disease-
// enrichment prompt (lets the model choose meaningfully) and the server-side
// whitelist set.
func (c *ContentIndex) AllStepRefs() []DiseaseNameRef {
	if c == nil {
		return nil
	}
	out := make([]DiseaseNameRef, 0, len(c.stepByID))
	for id, s := range c.stepByID {
		if s == nil {
			continue
		}
		out = append(out, DiseaseNameRef{ID: id, Name: s.Title})
	}
	return out
}

// AllRemedyRefs returns every shared remedy as (id, title), same use as AllStepRefs.
func (c *ContentIndex) AllRemedyRefs() []DiseaseNameRef {
	if c == nil {
		return nil
	}
	out := make([]DiseaseNameRef, 0, len(c.remedyByID))
	for id, r := range c.remedyByID {
		if r == nil {
			continue
		}
		out = append(out, DiseaseNameRef{ID: id, Name: r.Title})
	}
	return out
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

// hasUsableScientificName reports whether sci is a real, enrichable plant name
// the client can turn into a plant-detail page — a binomial or richer (genus +
// a lowercase-Latin species epithet, allowing a hybrid marker or rank marker in
// between), OR a quoted cultivar (e.g. Rosa 'Brass Band'). It is FALSE for the
// two degenerate engine outputs that create a DEAD Recent-snaps record on iOS:
//   - a blank / whitespace-only name, and
//   - a genus-only name (single token, or "Genus sp."/"Genus spp." with no real
//     epithet).
//
// Rationale: such a suggestion resolves to NO catalog plant_id AND has no
// enrichable scientific name, so iOS stores it (plant_id + scientific_name both
// empty/genus) and its plant-detail can never load — the exact "无法加载植物详情"
// dead record. The Pl@ntNet and Plant.id response converters drop failing
// candidates at the source; HandleIdentify applies a final guard on the chosen
// suggestion (covers the AI-vision paths and any future producer).
func hasUsableScientificName(sci string) bool {
	s := strings.TrimSpace(sci)
	if s == "" {
		return false
	}
	// A quoted cultivar epithet is a real, specific name even without a
	// lowercase-Latin species epithet (ASCII ' or the typographic ’).
	if strings.ContainsAny(s, "'’") {
		return true
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return false // genus only
	}
	// A usable name has at least one real species/infraspecific epithet after
	// the genus. Hybrid markers ("x"/"×") and rank/uncertainty markers are
	// connectors, not epithets — the real epithet follows them.
	for _, t := range fields[1:] {
		if isSpeciesEpithet(t) {
			return true
		}
	}
	return false
}

// isSpeciesEpithet reports whether t is a lowercase-Latin epithet (≥2 letters)
// that is not a rank / uncertainty marker. A bare "x"/"×" hybrid marker (1
// char) and markers like "sp." / "spp." / "cf." / "var." are excluded so a
// genus-only "Genus sp." is not mistaken for a binomial.
func isSpeciesEpithet(t string) bool {
	t = strings.TrimSuffix(t, ".")
	if len(t) < 2 || !isLowerLatin(t) {
		return false
	}
	switch t {
	case "sp", "spp", "cf", "aff", "var", "subsp", "ssp", "forma", "cv":
		return false
	}
	return true
}
