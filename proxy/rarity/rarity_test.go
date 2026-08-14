package rarity

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{
		MinSample:  1,
		MinSpecies: 0,
		TierCuts:   DefaultTierCuts,
	}
}

var testNow = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

// TestBuildManifestQuantileSpread pins the tier split on 100 species with
// strictly descending counts: exactly 40 common / 30 uncommon / 15 rare /
// 10 very-rare / 5 legendary under the default cuts, most-identified first.
func TestBuildManifestQuantileSpread(t *testing.T) {
	counts := map[string]int64{}
	for i := 0; i < 100; i++ {
		counts[fmt.Sprintf("AAA%04d", i+1)] = int64(1000 - i) // AAA0001 most common
	}
	m := BuildManifest(counts, 1, testNow, testCfg())

	want := map[string]int{
		TierCommon: 40, TierUncommon: 30, TierRare: 15, TierVeryRare: 10, TierLegendary: 5,
	}
	got := map[string]int{}
	for _, e := range m.Species {
		got[e.Tier]++
	}
	for tier, n := range want {
		if got[tier] != n {
			t.Errorf("tier %s: got %d species, want %d (full: %v)", tier, got[tier], n, got)
		}
	}
	// Boundary species: rank 40 (0-based 39) is the last common; rank 41 the
	// first uncommon; rank 96 the first legendary.
	if tier := m.Species["AAA0040"].Tier; tier != TierCommon {
		t.Errorf("rank 40 = %s, want common", tier)
	}
	if tier := m.Species["AAA0041"].Tier; tier != TierUncommon {
		t.Errorf("rank 41 = %s, want uncommon", tier)
	}
	if tier := m.Species["AAA0096"].Tier; tier != TierLegendary {
		t.Errorf("rank 96 = %s, want legendary", tier)
	}
}

// TestBuildManifestTieRunSharesTier: species with equal counts must land in
// the same tier even when the run straddles a cut, collapsing toward the more
// common side.
func TestBuildManifestTieRunSharesTier(t *testing.T) {
	counts := map[string]int64{}
	// 10 species: ranks 1-3 count 100, ranks 4-8 count 50 (run straddles the
	// 0.40 cut at index 4), ranks 9-10 count 10.
	for i := 1; i <= 3; i++ {
		counts[fmt.Sprintf("AAA%04d", i)] = 100
	}
	for i := 4; i <= 8; i++ {
		counts[fmt.Sprintf("AAA%04d", i)] = 50
	}
	for i := 9; i <= 10; i++ {
		counts[fmt.Sprintf("AAA%04d", i)] = 10
	}
	m := BuildManifest(counts, 1, testNow, testCfg())

	// Run of count=50 starts at 0-based index 3 → p=0.30 < 0.40 → ALL common.
	for i := 4; i <= 8; i++ {
		id := fmt.Sprintf("AAA%04d", i)
		if tier := m.Species[id].Tier; tier != TierCommon {
			t.Errorf("%s (count 50, straddling run) = %s, want common", id, tier)
		}
	}
	// The two count=10 species start at index 8 → p=0.80 → rare.
	if tier := m.Species["AAA0009"].Tier; tier != TierRare {
		t.Errorf("AAA0009 = %s, want rare", tier)
	}
}

// TestBuildManifestThreshold: below-MinSample species are omitted from the
// species map but still counted in totalScans (true oneIn denominator).
func TestBuildManifestThreshold(t *testing.T) {
	cfg := testCfg()
	cfg.MinSample = 50
	counts := map[string]int64{
		"AAA0001": 1000,
		"AAA0002": 50, // exactly at threshold → included
		"AAA0003": 49, // below → omitted
	}
	m := BuildManifest(counts, 1, testNow, cfg)

	if m.TotalScans != 1099 {
		t.Errorf("totalScans = %d, want 1099 (must include below-threshold scans)", m.TotalScans)
	}
	if _, ok := m.Species["AAA0003"]; ok {
		t.Error("below-threshold species must be omitted")
	}
	if _, ok := m.Species["AAA0002"]; !ok {
		t.Error("at-threshold species must be included")
	}
	// oneIn uses the full total: round(1099/50) = 22.
	if oneIn := m.Species["AAA0002"].OneIn; oneIn != 22 {
		t.Errorf("AAA0002 oneIn = %d, want 22", oneIn)
	}
}

// TestBuildManifestMinSpeciesFloor: with fewer eligible species than
// MinSpecies the species map publishes empty (hide, don't mislabel a
// head-only population), while totalScans stays real.
func TestBuildManifestMinSpeciesFloor(t *testing.T) {
	cfg := testCfg()
	cfg.MinSpecies = 5
	counts := map[string]int64{"AAA0001": 100, "AAA0002": 60, "AAA0003": 30}
	m := BuildManifest(counts, 3, testNow, cfg)

	if len(m.Species) != 0 {
		t.Errorf("species = %v, want empty under the MinSpecies floor", m.Species)
	}
	if m.TotalScans != 190 {
		t.Errorf("totalScans = %d, want 190", m.TotalScans)
	}
	if m.Version != 3 {
		t.Errorf("version = %d, want 3", m.Version)
	}
}

// TestBuildManifestOneIn pins rounding and the floor at 1.
func TestBuildManifestOneIn(t *testing.T) {
	counts := map[string]int64{
		"AAA0001": 900, // round(1000/900) = 1
		"AAA0002": 60,  // round(1000/60) = round(16.67) = 17
		"AAA0003": 40,  // round(1000/40) = 25
	}
	m := BuildManifest(counts, 1, testNow, testCfg())
	wants := map[string]int64{"AAA0001": 1, "AAA0002": 17, "AAA0003": 25}
	for id, want := range wants {
		if got := m.Species[id].OneIn; got != want {
			t.Errorf("%s oneIn = %d, want %d", id, got, want)
		}
	}
}

// TestBuildManifestDropsJunk: the AAA0000 sentinel and non-positive counts
// must vanish entirely — no tier, no totalScans contribution.
func TestBuildManifestDropsJunk(t *testing.T) {
	counts := map[string]int64{
		"AAA0000": 500, // unknown sentinel — junk even if a legacy row exists
		"":        7,
		"AAA0001": 0,
		"AAA0002": -3,
		"AAA0003": 10,
	}
	m := BuildManifest(counts, 1, testNow, testCfg())
	if m.TotalScans != 10 {
		t.Errorf("totalScans = %d, want 10 (junk must not count)", m.TotalScans)
	}
	if len(m.Species) != 1 {
		t.Errorf("species = %v, want only AAA0003", m.Species)
	}
}

// TestBuildManifestEmpty: no counts at all → valid empty artifact.
func TestBuildManifestEmpty(t *testing.T) {
	m := BuildManifest(nil, 1, testNow, testCfg())
	if m.TotalScans != 0 || len(m.Species) != 0 {
		t.Errorf("empty input → totalScans=%d species=%v", m.TotalScans, m.Species)
	}
}

// TestManifestJSONShape locks the wire contract: exact field names, RFC3339
// generatedAt, and `"species": {}` (never null) when empty.
func TestManifestJSONShape(t *testing.T) {
	counts := map[string]int64{"AAA0876": 100, "AAA0001": 140000}
	m := BuildManifest(counts, 3, testNow, testCfg())
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`"version":3`,
		`"generatedAt":"2026-08-13T00:00:00Z"`,
		`"totalScans":140100`,
		`"species":`,
		`"AAA0876":{"tier":"uncommon","oneIn":1401}`, // idx 1 of 2 → p=0.5 → uncommon
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wire JSON missing %s in %s", want, s)
		}
	}

	empty := BuildManifest(nil, 1, testNow, testCfg())
	body, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if !strings.Contains(string(body), `"species":{}`) {
		t.Errorf("empty species must marshal as {}, got %s", body)
	}
}

func TestParseTierCuts(t *testing.T) {
	cases := []struct {
		in      string
		want    [4]float64
		wantErr bool
	}{
		{"", DefaultTierCuts, false},
		{"  ", DefaultTierCuts, false},
		{"0.40,0.70,0.85,0.95", [4]float64{0.40, 0.70, 0.85, 0.95}, false},
		{"0.3, 0.5, 0.7, 0.9", [4]float64{0.3, 0.5, 0.7, 0.9}, false},
		{"0.4,0.7,0.85", [4]float64{}, true},           // wrong count
		{"0.4,0.7,0.85,0.95,0.99", [4]float64{}, true}, // wrong count
		{"0.4,0.7,x,0.95", [4]float64{}, true},         // non-numeric
		{"0.4,0.3,0.85,0.95", [4]float64{}, true},      // not ascending
		{"0.4,0.7,0.85,1.0", [4]float64{}, true},       // out of (0,1)
		{"0,0.7,0.85,0.95", [4]float64{}, true},        // out of (0,1)
	}
	for _, c := range cases {
		got, err := ParseTierCuts(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}
