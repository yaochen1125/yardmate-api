package enrichment

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

const testUUID = "ABCDEF01-2345-6789-ABCD-EF0123456789"

func newTestHandler(db ServiceDB, llm ServiceLLM) http.HandlerFunc {
	content, err := proxy.LoadContent()
	if err != nil {
		// LoadContent only fails if the embedded catalog JSON is malformed;
		// surfacing the error here would be a developer bug — panic is loud.
		panic(err)
	}
	svc := NewService(content, db, llm, NewCache(10, time.Hour))
	return HandleEnrichment(svc)
}

func makeRequest(t *testing.T, body string, deviceID, appVer string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/plants/enrichment", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if deviceID != "" {
		r.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVer != "" {
		r.Header.Set("X-App-Version", appVer)
	}
	return r
}

func TestHandler_MissingDeviceID_400(t *testing.T) {
	h := newTestHandler(nil, nil)
	r := makeRequest(t, `{"scientificName":"x"}`, "", "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing_device_id") {
		t.Errorf("expected missing_device_id, got %q", w.Body.String())
	}
}

func TestHandler_InvalidUUIDDeviceID_400(t *testing.T) {
	h := newTestHandler(nil, nil)
	r := makeRequest(t, `{"scientificName":"x"}`, "not-a-uuid", "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing_device_id") {
		t.Errorf("expected missing_device_id error, got %q", w.Body.String())
	}
}

func TestHandler_MissingAppVersion_400(t *testing.T) {
	h := newTestHandler(nil, nil)
	r := makeRequest(t, `{"scientificName":"x"}`, testUUID, "")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing_app_version") {
		t.Errorf("expected missing_app_version, got %q", w.Body.String())
	}
}

func TestHandler_BadJSON_400(t *testing.T) {
	h := newTestHandler(nil, nil)
	r := makeRequest(t, `{not json`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad_json") {
		t.Errorf("expected bad_json, got %q", w.Body.String())
	}
}

func TestHandler_EmptyScientificName_400(t *testing.T) {
	h := newTestHandler(nil, nil)
	r := makeRequest(t, `{"scientificName":""}`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing_scientific_name") {
		t.Errorf("expected missing_scientific_name, got %q", w.Body.String())
	}
}

func TestHandler_TooLongScientificName_400(t *testing.T) {
	h := newTestHandler(nil, nil)
	long := strings.Repeat("a", 201)
	r := makeRequest(t, `{"scientificName":"`+long+`"}`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "scientific_name_too_long") {
		t.Errorf("expected scientific_name_too_long, got %q", w.Body.String())
	}
}

func TestHandler_CatalogHit_200(t *testing.T) {
	h := newTestHandler(nil, nil)
	r := makeRequest(t, `{"scientificName":"Abelia chinensis"}`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"AAA0001"`) {
		t.Errorf("expected catalog id in body, got %q", w.Body.String())
	}
}

func TestHandler_DBUnavailable_502(t *testing.T) {
	db := &stubDB{lookupQ: []dbLookupResult{{err: ErrDBUnavailable}}}
	h := newTestHandler(db, nil)
	r := makeRequest(t, `{"scientificName":"Madeup notreal"}`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "db_unavailable") {
		t.Errorf("expected db_unavailable, got %q", w.Body.String())
	}
}

func TestHandler_LLMUnavailable_502(t *testing.T) {
	db := &stubDB{} // empty queues -> miss
	llm := &stubLLM{err: ErrEnrichmentUnavailable}
	h := newTestHandler(db, llm)
	r := makeRequest(t, `{"scientificName":"Madeup llmfail"}`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "enrichment_unavailable") {
		t.Errorf("expected enrichment_unavailable, got %q", w.Body.String())
	}
}

func TestHandler_FreshGeneration_200(t *testing.T) {
	llmOut := &proxy.PlantDetail{
		ScientificName: "Madeup fresh",
		CommonName:     "Fresh",
		Description:    "an llm-generated plant",
	}
	db := &stubDB{}
	llm := &stubLLM{ret: llmOut}
	h := newTestHandler(db, llm)
	r := makeRequest(t, `{"scientificName":"Madeup fresh","commonName":"Fresh"}`, testUUID, "1.0.0")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "an llm-generated plant") {
		t.Errorf("expected description in body, got %q", w.Body.String())
	}
}
