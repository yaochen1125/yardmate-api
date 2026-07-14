package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/secrets"
	"github.com/yaochen1125/yardmate-api/training"
)

// buildServerWithTraining wires a server whose only registered per-device
// endpoints are the training routes (all upstream clients nil), so the tests
// isolate the training registration gates. intakeEnabled sets
// TRAINING_UPLOAD_ENABLED in the vault (the intake-route gate); the store being
// present is the deletion gate.
func buildServerWithTraining(t *testing.T, store *training.Store, intakeEnabled bool) *Server {
	t.Helper()
	dir := t.TempDir()
	astore, err := attest.OpenStore(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = astore.Close() })
	verifier, err := attest.New(attest.Options{AppID: "TEAM.bundle", Store: astore})
	if err != nil {
		t.Fatal(err)
	}
	secretsBody := ""
	if intakeEnabled {
		secretsBody = "TRAINING_UPLOAD_ENABLED=true\n"
	}
	vault, err := secrets.Parse(strings.NewReader(secretsBody))
	if err != nil {
		t.Fatal(err)
	}
	lim := ratelimit.New(1000, time.Hour, 1000, 24*time.Hour, 1000, time.Hour)
	return newServer(verifier, vault, lim, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, store)
}

func openTestTrainingStore(t *testing.T) *training.Store {
	t.Helper()
	store, err := training.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// Store nil (nothing ever collected, intake off — the default) → both routes 404.
func TestTrainingRoutesUnregisteredWhenDisabled(t *testing.T) {
	s := buildTestServer(t) // training store nil
	for _, path := range []string{"/v1/training/photo", "/v1/training/delete"} {
		rr := do(t, s, http.MethodPost, path, map[string]string{})
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s with training off: status = %d, want 404", path, rr.Code)
		}
	}
}

// Store present AND intake flag on → both routes registered (400 = reached
// handler, missing device id — not 404).
func TestTrainingRoutesRegisteredWhenEnabled(t *testing.T) {
	s := buildServerWithTraining(t, openTestTrainingStore(t), true)
	for _, path := range []string{"/v1/training/photo", "/v1/training/delete"} {
		rr := do(t, s, http.MethodPost, path, map[string]string{}) // no device id header
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s with intake on: got 404, route not registered", path)
		}
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (reached handler, missing device id)", path, rr.Code)
		}
	}
}

// Rollback path (Codex #105 P1): the store is open (a corpus exists) but intake
// is OFF. Intake must be gone (404) yet deletion must stay reachable (400) so a
// user can still erase photos they previously shared.
func TestTrainingDeletionAvailableWhenIntakeDisabled(t *testing.T) {
	s := buildServerWithTraining(t, openTestTrainingStore(t), false)

	rr := do(t, s, http.MethodPost, "/v1/training/photo", map[string]string{})
	if rr.Code != http.StatusNotFound {
		t.Errorf("intake route with flag off: status = %d, want 404", rr.Code)
	}
	rr = do(t, s, http.MethodPost, "/v1/training/delete", map[string]string{})
	if rr.Code == http.StatusNotFound {
		t.Error("delete route unavailable with intake off — erasure must stay open")
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("delete route: status = %d, want 400 (reached handler)", rr.Code)
	}
}

// buildTrainingStore keeps the store open for deletion after the kill switch is
// flipped off, provided a corpus already exists (Codex #105 P1).
func TestBuildTrainingStoreDeletionSurvivesRollback(t *testing.T) {
	t.Setenv("YARDMATE_API_TRAINING_DIR", t.TempDir())
	offVault, err := secrets.Parse(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	onVault, err := secrets.Parse(strings.NewReader("TRAINING_UPLOAD_ENABLED=true\n"))
	if err != nil {
		t.Fatal(err)
	}

	// Off + no corpus yet → nil (nothing to intake or delete).
	if s := buildTrainingStore(offVault); s != nil {
		_ = s.Close()
		t.Fatal("off + no corpus: expected nil store")
	}
	// Flag on → opens (and creates the corpus).
	s := buildTrainingStore(onVault)
	if s == nil {
		t.Fatal("flag on: expected an open store")
	}
	_ = s.Close()
	// Corpus now exists; flag flipped OFF → store STILL opens so deletion works.
	s2 := buildTrainingStore(offVault)
	if s2 == nil {
		t.Fatal("off + existing corpus: store must stay open for deletion (Codex P1)")
	}
	_ = s2.Close()
}
