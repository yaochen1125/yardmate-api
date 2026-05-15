package proxy

import "testing"

// loadContentForTests builds a ContentIndex from the embedded JSON files.
// Used by both content_test and handlers_test (diagnose path).
func loadContentForTests(t *testing.T) *ContentIndex {
	t.Helper()
	c, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	return c
}

func TestLoadContent_IndexBuildsAllThreeMaps(t *testing.T) {
	c := loadContentForTests(t)
	if len(c.scientificNameToID) == 0 {
		t.Error("scientificNameToID empty after load")
	}
	if len(c.plantToCommonDiseases) == 0 {
		t.Error("plantToCommonDiseases empty after load")
	}
	if len(c.diseaseByID) == 0 {
		t.Error("diseaseByID empty after load")
	}
	if len(c.diseaseNameToID) == 0 {
		t.Error("diseaseNameToID empty after load")
	}
}

func TestLookupPlantID_ExactMatch(t *testing.T) {
	c := loadContentForTests(t)
	// AAA0001 = Abelia chinensis (first row of plants_index.json).
	id, ok := c.LookupPlantID("Abelia chinensis")
	if !ok {
		t.Fatal("Abelia chinensis should resolve")
	}
	if id != "AAA0001" {
		t.Errorf("plantId = %q, want AAA0001", id)
	}
}

func TestLookupPlantID_CaseInsensitive(t *testing.T) {
	c := loadContentForTests(t)
	id, ok := c.LookupPlantID("ABELIA CHINENSIS")
	if !ok || id != "AAA0001" {
		t.Errorf("case-insensitive miss: ok=%v id=%q", ok, id)
	}
}

func TestLookupPlantID_StripsVarietySuffix(t *testing.T) {
	c := loadContentForTests(t)
	// Fuzzy: drop "var. foo" suffix.
	id, ok := c.LookupPlantID("Abelia chinensis var. ignored")
	if !ok || id != "AAA0001" {
		t.Errorf("var. suffix not stripped: ok=%v id=%q", ok, id)
	}
}

func TestLookupPlantID_NormalizeMultiplicationSign(t *testing.T) {
	c := loadContentForTests(t)
	// "Abelia x grandiflora" is in the index (AAA0005); should also match
	// the unicode multiplication sign form.
	id, ok := c.LookupPlantID("Abelia × grandiflora")
	if !ok {
		t.Fatal("× form did not match")
	}
	if id == "" {
		t.Error("empty id for × form")
	}
}

func TestLookupPlantID_Miss(t *testing.T) {
	c := loadContentForTests(t)
	if _, ok := c.LookupPlantID("Fictional plant"); ok {
		t.Error("unknown plant should miss")
	}
	if _, ok := c.LookupPlantID(""); ok {
		t.Error("empty input should miss")
	}
}

func TestLookupCatalogID_ExactMatch(t *testing.T) {
	c := loadContentForTests(t)
	// diseases.json L20 = "Powdery mildew".
	id, ok := c.LookupCatalogID("Powdery mildew")
	if !ok || id != "L20" {
		t.Errorf("ok=%v id=%q, want L20", ok, id)
	}
}

func TestLookupCatalogID_StripsDiseaseSuffix(t *testing.T) {
	c := loadContentForTests(t)
	id, ok := c.LookupCatalogID("Powdery mildew disease")
	if !ok || id != "L20" {
		t.Errorf("suffix-strip: ok=%v id=%q, want L20", ok, id)
	}
}

func TestLookupCatalogID_CaseInsensitive(t *testing.T) {
	c := loadContentForTests(t)
	id, ok := c.LookupCatalogID("BLACK SPOT")
	if !ok || id != "L02" {
		t.Errorf("case: ok=%v id=%q, want L02", ok, id)
	}
}

func TestLookupCatalogID_Miss(t *testing.T) {
	c := loadContentForTests(t)
	if _, ok := c.LookupCatalogID("Made-up disease that does not exist"); ok {
		t.Error("unknown disease should miss")
	}
}

func TestCommonDiseasesFor_ReturnsList(t *testing.T) {
	c := loadContentForTests(t)
	// AAA0001 (Abelia chinensis) has common_diseases_list starting with R01.
	list := c.CommonDiseasesFor("AAA0001")
	if len(list) == 0 {
		t.Fatal("AAA0001 should have common_diseases_list")
	}
	if list[0] != "R01" {
		t.Errorf("first = %q, want R01", list[0])
	}
}

func TestDiseaseByID_KnownEntry(t *testing.T) {
	c := loadContentForTests(t)
	d, ok := c.DiseaseByID("L01")
	if !ok || d == nil {
		t.Fatal("L01 should exist")
	}
	if d.Name != "Brown spot" {
		t.Errorf("L01 name = %q, want Brown spot", d.Name)
	}
}

func TestNilSafe(t *testing.T) {
	var c *ContentIndex
	if _, ok := c.LookupPlantID("x"); ok {
		t.Error("nil receiver should miss")
	}
	if _, ok := c.LookupCatalogID("x"); ok {
		t.Error("nil receiver should miss")
	}
	if c.CommonDiseasesFor("x") != nil {
		t.Error("nil receiver should return nil slice")
	}
	if _, ok := c.DiseaseByID("x"); ok {
		t.Error("nil receiver should miss")
	}
	if c.AllDiseaseNames() != nil {
		t.Error("nil receiver should return nil slice")
	}
}

func TestAllDiseaseNames_NonEmpty(t *testing.T) {
	c := loadContentForTests(t)
	all := c.AllDiseaseNames()
	if len(all) < 50 {
		t.Errorf("AllDiseaseNames len = %d, want >= 50 (catalog has 70 entries)", len(all))
	}
}

func TestNormalizeScientificName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Monstera deliciosa", "monstera deliciosa"},
		{"MONSTERA  DELICIOSA", "monstera deliciosa"},
		{"Rosa × hybrida", "rosa hybrida"},
		{"Abelia chinensis var. Sweet", "abelia chinensis"},
		{"  trim me  ", "trim me"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeScientificName(tc.in); got != tc.want {
			t.Errorf("normalizeScientificName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeDiseaseName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Powdery mildew", "powdery mildew"},
		{"Powdery mildew disease", "powdery mildew"},
		{"Black spot infection", "black spot"},
		{"  BROWN  SPOT  ", "brown spot"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeDiseaseName(tc.in); got != tc.want {
			t.Errorf("normalizeDiseaseName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
