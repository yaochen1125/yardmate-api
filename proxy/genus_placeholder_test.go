package proxy

import (
	"encoding/json"
	"testing"
)

// contentWithExtraRows loads the embedded catalog and hot-reloads it with extra
// plants_index rows appended — the embed snapshot predates most genus-level
// promotions, so a test adds the genus row it needs (same path prod uses when
// autopilot promotes one).
func contentWithExtraRows(t *testing.T, extra ...map[string]any) *ContentIndex {
	t.Helper()
	c := loadContentForTests(t)
	var rows []map[string]any
	if err := json.Unmarshal(plantsIndexRaw, &rows); err != nil {
		t.Fatalf("unmarshal embedded plants_index: %v", err)
	}
	rows = append(rows, extra...)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if _, err := c.ReloadCatalogNames(raw, "test"); err != nil {
		t.Fatalf("ReloadCatalogNames: %v", err)
	}
	return c
}

func TestGenusLevelKey(t *testing.T) {
	cases := map[string]string{
		"Calathea":           "calathea",
		"Calathea species":   "calathea",
		"CALATHEA SPECIES":   "calathea",
		"calathea sp.":       "calathea",
		"Calathea sp":        "calathea",
		"Rosa spp.":          "rosa",
		"Salvia spp":         "salvia",
		"Begonia ssp.":       "begonia", // cut as a rank marker by normalizeScientificName
		"Begonia ssp":        "begonia",
		"  Calathea   sp.  ": "calathea",
		"Petunia spp.":       "petunia",
		// Not genus level.
		"Prunus speciosa":  "",
		"Prunus spinosa":   "",
		"Calathea ornata":  "",
		"Calathea sp. nov": "",
		"Rosa 'Peace'":     "",
		"AAA1758":          "",
		"":                 "",
		"   ":              "",
	}
	for in, want := range cases {
		if got := genusLevelKey(in); got != want {
			t.Errorf("genusLevelKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikeCatalogPlantID(t *testing.T) {
	yes := []string{"AAA1758", "aaa1758", "AaA0001", " AAA1758 ", "\tAAA1758\n", "AAA10000", "ZZZ9001"}
	no := []string{
		"", "AAA175", "AA1758", "AAAA1758", "AAA17a8", "AAA 1758", "AAA1758x",
		"1758AAA", "Rosa", "Calathea species", "Abelia chinensis", "ÄAA1758", "L20",
	}
	for _, s := range yes {
		if !LooksLikeCatalogPlantID(s) {
			t.Errorf("LooksLikeCatalogPlantID(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if LooksLikeCatalogPlantID(s) {
			t.Errorf("LooksLikeCatalogPlantID(%q) = true, want false", s)
		}
	}
}

// With a genus-level catalog entry "Calathea", every placeholder spelling
// resolves to it — while the Calathea SPECIES rows keep their own ids.
func TestLookupPlantID_PlaceholderEpithet_GenusEntry(t *testing.T) {
	base := loadContentForTests(t)
	ornataID, ok := base.LookupPlantID("Calathea ornata")
	if !ok {
		t.Fatal("precondition: Calathea ornata should be in the embedded catalog")
	}
	c := contentWithExtraRows(t, map[string]any{"id": "ZZZ9101", "scientific_name": "Calathea", "common_name": "Calatheas"})
	for _, q := range []string{
		"Calathea species", "calathea sp.", "Calathea spp", "Calathea spp.", "CALATHEA SPECIES",
		"  Calathea   sp. ", "Calathea ssp.", "Calathea ssp", "Calathea",
	} {
		if id, ok := c.LookupPlantID(q); !ok || id != "ZZZ9101" {
			t.Errorf("LookupPlantID(%q) = (%q,%v), want (ZZZ9101,true)", q, id, ok)
		}
	}
	if id, ok := c.LookupPlantID("Calathea ornata"); !ok || id != ornataID {
		t.Errorf("species row hijacked: LookupPlantID(Calathea ornata) = (%q,%v), want (%q,true)", id, ok, ornataID)
	}
}

// No genus-level entry → unchanged: the placeholder name misses (the caller goes
// to enrichment), even though the catalog holds six Calathea species.
func TestLookupPlantID_PlaceholderEpithet_NoGenusEntry_Misses(t *testing.T) {
	c := loadContentForTests(t)
	for _, q := range []string{"Calathea species", "Calathea sp.", "Calathea spp.", "Calathea"} {
		if id, ok := c.LookupPlantID(q); ok {
			t.Errorf("LookupPlantID(%q) = %q, want miss (no genus-level Calathea row)", q, id)
		}
	}
}

// The catalog may spell its genus row "Petunia spp." (AAA1591): any placeholder
// spelling — or the bare genus — lands on it.
func TestLookupPlantID_PlaceholderEpithet_SppCatalogSpelling(t *testing.T) {
	c := loadContentForTests(t)
	for _, q := range []string{"Petunia species", "Petunia sp.", "petunia spp", "Petunia spp.", "Petunia"} {
		if id, ok := c.LookupPlantID(q); !ok || id != "AAA1591" {
			t.Errorf("LookupPlantID(%q) = (%q,%v), want (AAA1591,true)", q, id, ok)
		}
	}
}

// A real epithet that merely looks like a placeholder is never folded to the
// genus row, and species rows are untouched.
func TestLookupPlantID_RealEpithetNotFolded(t *testing.T) {
	base := loadContentForTests(t)
	aviumID, ok := base.LookupPlantID("Prunus avium")
	if !ok {
		t.Fatal("precondition: Prunus avium should be in the embedded catalog")
	}
	c := contentWithExtraRows(t, map[string]any{"id": "ZZZ9102", "scientific_name": "Prunus", "common_name": "Cherries"})
	for _, q := range []string{"Prunus speciosa", "Prunus spinosa", "Prunus sp. nov"} {
		if id, ok := c.LookupPlantID(q); ok {
			t.Errorf("LookupPlantID(%q) = %q, want miss (real epithet, not a placeholder)", q, id)
		}
	}
	if id, ok := c.LookupPlantID("Prunus species"); !ok || id != "ZZZ9102" {
		t.Errorf("LookupPlantID(Prunus species) = (%q,%v), want (ZZZ9102,true)", id, ok)
	}
	if id, ok := c.LookupPlantID("Prunus avium"); !ok || id != aviumID {
		t.Errorf("LookupPlantID(Prunus avium) = (%q,%v), want (%q,true)", id, ok, aviumID)
	}
}

// identify's two-pass resolver inherits tier 3.
func TestResolvePlantID_PlaceholderEpithet(t *testing.T) {
	c := contentWithExtraRows(t, map[string]any{"id": "ZZZ9101", "scientific_name": "Calathea", "common_name": "Calatheas"})
	for _, q := range []string{"Calathea species", "calathea sp.", "Calathea spp"} {
		if id, ok := resolvePlantID(c, q); !ok || id != "ZZZ9101" {
			t.Errorf("resolvePlantID(%q) = (%q,%v), want (ZZZ9101,true)", q, id, ok)
		}
	}
	if id, ok := resolvePlantID(loadContentForTests(t), "Calathea species"); ok {
		t.Errorf("resolvePlantID without genus row = %q, want miss", id)
	}
}

// End to end: the AI answer "Calathea species" (the real 2026-10-21 case) comes
// back with the genus entry's plant_id; without a genus row it is returned
// out-of-catalog exactly as before.
func TestHandleIdentify_PlaceholderEpithet_ResolvesGenusEntry(t *testing.T) {
	h, cleanup := genusGuardHandler(t, "Calathea species", []map[string]string{
		{"id": "ZZZ9101", "scientific_name": "Calathea", "common_name": "Calatheas"},
	})
	defer cleanup()
	got := decodeIdentify(t, doCascadeReq(t, h))
	if len(got.Suggestions) == 0 {
		t.Fatal("no suggestions")
	}
	s0 := got.Suggestions[0]
	if s0.PlantID == nil || *s0.PlantID != "ZZZ9101" {
		t.Fatalf("plant_id = %v, want ZZZ9101 (genus entry); s0=%+v", s0.PlantID, s0)
	}
	// Shown like the genus-keep path: bare genus + curated name first.
	if s0.ScientificName != "Calathea" || s0.Name != "Calatheas" {
		t.Errorf("sci/name = %q/%q, want Calathea/Calatheas", s0.ScientificName, s0.Name)
	}
	if len(s0.CommonNames) == 0 || s0.CommonNames[0] != "Calatheas" {
		t.Errorf("common_names = %v, want curated 'Calatheas' first", s0.CommonNames)
	}
}

func TestHandleIdentify_PlaceholderEpithet_NoGenusEntryUnchanged(t *testing.T) {
	h, cleanup := genusGuardHandler(t, "Calathea species", nil)
	defer cleanup()
	got := decodeIdentify(t, doCascadeReq(t, h))
	if len(got.Suggestions) == 0 {
		t.Fatal("no suggestions")
	}
	s0 := got.Suggestions[0]
	if s0.PlantID != nil {
		t.Errorf("plant_id = %q, want nil (no genus entry → enrichment)", *s0.PlantID)
	}
	if s0.ScientificName != "Calathea species" {
		t.Errorf("scientific_name = %q, want unchanged %q", s0.ScientificName, "Calathea species")
	}
}
