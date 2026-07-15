package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVisionKNNAgreesWithDecision(t *testing.T) {
	pid := "AAA0262"
	resp := func(topID string, in bool) *VisionKNNResponse {
		return &VisionKNNResponse{InCatalog: in, Candidates: []VisionKNNCandidate{{CatalogID: topID, VisionSim: 0.9}}}
	}
	cases := []struct {
		name string
		s0   *Suggestion
		r    *VisionKNNResponse
		want bool
	}{
		{"agrees", &Suggestion{PlantID: &pid}, resp("AAA0262", true), true},
		{"different top id", &Suggestion{PlantID: &pid}, resp("AAA0263", true), false},
		{"out-of-catalog verdict", &Suggestion{PlantID: &pid}, resp("AAA0262", false), false},
		{"decision out-of-catalog (nil PlantID)", &Suggestion{PlantID: nil}, resp("AAA0262", true), false},
		{"no candidates", &Suggestion{PlantID: &pid}, &VisionKNNResponse{InCatalog: true}, false},
		{"nil resp", &Suggestion{PlantID: &pid}, nil, false},
		{"nil s0", nil, resp("AAA0262", true), false},
	}
	for _, c := range cases {
		if got := visionKNNAgreesWithDecision(c.s0, c.r); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestVisionKNNIdentifyParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vision/identify" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"catalog_id":"AAA0262","vision_sim":0.85}],"nn_sim":0.85,"in_catalog":true,"in_catalog_confidence":0.61,"model":"bioclip-2"}`))
	}))
	defer srv.Close()

	c := NewVisionKNNClient(srv.URL)
	got, err := c.Identify(context.Background(), []byte("fakejpegbytes"), "image/jpeg")
	if err != nil {
		t.Fatalf("Identify err: %v", err)
	}
	if !got.InCatalog || len(got.Candidates) != 1 || got.Candidates[0].CatalogID != "AAA0262" || got.NNSim != 0.85 {
		t.Fatalf("bad parse: %+v", got)
	}
}

func TestVisionKNNIdentifyNon200FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("index not loaded"))
	}))
	defer srv.Close()

	c := NewVisionKNNClient(srv.URL)
	if _, err := c.Identify(context.Background(), []byte("x"), "image/jpeg"); err == nil {
		t.Fatal("expected error on 503 (handler must fail open on error)")
	}
}

func TestNewVisionKNNClientDefaultEndpoint(t *testing.T) {
	if c := NewVisionKNNClient(""); c.Endpoint != defaultVisionKNNEndpoint {
		t.Fatalf("empty endpoint should default to %s, got %s", defaultVisionKNNEndpoint, c.Endpoint)
	}
}

func TestVisionKNNBoostedConfidence(t *testing.T) {
	cases := []struct {
		name         string
		cur, knn     float64
		wantMin, max float64 // want in (wantMin, max]; wantMin==want for exact
		exact        bool
	}{
		{"knn below cur → unchanged", 0.80, 0.60, 0.80, 0.80, true},
		{"knn equal cur → unchanged", 0.80, 0.80, 0.80, 0.80, true},
		{"gentle raise, well below GPT floor", 0.60, 0.90, 0.60, 0.75, false}, // 0.60+0.4*0.30=0.72
		{"capped at visionKNNBoostCap", 0.88, 0.999, 0.88, visionKNNBoostCap, false},
	}
	for _, c := range cases {
		got := visionKNNBoostedConfidence(c.cur, c.knn)
		if got < c.cur {
			t.Errorf("%s: lowered confidence %.3f -> %.3f", c.name, c.cur, got)
		}
		if got > visionKNNBoostCap+1e-9 {
			t.Errorf("%s: exceeded cap: %.3f", c.name, got)
		}
		if c.exact {
			if got != c.wantMin {
				t.Errorf("%s: got %.3f want %.3f", c.name, got, c.wantMin)
			}
		} else if !(got > c.wantMin && got <= c.max+1e-9) {
			t.Errorf("%s: got %.3f, want in (%.3f, %.3f]", c.name, got, c.wantMin, c.max)
		}
	}
}

// cannedPlantNetAbelia returns an in-catalog species (Abelia chinensis → AAA0001)
// at a modest score so a kNN agreement boost is observable.
const cannedPlantNetAbelia = `{
  "bestMatch": "Abelia chinensis",
  "results": [
    {"score": 0.70, "species": {
      "scientificNameWithoutAuthor": "Abelia chinensis",
      "scientificName": "Abelia chinensis R.Br.",
      "commonNames": ["Chinese Abelia"]}}
  ],
  "remainingIdentificationRequests": 480
}`

// TestHandleIdentify_VisionKNN_BoostAndFailOpen drives the full 7a-4 path: a 503
// kNN service must fail open (no mutation); an agreeing kNN service must gently
// RAISE confidence while leaving plant_id / scientific_name unchanged.
func TestHandleIdentify_VisionKNN_BoostAndFailOpen(t *testing.T) {
	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, cannedPlantNetAbelia)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}

	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}

	doReq := func(knn *VisionKNNClient) IdentifyResult {
		h := HandleIdentify(pn, nil, content, nil, nil, knn, false, false, false, false, false, false, nil)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, _ := w.CreateFormFile("image", "a.jpg")
		_, _ = fw.Write(jpegMagic)
		_ = w.WriteField("organ", "flower")
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, "/v1/identify", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("X-Device-Install-Id", testUUID)
		req.Header.Set("X-App-Version", "1.1.1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out IdentifyResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// Baseline: kNN 503 → fail-open, no boost.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failSrv.Close()
	base := doReq(NewVisionKNNClient(failSrv.URL))
	if len(base.Suggestions) == 0 || base.Suggestions[0].PlantID == nil {
		t.Fatalf("baseline decision not in-catalog: %+v", base.Suggestions)
	}
	basePID := *base.Suggestions[0].PlantID
	baseConf := base.Suggestions[0].Confidence
	baseSci := base.Suggestions[0].ScientificName

	// kNN agrees with the in-catalog decision → gentle boost.
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[{"catalog_id":"`+basePID+`","vision_sim":0.9}],"nn_sim":0.9,"in_catalog":true,"in_catalog_confidence":0.7,"model":"t"}`)
	}))
	defer okSrv.Close()
	boosted := doReq(NewVisionKNNClient(okSrv.URL))

	if boosted.Suggestions[0].PlantID == nil || *boosted.Suggestions[0].PlantID != basePID {
		t.Errorf("plant_id changed by boost: base=%s got=%v", basePID, boosted.Suggestions[0].PlantID)
	}
	if boosted.Suggestions[0].ScientificName != baseSci {
		t.Errorf("scientific_name changed by boost: %q -> %q", baseSci, boosted.Suggestions[0].ScientificName)
	}
	if !(boosted.Suggestions[0].Confidence > baseConf) {
		t.Errorf("expected boosted confidence > baseline %.3f, got %.3f", baseConf, boosted.Suggestions[0].Confidence)
	}
	if boosted.Suggestions[0].Confidence > visionKNNBoostCap+1e-9 {
		t.Errorf("boost exceeded cap %.2f: %.3f", visionKNNBoostCap, boosted.Suggestions[0].Confidence)
	}
}

// TestRerankSchemaPlantNeutralForCultivar guards Codex #97: the non-Rosa cultivar
// disambiguation must send a PLANT-NEUTRAL structured-output schema. The rose
// schema's "empty when not a rose" wording drove empty matches for Juncus/Aeonium/
// etc., silently disabling the disambiguation. RerankRose keeps the rose schema.
func TestRerankSchemaPlantNeutralForCultivar(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"cultivar_certain\":false,\"matches\":[]}"}}]}`)
	}))
	defer srv.Close()
	vision := &VisionClient{APIKey: "k", Endpoint: srv.URL, Model: "t", HTTP: srv.Client(), identifyHTTP: srv.Client()}

	if _, err := vision.RerankCultivar(context.Background(), jpegMagic, "image/jpeg", testRoseCands()); err != nil {
		t.Fatalf("RerankCultivar: %v", err)
	}
	if !bytes.Contains(gotBody, []byte(`"cultivar_rerank"`)) {
		t.Errorf("RerankCultivar must send the plant-neutral cultivar_rerank schema; body=%s", gotBody)
	}
	if bytes.Contains(gotBody, []byte("not a rose")) {
		t.Errorf("RerankCultivar schema must NOT contain rose-specific 'not a rose' wording (Codex #97)")
	}

	if _, err := vision.RerankRose(context.Background(), jpegMagic, "image/jpeg", testRoseCands()); err != nil {
		t.Fatalf("RerankRose: %v", err)
	}
	if !bytes.Contains(gotBody, []byte(`"rose_cultivar_rerank"`)) {
		t.Errorf("RerankRose must still send the rose_cultivar_rerank schema")
	}
}

// TestHandleIdentify_VisionKNN_SlowServiceSkipped guards Codex #100: a slow/wedged
// kNN service must NOT block the already-decided response — 7a-4 skips it after the
// grace budget instead of waiting the full client timeout. Verified by wall-clock
// (handler returns fast) and by the confidence matching the no-kNN baseline.
func TestHandleIdentify_VisionKNN_SlowServiceSkipped(t *testing.T) {
	orig := visionKNNWaitBudget
	visionKNNWaitBudget = 50 * time.Millisecond
	defer func() { visionKNNWaitBudget = orig }()

	pnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, cannedPlantNetAbelia)
	}))
	defer pnSrv.Close()
	pn := &PlantNetClient{APIKey: "k", Endpoint: pnSrv.URL, Lang: "en", NbResults: 10, HTTP: pnSrv.Client()}

	content, err := LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}

	doReq := func(knn *VisionKNNClient) (IdentifyResult, time.Duration) {
		h := HandleIdentify(pn, nil, content, nil, nil, knn, false, false, false, false, false, false, nil)
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fw, _ := w.CreateFormFile("image", "a.jpg")
		_, _ = fw.Write(jpegMagic)
		_ = w.WriteField("organ", "flower")
		_ = w.Close()
		req := httptest.NewRequest(http.MethodPost, "/v1/identify", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("X-Device-Install-Id", testUUID)
		req.Header.Set("X-App-Version", "1.1.1")
		rec := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(rec, req)
		elapsed := time.Since(start)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out IdentifyResult
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out, elapsed
	}

	// Baseline: no kNN at all.
	base, _ := doReq(nil)
	if len(base.Suggestions) == 0 || base.Suggestions[0].PlantID == nil {
		t.Fatalf("baseline not in-catalog: %+v", base.Suggestions)
	}
	baseConf := base.Suggestions[0].Confidence

	// Slow kNN (responds well after the 50ms grace budget) → must be skipped.
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"candidates":[{"catalog_id":"`+*base.Suggestions[0].PlantID+`","vision_sim":0.99}],"nn_sim":0.99,"in_catalog":true,"in_catalog_confidence":0.9,"model":"t"}`)
	}))
	defer slowSrv.Close()

	out, elapsed := doReq(NewVisionKNNClient(slowSrv.URL))
	if elapsed > 250*time.Millisecond {
		t.Errorf("handler blocked on slow kNN (%v); must skip after ~%v grace budget", elapsed, visionKNNWaitBudget)
	}
	if out.Suggestions[0].Confidence != baseConf {
		t.Errorf("slow kNN should be skipped (no boost); conf %.4f != baseline %.4f", out.Suggestions[0].Confidence, baseConf)
	}
}
