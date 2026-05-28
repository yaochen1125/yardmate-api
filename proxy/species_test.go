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
