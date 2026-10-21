package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHasUsableScientificName pins the predicate that gates whether a
// suggestion can become a loadable plant detail. FALSE for the two degenerate
// engine outputs that create a dead Recent-snaps record on iOS: a blank name
// and a genus-only name. TRUE for binomials, hybrids, infraspecific names, and
// quoted cultivars.
func TestHasUsableScientificName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Unusable — blank.
		{"", false},
		{"   ", false},
		{"\t\n", false},
		// Unusable — genus only.
		{"Bellis", false},
		{"Rosa", false},
		{"Rosa sp.", false},
		{"Rosa spp.", false},
		{"Quercus cf.", false},
		{"Daisy", false}, // a bare common name is a single token → genus-only shape
		// Usable — real binomials.
		{"Bellis perennis", true},
		{"Monstera deliciosa", true},
		{"Leucanthemum vulgare", true},
		// Usable — hybrids (ASCII x and Unicode ×): the epithet follows the marker.
		{"Abelia x grandiflora", true},
		{"Rosa × odorata", true},
		// Usable — infraspecific.
		{"Ceanothus griseus horizontalis", true},
		{"Salvia officinalis subsp. lavandulifolia", true},
		// Usable — hyphenated Latin epithets (16 such species in the catalog).
		{"Agave victoriae-reginae", true},
		{"Arctostaphylos uva-ursi", true},
		{"Athyrium filix-femina", true},
		{"Hibiscus rosa-sinensis", true},
		// Unusable — malformed hyphen (empty segment) is not a real epithet.
		{"Rosa -ursi", false},
		{"Rosa uva-", false},
		// Usable — quoted cultivar (no lowercase-Latin epithet but a real name).
		{"Rosa 'Brass Band'", true},
		{"Hosta 'Blue Angel'", true},
	}
	for _, c := range cases {
		if got := hasUsableScientificName(c.in); got != c.want {
			t.Errorf("hasUsableScientificName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestPlantNetToIdentifyResult_DropsNamelessAndGenusOnly asserts the Pl@ntNet
// converter filters blank + genus-only candidates at the source so they never
// reach the client, while keeping valid binomials in order.
func TestPlantNetToIdentifyResult_DropsNamelessAndGenusOnly(t *testing.T) {
	const raw = `{
      "bestMatch": "Bellis perennis",
      "results": [
        {"score":0.9,"species":{"scientificNameWithoutAuthor":"Bellis perennis","scientificName":"Bellis perennis L.","commonNames":["Lawn Daisy"]}},
        {"score":0.5,"species":{"scientificNameWithoutAuthor":"","scientificName":"","commonNames":["Daisy"]}},
        {"score":0.4,"species":{"scientificNameWithoutAuthor":"Rosa","scientificName":"Rosa L.","commonNames":["Rose"]}},
        {"score":0.3,"species":{"scientificNameWithoutAuthor":"Rosa sp.","scientificName":"Rosa sp.","commonNames":[]}},
        {"score":0.2,"species":{"scientificNameWithoutAuthor":"Leucanthemum vulgare","scientificName":"Leucanthemum vulgare Lam.","commonNames":["Oxeye Daisy"]}}
      ],
      "remainingIdentificationRequests": 100
    }`
	var apiResp plantNetAPIResponse
	if err := json.Unmarshal([]byte(raw), &apiResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := apiResp.toIdentifyResult()
	want := []string{"Bellis perennis", "Leucanthemum vulgare"}
	if len(got.Suggestions) != len(want) {
		t.Fatalf("len(Suggestions) = %d (%+v), want %d (%v)",
			len(got.Suggestions), scientificNames(got.Suggestions), len(want), want)
	}
	for i, w := range want {
		if got.Suggestions[i].ScientificName != w {
			t.Errorf("Suggestions[%d].ScientificName = %q, want %q", i, got.Suggestions[i].ScientificName, w)
		}
	}
}

// TestPlantIDToIdentifyResult_DropsNamelessAndGenusOnly is the Plant.id analogue:
// the converter guards on details.scientific_name (what downstream resolves).
func TestPlantIDToIdentifyResult_DropsNamelessAndGenusOnly(t *testing.T) {
	const raw = `{
      "result": {
        "is_plant": {"probability":0.97,"binary":true},
        "classification": {"suggestions": [
          {"name":"Bellis perennis","probability":0.9,"details":{"common_names":["Lawn Daisy"],"scientific_name":"Bellis perennis"}},
          {"name":"Daisy","probability":0.5,"details":{"common_names":["Daisy"],"scientific_name":""}},
          {"name":"Rosa","probability":0.4,"details":{"common_names":["Rose"],"scientific_name":"Rosa"}},
          {"name":"Leucanthemum vulgare","probability":0.2,"details":{"common_names":["Oxeye Daisy"],"scientific_name":"Leucanthemum vulgare"}}
        ]}
      }
    }`
	var apiResp plantIDAPIResponse
	if err := json.Unmarshal([]byte(raw), &apiResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := apiResp.toIdentifyResult()
	want := []string{"Bellis perennis", "Leucanthemum vulgare"}
	if len(got.Suggestions) != len(want) {
		t.Fatalf("len(Suggestions) = %d (%+v), want %d (%v)",
			len(got.Suggestions), scientificNames(got.Suggestions), len(want), want)
	}
	for i, w := range want {
		if got.Suggestions[i].ScientificName != w {
			t.Errorf("Suggestions[%d].ScientificName = %q, want %q", i, got.Suggestions[i].ScientificName, w)
		}
	}
}

// genusGuardHandler — PlantNet valid no-match (zero candidates) + AI vision
// answering sci, so the 7d final guard sees the AI guess as Suggestions[0].
// extraIndex (optional) is appended to the embedded plants_index so a test can
// add a catalog GENUS entry (the embed snapshot predates genus promotion).
func genusGuardHandler(t *testing.T, sci string, extraIndex []map[string]string) (http.Handler, func()) {
	t.Helper()
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"is_plant": true, "scientific_name": sci, "common_names": []string{"Daisy"}, "confidence": 0.4})
		outer, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(inner)}}}})
		_, _ = w.Write(outer)
	}))
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // PlantNet valid no-match (zero)
		_, _ = io.WriteString(w, cannedPlantNetNoMatch)
	}))
	pn := &PlantNetClient{APIKey: "test-key", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	if len(extraIndex) > 0 {
		var rows []map[string]any
		if err := json.Unmarshal(plantsIndexRaw, &rows); err != nil {
			t.Fatalf("unmarshal embedded index: %v", err)
		}
		for _, e := range extraIndex {
			rows = append(rows, map[string]any{"id": e["id"], "scientific_name": e["scientific_name"], "common_name": e["common_name"]})
		}
		raw, _ := json.Marshal(rows)
		if _, err := content.ReloadCatalogNames(raw, "test"); err != nil {
			t.Fatalf("ReloadCatalogNames: %v", err)
		}
	}
	h := HandleIdentify(pn, nil, content, vision, nil, nil, false, false, false, false, false, false, false, PlantIDFallbackAlways, nil, nil)
	return h, func() { pnSrv.Close(); vsrv.Close() }
}

func decodeIdentify(t *testing.T, rec *httptest.ResponseRecorder) IdentifyResult {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

// TestHandleIdentify_NamelessGuard_GenusOnlyVision_KeptAsGenus — the 7d guard
// used to drop a GENUS-ONLY AI guess to the Unknown sentinel (genus detail pages
// could not load, 2026-07). Genus pages load now (catalog genus entries + out-
// of-catalog genus enrichment), so a genus answer is kept as the bare genus
// (2026-10-21: "Begonia spp." → "can't identify" cost a paying user).
func TestHandleIdentify_NamelessGuard_GenusOnlyVision_KeptAsGenus(t *testing.T) {
	for _, sci := range []string{"Bellis", "Bellis sp.", "Bellis spp."} {
		t.Run(sci, func(t *testing.T) {
			h, cleanup := genusGuardHandler(t, sci, nil)
			defer cleanup()
			got := decodeIdentify(t, doCascadeReq(t, h))
			if len(got.Suggestions) != 1 {
				t.Fatalf("Suggestions = %+v, want 1 genus suggestion", got.Suggestions)
			}
			s0 := got.Suggestions[0]
			if s0.ScientificName != "Bellis" {
				t.Errorf("scientific_name = %q, want bare genus %q", s0.ScientificName, "Bellis")
			}
			if s0.PlantID != nil {
				t.Errorf("plant_id = %q, want nil (no catalog genus entry → out-of-catalog enrichment)", *s0.PlantID)
			}
			if !got.IsPlant {
				t.Error("IsPlant = false, want true (not the Unknown sentinel)")
			}
		})
	}
}

// A genus that IS a catalog entry resolves to its plant_id + curated name.
func TestHandleIdentify_NamelessGuard_GenusOnlyVision_CatalogGenusEntry(t *testing.T) {
	h, cleanup := genusGuardHandler(t, "Bellis spp.", []map[string]string{
		{"id": "AAA9999", "scientific_name": "Bellis", "common_name": "Daisies"},
	})
	defer cleanup()
	got := decodeIdentify(t, doCascadeReq(t, h))
	if len(got.Suggestions) != 1 {
		t.Fatalf("Suggestions = %+v, want 1", got.Suggestions)
	}
	s0 := got.Suggestions[0]
	if s0.PlantID == nil || *s0.PlantID != "AAA9999" {
		t.Fatalf("plant_id = %v, want AAA9999 (catalog genus entry)", s0.PlantID)
	}
	if s0.ScientificName != "Bellis" || s0.Name != "Daisies" {
		t.Errorf("sci/name = %q/%q, want Bellis/Daisies", s0.ScientificName, s0.Name)
	}
}

// Blank names, family names and generic words are still not enrichable plants →
// Unknown sentinel, exactly as before.
func TestHandleIdentify_NamelessGuard_NonGenusVision_Sentinel(t *testing.T) {
	for _, sci := range []string{"", "   ", "Asteraceae", "Compositae", "Unknown", "Succulent"} {
		t.Run(sci, func(t *testing.T) {
			h, cleanup := genusGuardHandler(t, sci, nil)
			defer cleanup()
			rec := doCascadeReq(t, h)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			assertUnknownSentinel(t, rec.Body.Bytes())
		})
	}
}

func TestGenusOnlyName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Begonia", "Begonia"},
		{"Begonia spp.", "Begonia"},
		{"Begonia sp.", "Begonia"},
		{"  Potamogeton   sp. ", "Potamogeton"},
		{"Begonia rex", ""},  // a binomial is not genus-only
		{"begonia", ""},      // not Capitalised
		{"Rosa 'Peace'", ""}, // cultivar — handled as usable elsewhere
		{"Asteraceae", ""},   // family
		{"Lamiales", ""},     // order
		{"Gramineae", ""},    // conserved family name
		{"Unknown", ""},      // generic word
		{"Ab", ""},           // too short
		{"Begonia cf.", ""},  // only sp/spp may follow
		{"Plantae", ""},
		{"Fungi", ""},
		{"Basidiomycota", ""},
		{"Agaricomycetes", ""},
		{"Angiospermae", ""},
		{"Eudicots", ""},
		{"Orchid", ""},
		{"Palm", ""},
		{"Ivy", ""},
		{"Lily", ""},
		{"Succulents", ""},
		{"Iris", "Iris"}, // English word that IS a genus
		{"Aster", "Aster"},
		{"", ""},
	}
	for _, c := range cases {
		if got := genusOnlyName(c.in); got != c.want {
			t.Errorf("genusOnlyName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func scientificNames(s []Suggestion) []string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[i].ScientificName
	}
	return out
}

// 库内属级条目写成 "Petunia spp."（真实库 AAA1591）时，GPT 答 "Petunia sp." / "Petunia"
// 也要对上，并补上库内俗名，不能另生成一个重复的库外 Petunia。
func TestHandleIdentify_NamelessGuard_GenusMatchesSppCatalogEntry(t *testing.T) {
	for _, sci := range []string{"Petunia sp.", "Petunia", "Petunia spp."} {
		t.Run(sci, func(t *testing.T) {
			h, cleanup := genusGuardHandler(t, sci, nil)
			defer cleanup()
			got := decodeIdentify(t, doCascadeReq(t, h))
			s0 := got.Suggestions[0]
			if s0.PlantID == nil || *s0.PlantID != "AAA1591" {
				t.Fatalf("plant_id = %v, want AAA1591 (catalog 'Petunia spp.')", s0.PlantID)
			}
			if len(s0.CommonNames) == 0 || s0.CommonNames[0] != "Garden Petunia" {
				t.Errorf("common_names = %v, want curated 'Garden Petunia' first", s0.CommonNames)
			}
			// "Petunia spp." 已被既有的 ai-catalog-recovery 精确命中（不经 genus-keep），
			// Name 沿用那条路径的既有行为；只断言 genus-keep 路径会把 Name 换成库内名。
			if sci != "Petunia spp." && s0.Name != "Garden Petunia" {
				t.Errorf("name = %q, want curated 'Garden Petunia'", s0.Name)
			}
		})
	}
}
