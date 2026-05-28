package imageingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAdminToken = "super-secret-admin-token"

func newTestService(t *testing.T, allowAttrib bool, seedNames []string) (*Service, *mockStore, *mockLedger) {
	t.Helper()
	commons := &mockCommons{
		searchRet: []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())},
	}
	store := newMockStore()
	ledger := newMockLedger()
	in := NewIngestor(commons, store, ledger, &mockSeeds{names: seedNames}, Config{AllowAttributionLicenses: allowAttrib})
	return NewService(in, testAdminToken), store, ledger
}

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
	svc, _, _ := newTestService(t, false, nil)
	w := runReq(svc, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "missing_admin_token") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleRun_BadToken_401(t *testing.T) {
	svc, _, _ := newTestService(t, false, nil)
	w := runReq(svc, "wrong-token", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad_admin_token") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleRun_Disabled_503(t *testing.T) {
	// nil service → ingest_disabled (route normally not registered, but the
	// handler guards anyway).
	w := runReq(nil, testAdminToken, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ingest_disabled") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleRun_SingleSlugWithoutName_400(t *testing.T) {
	svc, _, _ := newTestService(t, false, nil)
	w := runReq(svc, testAdminToken, "?slug=rosa")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad_request") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleRun_SingleSlug_OK(t *testing.T) {
	svc, store, ledger := newTestService(t, false, nil)
	w := runReq(svc, testAdminToken, "?slug=rosa&name=Rosa+regina")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var out IngestOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != OutcomeIngested {
		t.Errorf("status = %q, want ingested", out.Status)
	}
	if _, ok := store.puts[heroKey("rosa")]; !ok {
		t.Errorf("expected hero put")
	}
	// Single-slug path also records the ledger.
	if _, ok := ledger.rows["rosa"]; !ok {
		t.Errorf("expected ledger row for rosa")
	}
}

func TestHandleRun_Batch_202(t *testing.T) {
	svc, _, ledger := newTestService(t, false, []string{"Rosa regina"})
	w := runReq(svc, testAdminToken, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202 body=%s", w.Code, w.Body.String())
	}
	// Batch runs in a detached goroutine; poll the ledger briefly for the row.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ledger.mu.Lock()
		_, ok := ledger.rows["rosa-regina"]
		ledger.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("batch did not record rosa-regina within deadline")
}

func TestHandleRun_BatchBadLimit_400(t *testing.T) {
	svc, _, _ := newTestService(t, false, nil)
	w := runReq(svc, testAdminToken, "?limit=notanumber")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad_request") {
		t.Errorf("body = %q", w.Body.String())
	}
}

// Compile-time guard: ensure the production types satisfy the ingestor
// interfaces (so a signature drift fails the build, not just at wiring time).
var (
	_ CommonsSearcher = (*CommonsClient)(nil)
	_ ObjectStore     = (*R2Client)(nil)
	_ LedgerStore     = (*Ledger)(nil)
	_ SeedSource      = (*SeedReader)(nil)
)

// silence unused import if context drops out of a future edit.
var _ = context.Background
