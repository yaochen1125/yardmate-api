// Package rarity aggregates the Plantdex per-species identify counters
// (dex_identify_daily, enrichment migration 012) into the dex/rarity.json
// artifact consumed by the iOS Plantdex (contract:
// yardmate-swiftui docs/releases/v1/main-navigation/home/plantdex/plantdex.md
// §数据契约). Tiers are species-percentile buckets over the identify-share
// distribution, precomputed here so the client only reads. See SPEC.md.
package rarity

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tier names — exact wire strings from the plantdex contract. Order matters:
// index 0 is the most common bucket, index 4 the rarest.
const (
	TierCommon    = "common"
	TierUncommon  = "uncommon"
	TierRare      = "rare"
	TierVeryRare  = "very-rare"
	TierLegendary = "legendary"
)

// unknownSentinelPlantID mirrors proxy.unknownSentinelPlantID (AAA0000, the
// "Plantae incognita" miss placeholder). The identify counter already refuses
// to record it; filtering here too keeps a stray legacy row from ever gaining
// a rarity tier or polluting totalScans.
const unknownSentinelPlantID = "AAA0000"

// Defaults for the vault-tunable knobs (see SPEC.md §Config).
var DefaultTierCuts = [4]float64{0.40, 0.70, 0.85, 0.95}

const (
	DefaultMinSample  = 50             // below → species omitted (client hides rarity)
	DefaultMinSpecies = 100            // fewer eligible species → publish empty species map
	DefaultInterval   = 24 * time.Hour // publish cadence
)

// Config carries the aggregation + publish knobs, resolved from the secrets
// vault by main.buildRarityPublisher and normalized by NewPublisher.
type Config struct {
	// Prefix is the R2 key prefix the artifact publishes under:
	// {Prefix}/dex/rarity.json. "content" for prod, "content-staging" for
	// staging (the server must self-select — the shared bucket has no
	// per-environment credentials, staging-runbook known boundary).
	Prefix string
	// MinSample is the minimum all-time scan count for a species to appear in
	// the artifact (cold-start gate; absent species → client hides rarity).
	MinSample int64
	// MinSpecies is the minimum number of eligible species for tiers to be
	// published at all. Below it the species map publishes empty: with only a
	// handful of (necessarily head/common) species eligible, percentile tiers
	// would brand the least common of them "legendary" — hiding rarity beats
	// mislabeling it during cold start.
	MinSpecies int
	// TierCuts are the ascending species-percentile boundaries splitting
	// common / uncommon / rare / very-rare / legendary.
	TierCuts [4]float64
	// Interval is the periodic publish cadence.
	Interval time.Duration
	// AdminToken gates POST /internal/rarity/rebuild (empty → route not
	// registered; the handler additionally denies all when empty).
	AdminToken string
}

// SpeciesEntry is one species' rarity payload in the artifact.
type SpeciesEntry struct {
	Tier  string `json:"tier"`
	OneIn int64  `json:"oneIn"`
}

// Manifest is the dex/rarity.json wire shape (plantdex contract §rarity.json).
type Manifest struct {
	Version     int                     `json:"version"`
	GeneratedAt string                  `json:"generatedAt"`
	TotalScans  int64                   `json:"totalScans"`
	Species     map[string]SpeciesEntry `json:"species"`
}

// ParseTierCuts parses a "0.40,0.70,0.85,0.95"-style vault string into tier
// boundaries. Empty input yields the defaults; anything malformed (wrong
// count, non-numeric, out of (0,1), not strictly ascending) errors so the
// caller can WARN and fall back to defaults rather than publish nonsense.
func ParseTierCuts(s string) ([4]float64, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultTierCuts, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return DefaultTierCuts, fmt.Errorf("want 4 comma-separated cuts, got %d", len(parts))
	}
	var cuts [4]float64
	prev := 0.0
	for i, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return DefaultTierCuts, fmt.Errorf("cut %d: %v", i, err)
		}
		if v <= prev || v >= 1 {
			return DefaultTierCuts, fmt.Errorf("cuts must be strictly ascending within (0,1); cut %d = %v", i, v)
		}
		cuts[i] = v
		prev = v
	}
	return cuts, nil
}

// tierForPercentile maps a species-rank percentile (fraction of eligible
// species strictly more identified than this one, 0 = most identified) to a
// tier via the ascending cuts.
func tierForPercentile(p float64, cuts [4]float64) string {
	switch {
	case p < cuts[0]:
		return TierCommon
	case p < cuts[1]:
		return TierUncommon
	case p < cuts[2]:
		return TierRare
	case p < cuts[3]:
		return TierVeryRare
	default:
		return TierLegendary
	}
}

// BuildManifest turns raw per-species scan totals into the publishable
// artifact. Pure and deterministic (caller supplies version + timestamp) so
// the quantile/threshold behaviour is directly unit-testable.
//
// Semantics (SPEC.md §Algorithm):
//   - totalScans = Σ all species counts — including below-threshold species —
//     so oneIn reflects true share even for barely-eligible species. The
//     AAA0000 sentinel and non-positive counts are dropped as junk.
//   - A species is eligible iff count ≥ MinSample. Fewer than MinSpecies
//     eligible → empty species map (cold-start: hide, don't mislabel).
//   - Tiers: eligible species sorted by count desc; a species' percentile is
//     startOfItsTieRun/len(eligible), so equal counts always share a tier and
//     a run straddling a cut collapses toward the more common tier.
//   - oneIn = round(totalScans / count), floored at 1.
func BuildManifest(counts map[string]int64, version int, generatedAt time.Time, cfg Config) Manifest {
	m := Manifest{
		Version:     version,
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Species:     map[string]SpeciesEntry{},
	}

	type sc struct {
		id string
		n  int64
	}
	eligible := make([]sc, 0, len(counts))
	for id, n := range counts {
		if id == "" || id == unknownSentinelPlantID || n <= 0 {
			continue
		}
		m.TotalScans += n
		if n >= cfg.MinSample {
			eligible = append(eligible, sc{id: id, n: n})
		}
	}
	if len(eligible) < cfg.MinSpecies || len(eligible) == 0 {
		return m
	}

	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].n != eligible[j].n {
			return eligible[i].n > eligible[j].n
		}
		return eligible[i].id < eligible[j].id
	})

	total := float64(m.TotalScans)
	n := float64(len(eligible))
	runStart := 0
	for i, s := range eligible {
		if s.n != eligible[runStart].n {
			runStart = i
		}
		oneIn := int64(math.Round(total / float64(s.n)))
		if oneIn < 1 {
			oneIn = 1
		}
		m.Species[s.id] = SpeciesEntry{
			Tier:  tierForPercentile(float64(runStart)/n, cfg.TierCuts),
			OneIn: oneIn,
		}
	}
	return m
}
