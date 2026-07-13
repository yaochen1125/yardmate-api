// Package rosererank holds the pure types + decision logic for the rose
// cultivar rerank step of /v1/identify. It imports NOTHING from proxy
// (dependency inversion — see SPEC.md §1.5): the proxy package builds the
// candidates from its PlantDetail entries, runs the vision call, and applies the
// result; rosererank only owns the wire types and the pure Decide function so
// they can be unit-tested in isolation.
package rosererank

// RoseCandidate is one catalog rose (species or cultivar) offered to the vision
// model as a rerank option. Plain data built by proxy from PlantDetail entries
// whose genus == "Rosa".
type RoseCandidate struct {
	PlantID        string   // YardMate catalog id, e.g. "AAA1136"
	ScientificName string   // e.g. "Rosa 'About Face'" or "Rosa rugosa"
	CommonName     string   // e.g. "About Face"
	FlowerColor    []string // e.g. ["orange","yellow"]
	FoliageColor   []string // e.g. ["purple","dark-green"] — the discriminator for dark-leaved cultivars (Aeonium 'Zwartkop', Sambucus 'Black Lace') where FlowerColor is identical to the species
	Description    string   // catalog description, truncated to ~30 words by the caller
}

// RoseMatch is one ranked cultivar the vision model returned.
type RoseMatch struct {
	PlantID    string  `json:"plant_id"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// RoseRerankResult is the parsed strict-json_schema vision response.
type RoseRerankResult struct {
	CultivarCertain bool        `json:"cultivar_certain"`
	Matches         []RoseMatch `json:"matches"`
}
