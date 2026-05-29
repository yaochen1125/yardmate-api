package imageingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy/imageingest/sources"
)

const (
	testAdminToken = "super-secret-admin-token"
	testDeviceID   = "11111111-1111-1111-1111-111111111111"
)

func newTestService(allowAttrib bool) (*Service, *mockStore, *mockLedger) {
	src := &mockSource{searchRet: []sources.Candidate{
		cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 4000, 3000),
	}}
	store := newMockStore()
	ledger := newMockLedger()
	in := newIngestor(src, store, ledger, Config{AllowAttributionLicenses: allowAttrib})
	return NewService(in, testAdminToken), store, ledger
}

// --- internal /internal/imageingest/run ---

func runReq(svc *Service, token, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/internal/imageingest/run"+query, nil)
	if token != "" {
		r.Header.Set(adminTokenHeader, token)
	}
	w := httptest.NewRecorder()
	HandleRun(svc)(w, r)
	return w
}

func TestHandleRun_MissingToken_401(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := runReq(svc, "", "")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "missing_admin_token") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleRun_BadToken_401(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := runReq(svc, "wrong-token", "")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "bad_admin_token") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleRun_Disabled_503(t *testing.T) {
	w := runReq(nil, testAdminToken, "")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "ingest_disabled") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleRun_NoSlugNoNames_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := runReq(svc, testAdminToken, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleRun_SingleSlugWithoutName_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := runReq(svc, testAdminToken, "?slug=rosa")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleRun_SingleSlug_OK(t *testing.T) {
	svc, store, ledger := newTestService(false)
	w := runReq(svc, testAdminToken, "?slug=rosa&name=Rosa+regina&image_count=1")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var out IngestOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Slug != "rosa" || len(out.PerImage) != 1 || out.PerImage[0].Status != ImgIngested {
		t.Errorf("outcome = %+v", out)
	}
	if _, ok := store.puts[galleryKey("rosa", 1)]; !ok {
		t.Errorf("expected put at rosa/1.png")
	}
	if ledger.files[fileKey("rosa", 1)] == nil {
		t.Errorf("expected file ledger row")
	}
}

func TestHandleRun_BatchNames_OK(t *testing.T) {
	svc, _, ledger := newTestService(false)
	w := runReq(svc, testAdminToken, "?names=Rosa+regina,Tulipa+gesneriana&image_count=1")
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var outs []IngestOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &outs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(outs) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(outs))
	}
	if ledger.species["rosa-regina"] == nil || ledger.species["tulipa-gesneriana"] == nil {
		t.Errorf("expected species rows for both names")
	}
}

func TestHandleRun_BadImageCount_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := runReq(svc, testAdminToken, "?slug=rosa&name=Rosa+regina&image_count=notanumber")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

// --- public /v1/plants/imageingest ---

func publicReq(svc *Service, deviceID, appVer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/plants/imageingest", strings.NewReader(body))
	if deviceID != "" {
		r.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVer != "" {
		r.Header.Set("X-App-Version", appVer)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	HandlePublic(svc)(w, r)
	return w
}

func TestHandlePublic_Disabled_503(t *testing.T) {
	w := publicReq(nil, testDeviceID, "1.0", `{"scientific_name":"Rosa regina"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestHandlePublic_MissingDeviceID_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := publicReq(svc, "", "1.0", `{"scientific_name":"Rosa regina"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing_device_id") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandlePublic_MissingAppVersion_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := publicReq(svc, testDeviceID, "", `{"scientific_name":"Rosa regina"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing_app_version") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandlePublic_BadJSON_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := publicReq(svc, testDeviceID, "1.0", `{not json`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandlePublic_EmptyName_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := publicReq(svc, testDeviceID, "1.0", `{"scientific_name":"  "}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandlePublic_ImageCountOutOfRange_400(t *testing.T) {
	svc, _, _ := newTestService(false)
	w := publicReq(svc, testDeviceID, "1.0", `{"scientific_name":"Rosa regina","image_count":99}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandlePublic_Accepted_202(t *testing.T) {
	svc, _, ledger := newTestService(false)
	w := publicReq(svc, testDeviceID, "1.0", `{"scientific_name":"Rosa regina","image_count":1}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["accepted"] != true || resp["slug"] != "rosa-regina" {
		t.Errorf("resp = %+v", resp)
	}
	// Fire-and-forget: poll the ledger for the species row the goroutine writes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		_, ok := ledger.species["rosa-regina"]
		ledger.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("public ingest did not record rosa-regina within deadline")
}

// Compile-time guards: production types satisfy the ingestor interfaces so a
// signature drift fails the build, not just at wiring time.
var (
	_ ImageSource = (*sources.INatClient)(nil)
	_ ImageSource = (*sources.WikimediaClient)(nil)
	_ ObjectStore = (*R2Client)(nil)
	_ LedgerStore = (*Ledger)(nil)
)
