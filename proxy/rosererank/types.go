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

// Tier is how much the rerank verdict may influence the identify result.
type Tier int

const (
	// TierNone: no usable cultivar signal — keep the species result verbatim.
	TierNone Tier = iota
	// TierCertain: the model is confident AND the top surviving (real, in-catalog)
	// candidate clears MinConfidenceFloor — REPLACE the species suggestions with the
	// matched cultivars (the original V1 behaviour).
	TierCertain
	// TierGuess: the model is NOT confident (or the top real candidate sits below
	// MinConfidenceFloor) but there ARE plausible in-catalog candidates — surface
	// them as low-confidence "possibly" guesses ALONGSIDE the species result, never
	// as a verdict. This is the honest middle ground that lets a distinguishable-in-
	// principle cultivar (e.g. 'Queen of Sweden') reach the user for confirmation
	// instead of silently collapsing to the bare genus (SPEC §2.4 / §6).
	TierGuess
)

// String renders the tier for logs/telemetry (SPEC §6).
func (t Tier) String() string {
	switch t {
	case TierCertain:
		return "certain"
	case TierGuess:
		return "guess"
	default:
		return "none"
	}
}

// Outcome is the result of Decide: which tier applies + the surviving (real,
// deduped, ≤MaxMatches) candidates. Matches is empty when Tier == TierNone.
type Outcome struct {
	Tier    Tier
	Matches []RoseMatch
}
