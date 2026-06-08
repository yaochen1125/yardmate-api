package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Mapping-recall boost coverage (post-narrowing): the alias table holds TRUE
// synonyms only, and LLM disambiguation feeds the description-enriched catalog.
// Genuinely out-of-catalog names map to nil — their detail is handled by disease
// enrichment (SPEC_disease.md), NOT force-mapped here. All offline (httptest).

func TestLookupDiseaseAlias(t *testing.T) {
	c := loadContentForTests(t)
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"overwatering", "L08", true},    // = Waterlogging
		{"Over-Watering", "L08", true},   // case-folded by normalizeDiseaseName
		{"  waterlogged  ", "L08", true}, // trimmed
		{"botrytis", "L23", true},        // catalog "Gray mold (botrytis)"
		{"drought stress", "", false},    // causal name → NOT remapped (routes to enrichment)
		{"totally unknown thing", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		id, ok := c.LookupDiseaseAlias(tc.in)
		if ok != tc.wantOK || id != tc.wantID {
			t.Errorf("LookupDiseaseAlias(%q) = (%q,%v), want (%q,%v)", tc.in, id, ok, tc.wantID, tc.wantOK)
		}
	}
	// No dangling aliases: every target id must exist in the catalog.
	for name, id := range diseaseNameAliases {
		if _, ok := c.DiseaseByID(id); !ok {
			t.Errorf("alias %q → %q not in catalog", name, id)
		}
	}
}

func TestAllDiseaseNames_CarryDescription(t *testing.T) {
	c := loadContentForTests(t)
	refs := c.AllDiseaseNames()
	if len(refs) == 0 {
		t.Fatal("AllDiseaseNames empty")
	}
	withDesc := 0
	for _, r := range refs {
		if strings.TrimSpace(r.Description) != "" {
			withDesc++
		}
	}
	if withDesc == 0 {
		t.Error("no DiseaseNameRef carries a Description — semantic hint not wired into the prompt")
	}
}

func TestMapCatalogID_Cascade(t *testing.T) {
	c := loadContentForTests(t)

	// Alias hit (true synonym) — resolves without any vision client.
	if id := mapCatalogID(context.Background(), "overwatering", c, nil); id == nil || *id != "L08" {
		t.Errorf("alias overwatering: got %s, want L08", derefOr(id))
	}
	// Out-of-catalog name + nil vision → nil (enrichment handles detail separately).
	if id := mapCatalogID(context.Background(), "drought stress", c, nil); id != nil {
		t.Errorf("out-of-catalog drought stress (nil vision): got %s, want nil", derefOr(id))
	}
	// LLM answers NONE → nil (no forced pass / L08 net anymore).
	vNone, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"NONE"}}]}`)
	})
	defer srv.Close()
	if id := mapCatalogID(context.Background(), "some alien blight", c, vNone); id != nil {
		t.Errorf("LLM NONE: got %s, want nil", derefOr(id))
	}
	// Nil content → nil.
	if id := mapCatalogID(context.Background(), "anything", nil, nil); id != nil {
		t.Errorf("nil content: got %s, want nil", derefOr(id))
	}
}

func TestDisambiguatePromptCarriesDescription(t *testing.T) {
	var body string
	c, srv := newTestVisionClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, cannedOpenAIPick)
	})
	defer srv.Close()
	refs := []DiseaseNameRef{{ID: "L20", Name: "Powdery mildew", Description: "white powdery coating on leaves"}}
	_, _ = c.DisambiguateDiseaseName(context.Background(), "x", refs)
	if !strings.Contains(body, "white powdery coating on leaves") {
		t.Errorf("prompt missing catalog description: %s", body)
	}
}

func derefOr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
