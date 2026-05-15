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
	// scientificNameToID maps normalized scientific_name to plantId.
	// e.g. "monstera deliciosa" -> "AAA1234".
	scientificNameToID map[string]string

	// plantToCommonDiseases maps plantId to its ordered common_diseases_list
	// (catalog ids). Used by the F-option-2 异常 fallback in /v1/diagnose.
	plantToCommonDiseases map[string][]string

	// diseaseNameToID maps normalized disease catalog name to catalog id.
	// e.g. "powdery mildew" -> "L20".
	diseaseNameToID map[string]string

	// diseaseByID gives the full catalog entry by id.
	diseaseByID map[string]*DiseaseCatalog
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
	sci := make(map[string]string, len(plants))
	for _, p := range plants {
		key := normalizeScientificName(p.ScientificName)
		if key == "" || p.ID == "" {
			continue
		}
		sci[key] = p.ID
	}

	var details []struct {
		ID                 string   `json:"id"`
		CommonDiseasesList []string `json:"common_diseases_list"`
	}
	if err := json.Unmarshal(plantsDetailRaw, &details); err != nil {
		return nil, fmt.Errorf("content: plants_detail: %w", err)
	}
	pdis := make(map[string][]string, len(details))
	for _, d := range details {
		if d.ID == "" {
			continue
		}
		pdis[d.ID] = d.CommonDiseasesList
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
		scientificNameToID:    sci,
		plantToCommonDiseases: pdis,
		diseaseNameToID:       dnam,
		diseaseByID:           diseaseFile.Diseases,
	}, nil
}

// LookupPlantID maps a Plant.id-reported scientific name to a YardMate
// plantId. Match is case-insensitive and tolerates variety / cultivar /
// subspecies suffixes (`var.`, `cv.`, `subsp.`, `f.`, `×`).
//
// Returns ("", false) on miss — iOS detail page must tolerate plantId=null
// (renders Plant.id-only data without YardMate cross-reference).
func (c *ContentIndex) LookupPlantID(scientificName string) (string, bool) {
	if c == nil {
		return "", false
	}
	key := normalizeScientificName(scientificName)
	if key == "" {
		return "", false
	}
	if id, ok := c.scientificNameToID[key]; ok {
		return id, true
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
