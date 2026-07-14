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

// buildServerWithTraining wires a server whose ONLY registered per-device
// endpoints are the training routes (all upstream clients nil), so the tests
// isolate the TRAINING_UPLOAD_ENABLED registration gate.
func buildServerWithTraining(t *testing.T, store *training.Store) *Server {
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
	vault, err := secrets.Parse(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	lim := ratelimit.New(1000, time.Hour, 1000, 24*time.Hour, 1000, time.Hour)
	return newServer(verifier, vault, lim, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, store)
}

// When the store is nil (TRAINING_UPLOAD_ENABLED off — the default), the routes
// are not registered → 404, so no upload can happen while the flag is off.
func TestTrainingRoutesUnregisteredWhenDisabled(t *testing.T) {
	s := buildTestServer(t) // training store nil
	for _, path := range []string{"/v1/training/photo", "/v1/training/delete"} {
		rr := do(t, s, http.MethodPost, path, map[string]string{})
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s with training off: status = %d, want 404", path, rr.Code)
		}
	}
}

// When a store is present the routes ARE registered: a malformed request reaches
// the handler and gets a 400 (missing device id), never a 404.
func TestTrainingRoutesRegisteredWhenEnabled(t *testing.T) {
	store, err := training.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := buildServerWithTraining(t, store)

	for _, path := range []string{"/v1/training/photo", "/v1/training/delete"} {
		rr := do(t, s, http.MethodPost, path, map[string]string{}) // no device id header
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s with training on: got 404, route not registered", path)
		}
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (reached handler, missing device id)", path, rr.Code)
		}
	}
}
