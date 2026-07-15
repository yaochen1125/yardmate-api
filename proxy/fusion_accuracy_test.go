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

// --- P1C #3: engine↔GPT agreement boost -------------------------------------

// TestBoostedConfidence covers the pure boost math: it RAISES toward the floor/
// max, clamps to the cap, and NEVER returns below the engine's own confidence.
func TestBoostedConfidence(t *testing.T) {
	cases := []struct {
		name              string
		engine, gpt, want float64
	}{
		// Both low but agreeing → pulled up to the 0.90 floor.
		{"both low → floor", 0.40, 0.50, agreementBoostFloor},
		// Max of the two sits between floor and cap → that max wins.
		{"max within band", 0.88, 0.95, 0.95},
		// Above the cap → clamped to 0.99 (still a raise vs the 0.90 engine).
		{"clamp to cap", 0.90, 0.999, agreementBoostCap},
		// Engine already above the cap → never lowered, kept as-is.
		{"never below engine (engine>cap)", 0.995, 0.40, 0.995},
		// GPT lower than engine, engine below floor → floor still raises it.
		{"gpt weaker, engine below floor", 0.70, 0.30, agreementBoostFloor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := boostedConfidence(c.engine, c.gpt)
			if got != c.want {
				t.Fatalf("boostedConfidence(%.3f, %.3f) = %.3f, want %.3f", c.engine, c.gpt, got, c.want)
			}
			if got < c.engine {
				t.Fatalf("boost lowered confidence: got %.3f < engine %.3f", got, c.engine)
			}
		})
	}
}

// TestSpeciesKey verifies the species-level key used to decide agreement:
// casing, an infraspecific (var./subsp.) tail and the hybrid marker must NOT
// block a match, while genuinely different species must NOT match. (Per the
// speciesBinomial contract a quoted CULTIVAR is deliberately kept distinct — the
// boost stays conservative and simply doesn't fire on a cultivar-vs-species
// mismatch, which is a safe no-op.)
func TestSpeciesKey(t *testing.T) {
	if a, b := speciesKey("Monstera deliciosa"), speciesKey("monstera deliciosa var. borsigiana"); a != b {
		t.Errorf("infraspecific tail should collapse to the species key: %q vs %q", a, b)
	}
	if a, b := speciesKey("  Abelia   Chinensis  "), speciesKey("abelia chinensis"); a != b {
		t.Errorf("casing/whitespace should not change species key: %q vs %q", a, b)
	}
	if a, b := speciesKey("Abelia × grandiflora"), speciesKey("Abelia x grandiflora"); a != b {
		t.Errorf("hybrid marker should normalize equal: %q vs %q", a, b)
	}
	if a, b := speciesKey("Monstera deliciosa"), speciesKey("Monstera adansonii"); a == b {
		t.Errorf("different species must not share a key: both %q", a)
	}
	if speciesKey("   ") != "" {
		t.Errorf("blank input should yield empty key")
	}
}

// TestHandleIdentify_AgreementBoost is the end-to-end guard: an in-catalog
// engine hit whose species the GPT arbiter independently confirms gets its
// confidence RAISED (plant unchanged); disagreement leaves it untouched; and the
// AGREEMENT_BOOST_ENABLED=false kill-switch restores the original behavior.
func TestHandleIdentify_AgreementBoost(t *testing.T) {
	const engineConf = 0.55 // in-catalog but low; below any override/skip gate
	// Pl@ntNet returns Abelia chinensis (catalog AAA0001) at 0.55.
	pnBody := fmt.Sprintf(`{
	  "bestMatch": "Abelia chinensis",
	  "results": [
	    {"score": %v, "species": {
	      "scientificNameWithoutAuthor": "Abelia chinensis",
	      "scientificName": "Abelia chinensis R.Br.",
	      "commonNames": ["Chinese Abelia"]}}
	  ],
	  "remainingIdentificationRequests": 480
	}`, engineConf)
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, pnBody)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}

	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}

	// visionArbiter returns a single canned IdentifyPlant JSON.
	visionArbiter := func(sci string, conf float64) *VisionClient {
		body := fmt.Sprintf(`{"choices":[{"message":{"content":"{\"is_plant\":true,\"scientific_name\":\"%s\",\"common_names\":[\"x\"],\"confidence\":%v}"}}]}`, sci, conf)
		vsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(vsrv.Close)
		return &VisionClient{APIKey: "k", Endpoint: vsrv.URL, Model: "t", HTTP: vsrv.Client()}
	}

	doReq := func(vision *VisionClient, boostEnabled bool) IdentifyResult {
		// rose + disambig OFF, bloom OFF; only the agreement boost varies.
		h := HandleIdentify(pn, nil, content, vision, nil, nil, false, false, boostEnabled, false, false, false, false, nil)
		body, ct := buildMultipart(t, "image", jpegMagic)
		req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Device-Install-Id", testUUID)
		req.Header.Set("X-App-Version", "1.1.1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		var out IdentifyResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	assertAbelia := func(t *testing.T, out IdentifyResult) {
		t.Helper()
		if len(out.Suggestions) == 0 || out.Suggestions[0].PlantID == nil || *out.Suggestions[0].PlantID != "AAA0001" {
			t.Fatalf("want top plant_id AAA0001 (unchanged), got %+v", out.Suggestions)
		}
	}

	// (a) GPT agrees on the species → confidence boosted, plant unchanged.
	agree := doReq(visionArbiter("Abelia chinensis", 0.62), true)
	assertAbelia(t, agree)
	if agree.Suggestions[0].Confidence <= engineConf {
		t.Errorf("agreement boost: confidence not raised: got %.3f, want > %.3f", agree.Suggestions[0].Confidence, engineConf)
	}
	if agree.Suggestions[0].Confidence != agreementBoostFloor {
		t.Errorf("agreement boost: want floor %.2f, got %.3f", agreementBoostFloor, agree.Suggestions[0].Confidence)
	}

	// (b) GPT names an out-of-catalog, different species → no agreement, no boost.
	disagree := doReq(visionArbiter("Quercus roburxyz", 0.80), true)
	assertAbelia(t, disagree)
	if disagree.Suggestions[0].Confidence != engineConf {
		t.Errorf("disagreement: confidence must stay %.3f, got %.3f", engineConf, disagree.Suggestions[0].Confidence)
	}

	// (c) Kill-switch OFF: even with GPT agreeing, confidence is untouched.
	off := doReq(visionArbiter("Abelia chinensis", 0.62), false)
	assertAbelia(t, off)
	if off.Suggestions[0].Confidence != engineConf {
		t.Errorf("kill-switch off: confidence must stay %.3f, got %.3f", engineConf, off.Suggestions[0].Confidence)
	}
}

// --- P1C #5: bloom-month tiebreak -------------------------------------------

// TestBloomTiebreak covers the pure tiebreak decision over in-catalog candidates.
func TestBloomTiebreak(t *testing.T) {
	// idx0 is the confidence best in every case; only its neighbours vary.
	t.Run("near-tie prefers the blooming candidate", func(t *testing.T) {
		cands := []bloomTiebreakCand{
			{origIdx: 0, plantID: "A", confidence: 0.60, bloomsMonth: false},
			{origIdx: 1, plantID: "B", confidence: 0.57, bloomsMonth: true},
		}
		got, changed := bloomTiebreak(cands, 0, bloomTiebreakEpsilon)
		if !changed || got.plantID != "B" {
			t.Fatalf("want switch to B, got %+v changed=%v", got, changed)
		}
	})
	t.Run("clear winner is never overridden", func(t *testing.T) {
		cands := []bloomTiebreakCand{
			{origIdx: 0, plantID: "A", confidence: 0.80, bloomsMonth: false},
			{origIdx: 1, plantID: "B", confidence: 0.60, bloomsMonth: true}, // 0.20 gap > epsilon
		}
		got, changed := bloomTiebreak(cands, 0, bloomTiebreakEpsilon)
		if changed || got.plantID != "A" {
			t.Fatalf("clear winner A must stay, got %+v changed=%v", got, changed)
		}
	})
	t.Run("best already blooms → no change", func(t *testing.T) {
		cands := []bloomTiebreakCand{
			{origIdx: 0, plantID: "A", confidence: 0.60, bloomsMonth: true},
			{origIdx: 1, plantID: "B", confidence: 0.59, bloomsMonth: true},
		}
		got, changed := bloomTiebreak(cands, 0, bloomTiebreakEpsilon)
		if changed || got.plantID != "A" {
			t.Fatalf("best already blooming must stay, got %+v changed=%v", got, changed)
		}
	})
	t.Run("no bloom data anywhere → no change", func(t *testing.T) {
		cands := []bloomTiebreakCand{
			{origIdx: 0, plantID: "A", confidence: 0.60, bloomsMonth: false},
			{origIdx: 1, plantID: "B", confidence: 0.58, bloomsMonth: false},
		}
		got, changed := bloomTiebreak(cands, 0, bloomTiebreakEpsilon)
		if changed || got.plantID != "A" {
			t.Fatalf("no bloom data must stay, got %+v changed=%v", got, changed)
		}
	})
	t.Run("highest-confidence near-tie blooming candidate wins", func(t *testing.T) {
		cands := []bloomTiebreakCand{
			{origIdx: 0, plantID: "A", confidence: 0.60, bloomsMonth: false},
			{origIdx: 1, plantID: "B", confidence: 0.57, bloomsMonth: true},
			{origIdx: 2, plantID: "C", confidence: 0.58, bloomsMonth: true},
		}
		got, changed := bloomTiebreak(cands, 0, bloomTiebreakEpsilon)
		if !changed || got.plantID != "C" {
			t.Fatalf("want highest near-tie bloomer C, got %+v changed=%v", got, changed)
		}
	})
}

// TestHandleIdentify_BloomTiebreak drives the tiebreak end-to-end against real
// catalog bloom data: it discovers two in-catalog plants where exactly one
// blooms in a chosen month, feeds them as near-tie engine candidates (the
// NON-blooming one at higher confidence), pins nowMonth, and asserts the
// blooming plant is chosen when enabled and the confidence best when disabled.
func TestHandleIdentify_BloomTiebreak(t *testing.T) {
	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}

	// Discover a suitable (blooming plantB, non-blooming plantA, month) triple.
	names := content.CatalogScientificNames() // id -> scientific name
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
		idA, sciA string // non-blooming-in-month (higher confidence)
		idB, sciB string // blooming-in-month (near-tie, lower confidence)
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
			if aID == bID || !roundTrips(aID, aSci) {
				continue
			}
			if bloomsInMonth(content, aID, m) {
				continue // A must NOT bloom in the chosen month
			}
			idA, sciA, idB, sciB, month, found = aID, aSci, bID, bSci, m, true
			break
		}
		if found {
			break
		}
	}
	if !found {
		t.Skip("no suitable bloom-differing catalog pair found")
	}

	// Pin the wall clock to the discovered month for the duration of the test.
	orig := nowMonth
	nowMonth = func() time.Month { return month }
	defer func() { nowMonth = orig }()

	// Engine returns A (0.60, non-blooming, best by confidence) then B (0.58,
	// blooming, near-tie). Without the tiebreak A wins; with it, B wins.
	pnBody := fmt.Sprintf(`{
	  "bestMatch": %q,
	  "results": [
	    {"score": 0.60, "species": {"scientificNameWithoutAuthor": %q, "scientificName": %q, "commonNames": ["A"]}},
	    {"score": 0.58, "species": {"scientificNameWithoutAuthor": %q, "scientificName": %q, "commonNames": ["B"]}}
	  ],
	  "remainingIdentificationRequests": 480
	}`, sciA, sciA, sciA, sciB, sciB)
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, pnBody)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}

	doReq := func(bloomEnabled bool) IdentifyResult {
		// vision nil (no arbiter needed), rose/disambig/boost OFF.
		h := HandleIdentify(pn, nil, content, nil, nil, nil, false, false, false, bloomEnabled, false, false, false, nil)
		body, ct := buildMultipart(t, "image", jpegMagic)
		req := httptest.NewRequest(http.MethodPost, "/v1/identify", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Device-Install-Id", testUUID)
		req.Header.Set("X-App-Version", "1.1.1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
		var out IdentifyResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Suggestions) == 0 || out.Suggestions[0].PlantID == nil {
			t.Fatalf("no in-catalog top suggestion: %+v", out.Suggestions)
		}
		return out
	}

	// ON: near-tie → the blooming plant B is promoted to [0].
	if got := *doReq(true).Suggestions[0].PlantID; got != idB {
		t.Errorf("bloom tiebreak ON: want blooming %s (month=%d), got %s (A=%s)", idB, int(month), got, idA)
	}
	// OFF (kill-switch): the confidence best A stays at [0].
	if got := *doReq(false).Suggestions[0].PlantID; got != idA {
		t.Errorf("bloom tiebreak OFF: want confidence-best %s, got %s", idA, got)
	}
}
