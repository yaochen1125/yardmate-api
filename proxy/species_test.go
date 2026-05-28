package proxy

import "testing"

func TestSpeciesBinomial(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare trinomial subspecies", "Triteleia ixioides anilina", "Triteleia ixioides"},
		{"bare trinomial (catalog Ceanothus)", "Ceanothus griseus horizontalis", "Ceanothus griseus"},
		{"rank marker subsp.", "Rosa chinensis subsp. spontanea", "Rosa chinensis"},
		{"rank marker var.", "Brassica oleracea var. capitata", "Brassica oleracea"},
		{"already binomial", "Monstera deliciosa", "Monstera deliciosa"},
		{"cultivar unchanged", "Rosa 'Knock Out'", "Rosa 'Knock Out'"},
		{"cultivar with species unchanged", "Aeonium arboreum 'Zwartkop'", "Aeonium arboreum 'Zwartkop'"},
		{"author citation unchanged", "Rosa chinensis Jacq.", "Rosa chinensis Jacq."},
		{"capitalised 2nd token unchanged", "Graptoveria Fred Ives", "Graptoveria Fred Ives"},
		{"hybrid marker unchanged", "Abelia × grandiflora", "Abelia × grandiflora"},
		{"ASCII hybrid x (Abelia)", "Abelia x grandiflora", "Abelia x grandiflora"},
		{"ASCII hybrid x (Citrus)", "Citrus x paradisi", "Citrus x paradisi"},
		{"extra whitespace normalised", "  Rosa   chinensis  ", "Rosa chinensis"},
		{"single token", "Rosa", "Rosa"},
		{"rank marker without species epithet (defense)", "Eucalyptus f. xxx", "Eucalyptus f. xxx"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := speciesBinomial(tc.in); got != tc.want {
			t.Errorf("%s: speciesBinomial(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestIsLowerLatin(t *testing.T) {
	lower := []string{"anilina", "horizontalis", "chinensis"}
	notLower := []string{"", "Jacq.", "Fred", "var.", "x1", "Zwartkop"}
	for _, w := range lower {
		if !isLowerLatin(w) {
			t.Errorf("isLowerLatin(%q) = false, want true", w)
		}
	}
	for _, w := range notLower {
		if isLowerLatin(w) {
			t.Errorf("isLowerLatin(%q) = true, want false", w)
		}
	}
}

func TestLookupCommonName_CatalogHit(t *testing.T) {
	c := loadContentForTests(t)
	// AAA0001 = Abelia chinensis / "Chinese Abelia" (first row of plants_index).
	id, ok := c.LookupPlantID("Abelia chinensis")
	if !ok {
		t.Fatal("Abelia chinensis should resolve to a plantId")
	}
	name, ok := c.LookupCommonName(id)
	if !ok || name == "" {
		t.Fatalf("LookupCommonName(%q) = (%q,%v), want a non-empty curated name", id, name, ok)
	}
}

func TestLookupCommonName_NilSafeAndMiss(t *testing.T) {
	var nilIndex *ContentIndex
	if _, ok := nilIndex.LookupCommonName("AAA0001"); ok {
		t.Error("nil index should return false")
	}
	c := loadContentForTests(t)
	if _, ok := c.LookupCommonName("ZZZ9999"); ok {
		t.Error("unknown plantId should return false")
	}
	if _, ok := c.LookupCommonName(""); ok {
		t.Error("empty plantId should return false")
	}
}

func TestResolvePlantID_TwoPass(t *testing.T) {
	c := loadContentForTests(t)

	// pass 1: exact catalog hit (binomial in the 1522 catalog).
	if id, ok := resolvePlantID(c, "Abelia chinensis"); !ok || id != "AAA0001" {
		t.Errorf("exact = (%q,%v), want (AAA0001,true)", id, ok)
	}

	// pass 2: bare-trinomial subspecies whose SPECIES is in the catalog.
	// LookupPlantID(original) misses (key has 3 tokens, no row); speciesBinomial
	// collapses to "Abelia chinensis" → LookupPlantID hits AAA0001. This is the
	// fix for PR #24 review finding 2 (subspecies-to-species merge must work
	// inside the catalog-preference selection too, not just in step 7b).
	if id, ok := resolvePlantID(c, "Abelia chinensis spontanea"); !ok || id != "AAA0001" {
		t.Errorf("species fallback = (%q,%v), want (AAA0001,true)", id, ok)
	}

	// miss: neither original nor species is in catalog.
	if _, ok := resolvePlantID(c, "Zzzz nonexistent plantii"); ok {
		t.Error("out-of-catalog should miss")
	}

	// nil content is safe (LookupPlantID is nil-safe).
	if _, ok := resolvePlantID(nil, "Abelia chinensis"); ok {
		t.Error("nil content should miss")
	}
}
