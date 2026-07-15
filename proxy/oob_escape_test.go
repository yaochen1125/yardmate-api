package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// (d) Regression — bloom-tiebreak interaction (review r3584457237). With
// BLOOM_TIEBREAK_ENABLED (server default) the tiebreak swaps bestIdx to a
// lower-confidence blooming near-tie BEFORE the escape runs; the OOB margin must
// compare against the STRONGEST in-catalog hit (pre-tiebreak), not the demoted
// winner. Real margin 0.90−0.41 = 0.49 (< 0.50) must NOT escape even though the
// post-tiebreak 0.90−0.40 = 0.50 would have.
func TestHandleIdentify_OOBEscape_BloomTiebreak_MarginUsesStrongestCatalogHit(t *testing.T) {
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	// Discover an in-catalog pair where exactly one blooms in some month (mirrors
	// TestHandleIdentify_BloomTiebreak): A does NOT bloom (strongest catalog hit,
	// 0.41), B blooms (near-tie, 0.40).
	names := content.CatalogScientificNames()
	roundTrips := func(id, sci string) bool {
		got, ok := resolvePlantID(content, sci)
		return ok && got == id
	}
	bloomMonths := func(id string) []int {
		if d, ok := content.LookupFullDetail(id); ok && d != nil {
			return d.BloomMonthsNorth
		}
		return nil
	}
	var (
		sciA      string // non-blooming strongest catalog hit (0.41)
		idB, sciB string // blooming near-tie, lower confidence (0.40)
		month     time.Month
		found     bool
	)
	for bID, bSci := range names {
		bm := bloomMonths(bID)
		if len(bm) == 0 || !roundTrips(bID, bSci) {
			continue
		}
		m := time.Month(bm[0])
		for aID, aSci := range names {
			if aID == bID || !roundTrips(aID, aSci) || bloomsInMonth(content, aID, m) {
				continue
			}
			sciA, idB, sciB, month, found = aSci, bID, bSci, m, true
			break
		}
		if found {
			break
		}
	}
	if !found {
		t.Skip("no suitable bloom-differing catalog pair found")
	}
	orig := nowMonth
	nowMonth = func() time.Month { return month }
	defer func() { nowMonth = orig }()

	// results[0] out-of-catalog 0.90 (different genus); A 0.41 (non-blooming,
	// strongest catalog hit); B 0.40 (blooming near-tie). bloom tiebreak swaps
	// bestIdx A→B; the escape must still compare 0.90−0.41 = 0.49 < 0.50 → NO escape.
	pnBody := fmt.Sprintf(`{
  "bestMatch": "Fakeplant nonexistus",
  "results": [
    {"score": 0.90, "species": {"scientificNameWithoutAuthor": "Fakeplant nonexistus", "scientificName": "Fakeplant nonexistus Auth.", "commonNames": []}},
    {"score": 0.41, "species": {"scientificNameWithoutAuthor": %q, "scientificName": %q, "commonNames": ["A"]}},
    {"score": 0.40, "species": {"scientificNameWithoutAuthor": %q, "scientificName": %q, "commonNames": ["B"]}}
  ],
  "remainingIdentificationRequests": 480
}`, sciA, sciA, sciB, sciB)
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, pnBody)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}
	// bloom ON (default) + oobEscape ON; everything else off.
	h := HandleIdentify(pn, nil, content, nil, nil, nil, false, false, false, true, false, true, nil)

	rec := doCascadeReq(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var result IdentifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Escape must NOT fire (margin uses A, 0.49 < 0.50) → result is the bloom
	// winner B (in-catalog, PlantID set), not the out-of-catalog top.
	if result.Suggestions[0].ScientificName != sciB {
		t.Errorf("Suggestions[0] = %q, want %q (bloom winner; escape must not fire — margin uses strongest hit %q)",
			result.Suggestions[0].ScientificName, sciB, sciA)
	}
	if result.Suggestions[0].PlantID == nil || *result.Suggestions[0].PlantID != idB {
		t.Errorf("Suggestions[0].PlantID = %v, want %s (in-catalog, not escaped)", result.Suggestions[0].PlantID, idB)
	}
}
