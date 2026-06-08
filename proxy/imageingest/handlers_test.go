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

// --- public /v1/plants/catalog-images (in-catalog supplementary, SPEC §2.8) ---

// newTestCatalogService builds a Service whose ingestor carries a 1-entry
// authoritative catalog map (AAA0001 -> "Abelia chinensis"), a mock cascade, a
// mock store, and a NIL ledger (the in-catalog path is zero-DB). Returns the
// source + store so tests can inspect what was searched / uploaded.
func newTestCatalogService() (*Service, *mockSource, *mockStore) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	in := NewIngestor(src, nil, store, nil, Config{
		AllowAttributionLicenses: true,
		DefaultImageCount:        4,
		CatalogNames:             map[string]string{"AAA0001": "Abelia chinensis"},
	})
	return NewService(in, testAdminToken), src, store
}

func catalogReq(svc *Service, deviceID, appVer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/plants/catalog-images", strings.NewReader(body))
	if deviceID != "" {
		r.Header.Set("X-Device-Install-Id", deviceID)
	}
	if appVer != "" {
		r.Header.Set("X-App-Version", appVer)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	HandleCatalog(svc)(w, r)
	return w
}

func TestHandleCatalog_Disabled_503(t *testing.T) {
	w := catalogReq(nil, testDeviceID, "1.0", `{"catalog_id":"AAA0001"}`)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "ingest_disabled") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleCatalog_MissingDeviceID_400(t *testing.T) {
	svc, _, _ := newTestCatalogService()
	w := catalogReq(svc, "", "1.0", `{"catalog_id":"AAA0001"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing_device_id") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleCatalog_BadID_400(t *testing.T) {
	svc, src, store := newTestCatalogService()
	w := catalogReq(svc, testDeviceID, "1.0", `{"catalog_id":"bad/id","scientific_name":"X"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_request") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
	if src.lastQuery() != "" || len(store.puts) != 0 {
		t.Error("a malformed catalog_id must never search or write (path-injection guard)")
	}
}

// TestHandleCatalog_UnknownID_400 is the second required P1 test: a well-formed
// catalog_id that is not one of the curated 1522 → 400 unknown_catalog_id, with
// no search and no R2 write (the 400 short-circuits before the goroutine).
func TestHandleCatalog_UnknownID_400(t *testing.T) {
	svc, src, store := newTestCatalogService()
	w := catalogReq(svc, testDeviceID, "1.0", `{"catalog_id":"ZZZ9999","scientific_name":"Abelia chinensis"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown_catalog_id") {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
	if src.lastQuery() != "" {
		t.Errorf("unknown id must not trigger a cascade search, got %q", src.lastQuery())
	}
	if len(store.puts) != 0 {
		t.Error("unknown id must never write to R2")
	}
}

func TestHandleCatalog_NoClientName_202(t *testing.T) {
	svc, _, _ := newTestCatalogService()
	// scientific_name is now advisory; its absence must NOT 400 (it used to).
	w := catalogReq(svc, testDeviceID, "1.0", `{"catalog_id":"AAA0001"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleCatalog_WrongClientName_UsesAuthoritative is the first required P1
// test: a tampered client pairs a real id (AAA0001) with an UNRELATED species
// name; the backend must search the SERVER's authoritative name regardless, so
// the stored external/ gallery can never be poisoned (SPEC §2.8).
func TestHandleCatalog_WrongClientName_UsesAuthoritative(t *testing.T) {
	svc, src, store := newTestCatalogService()
	w := catalogReq(svc, testDeviceID, "1.0",
		`{"catalog_id":"AAA0001","scientific_name":"Toxicodendron radicans"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	// Fire-and-forget: poll for the index.json commit marker the goroutine writes;
	// once present the cascade has run, so its recorded query is final.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		_, done := store.puts[catalogIndexKey("AAA0001")]
		store.mu.Unlock()
		if done {
			if got := src.lastQuery(); got != "Abelia chinensis" {
				t.Fatalf("cascade searched %q, want authoritative %q (client name leaked into the search)", got, "Abelia chinensis")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("catalog ingest did not complete within deadline")
}

// Compile-time guards: production types satisfy the ingestor interfaces so a
// signature drift fails the build, not just at wiring time.
var (
	_ ImageSource = (*sources.INatClient)(nil)
	_ ImageSource = (*sources.WikimediaClient)(nil)
	_ ObjectStore = (*R2Client)(nil)
	_ LedgerStore = (*Ledger)(nil)
)

// canonicalizeHeroPhotoID normalizes string/number JSON to a base-10 int64
// string, or "" for anything non-canonical (SPEC §2.1.1 / §9 #17).
func TestCanonicalizeHeroPhotoID(t *testing.T) {
	cases := []struct {
		raw  string // raw JSON token
		want string
	}{
		{`"12345"`, "12345"}, // JSON string (what iOS sends)
		{`12345`, "12345"},   // bare JSON number
		{`"00123"`, "123"},   // leading zeros normalized away (still matches int64 key)
		{`" 678 "`, "678"},   // whitespace-padded string
		{``, ""},             // absent
		{`""`, ""},           // empty string
		{`"abc"`, ""},        // non-numeric
		{`"-5"`, ""},         // negative
		{`0`, ""},            // zero is not a valid id
		{`"1 2"`, ""},        // embedded space
		{`"+9"`, ""},         // signed
	}
	for _, c := range cases {
		if got := canonicalizeHeroPhotoID(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("canonicalizeHeroPhotoID(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}
