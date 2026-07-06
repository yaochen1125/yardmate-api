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

// TestHandleIdentify_NamelessGuard_GenusOnlyVision_Sentinel exercises the final
// 7d guard: engine returns zero candidates and AI vision recovers only a
// GENUS-ONLY guess ("Bellis"). Without the guard this would surface as an
// ai-raw-oob suggestion the client stores as a dead record; the guard converts
// it to the Unknown sentinel, which iOS refuses to record.
func TestHandleIdentify_NamelessGuard_GenusOnlyVision_Sentinel(t *testing.T) {
	vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"Bellis\",\"common_names\":[\"Daisy\"],\"confidence\":0.4}"}}]}`)
	}))
	defer vsrv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}

	h, cleanup := newCascadeHandlerWithVision(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound) // PlantNet valid no-match (zero)
			_, _ = io.WriteString(w, cannedPlantNetNoMatch)
		}, nil, vision)
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	assertUnknownSentinel(t, rec.Body.Bytes())
}

func scientificNames(s []Suggestion) []string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[i].ScientificName
	}
	return out
}
