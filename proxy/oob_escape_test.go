package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- #21 High-confidence out-of-catalog escape from rule B (OOB_ESCAPE_ENABLED) ---
//
// Rule B normally promotes ANY in-catalog candidate over a higher-confidence
// out-of-catalog top (no threshold). The escape is a NARROW exception: keep the
// engine's ORIGINAL top when it is out-of-catalog AND ≥ oobEscapeMinConfidence
// AND at least oobEscapeMinMargin more confident than the best in-catalog hit
// AND a DIFFERENT genus. GPT arbiter is observe-only (vision nil here → no
// arbiter), so these tests exercise the engine-only decision path.
//
// catalog fixture (shared with the CatalogPref tests): Abelia chinensis →
// AAA0001 is a name guaranteed present in the embedded catalog.

// newOOBEscapeHandler wires HandleIdentify with a Pl@ntNet fake and ONLY the
// OOB-escape flag ON; vision is nil so the decision rests on the engine
// candidates alone.
func newOOBEscapeHandler(t *testing.T, plantNetUp http.HandlerFunc) (http.Handler, func()) {
	t.Helper()
	pnSrv := httptest.NewServer(plantNetUp)
	pnClient := &PlantNetClient{
		APIKey: "test-key", Endpoint: pnSrv.URL,
		Lang: "en", NbResults: 10, HTTP: pnSrv.Client(),
	}
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	// Flags: rose/disambig/agreement/bloom/geoPrior OFF, oobEscape ON.
	h := HandleIdentify(pnClient, nil, content, nil, nil, nil, false, false, false, false, false, true, nil)
	return h, pnSrv.Close
}

// (a) Escape FIRES: confident (≥0.90) out-of-catalog top of a DIFFERENT genus
// than the far-weaker in-catalog hit → engine top kept out-of-catalog
// (PlantID nil → iOS enrichment), the weak catalog near-miss is NOT promoted.
func TestHandleIdentify_OOBEscape_ConfidentDifferentGenus_KeepsOutOfCatalog(t *testing.T) {
	// Top out-of-catalog "Fakeplant nonexistus" (0.92); in-catalog "Abelia
	// chinensis" (0.30). Different genus, margin 0.62 ≥ 0.50, top ≥ 0.90.
	const pn = `{
  "bestMatch": "Fakeplant nonexistus",
  "results": [
    {"score": 0.92, "species": {"scientificNameWithoutAuthor": "Fakeplant nonexistus",
      "scientificName": "Fakeplant nonexistus Auth.", "commonNames": []}},
    {"score": 0.30, "species": {"scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.", "commonNames": ["Chinese Abelia"]}}
  ],
  "remainingIdentificationRequests": 480
}`
	h, cleanup := newOOBEscapeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, pn)
	})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Suggestions[0].ScientificName != "Fakeplant nonexistus" {
		t.Errorf("Suggestions[0] = %q, want Fakeplant nonexistus (escape keeps confident out-of-catalog top)",
			result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID != nil {
		t.Errorf("Suggestions[0].PlantID = %v, want nil (out-of-catalog → enrichment)",
			*result.Suggestions[0].PlantID)
	}
}

// (b) Escape does NOT fire on SAME genus (engine wobbling within a genus — the
// California-poppy failure mode) → rule B still promotes the reviewed catalog
// species even though the out-of-catalog top is more confident.
func TestHandleIdentify_OOBEscape_SameGenus_StillPrefersCatalog(t *testing.T) {
	// Top out-of-catalog "Abelia nonexistus" (0.92) is the SAME genus (Abelia)
	// as in-catalog "Abelia chinensis" (0.30) → gate 4 (different genus) fails.
	const pn = `{
  "bestMatch": "Abelia nonexistus",
  "results": [
    {"score": 0.92, "species": {"scientificNameWithoutAuthor": "Abelia nonexistus",
      "scientificName": "Abelia nonexistus Auth.", "commonNames": []}},
    {"score": 0.30, "species": {"scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.", "commonNames": ["Chinese Abelia"]}}
  ],
  "remainingIdentificationRequests": 480
}`
	h, cleanup := newOOBEscapeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, pn)
	})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Suggestions[0].ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestions[0] = %q, want Abelia chinensis (same-genus → no escape, rule B holds)",
			result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", result.Suggestions[0].PlantID)
	}
}

// (c) Escape does NOT fire on a NARROW confidence margin (< oobEscapeMinMargin)
// even across genera → the in-catalog hit is close enough to still be preferred.
func TestHandleIdentify_OOBEscape_NarrowMargin_StillPrefersCatalog(t *testing.T) {
	// Top out-of-catalog 0.92, in-catalog 0.50 → margin 0.42 < 0.50 → no escape.
	const pn = `{
  "bestMatch": "Fakeplant nonexistus",
  "results": [
    {"score": 0.92, "species": {"scientificNameWithoutAuthor": "Fakeplant nonexistus",
      "scientificName": "Fakeplant nonexistus Auth.", "commonNames": []}},
    {"score": 0.50, "species": {"scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.", "commonNames": ["Chinese Abelia"]}}
  ],
  "remainingIdentificationRequests": 480
}`
	h, cleanup := newOOBEscapeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, pn)
	})
	defer cleanup()

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Suggestions[0].ScientificName != "Abelia chinensis" {
		t.Errorf("Suggestions[0] = %q, want Abelia chinensis (narrow margin → no escape)",
			result.Suggestions[0].ScientificName)
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != "AAA0001" {
		t.Errorf("Suggestions[0].PlantID = %v, want AAA0001", result.Suggestions[0].PlantID)
	}
}
