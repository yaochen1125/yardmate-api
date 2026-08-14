package rarity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeCounts struct {
	m   map[string]int64
	err error
}

func (f *fakeCounts) IdentifyCountTotals(context.Context) (map[string]int64, error) {
	return f.m, f.err
}

type fakeStore struct {
	objects    map[string][]byte
	getErr     error
	putErr     error
	lastPutKey string
	lastPutCT  string
	lastPutCC  string
	putCalls   int
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (f *fakeStore) Put(_ context.Context, key string, body []byte, contentType, cacheControl string) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[key] = body
	f.lastPutKey, f.lastPutCT, f.lastPutCC = key, contentType, cacheControl
	f.putCalls++
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	body, ok := f.objects[key]
	return body, ok, nil
}

func newTestPublisher(counts *fakeCounts, store *fakeStore, cfg Config) *Publisher {
	if cfg.TierCuts == ([4]float64{}) {
		cfg.TierCuts = DefaultTierCuts
	}
	return NewPublisher(counts, store, cfg)
}

func TestPublishFirstAndSecondVersion(t *testing.T) {
	counts := &fakeCounts{m: map[string]int64{"AAA0001": 80, "AAA0002": 20}}
	store := newFakeStore()
	p := newTestPublisher(counts, store, Config{MinSample: 1})

	m1, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if m1.Version != 1 {
		t.Errorf("first version = %d, want 1", m1.Version)
	}
	if store.lastPutKey != "content/dex/rarity.json" {
		t.Errorf("key = %q, want content/dex/rarity.json (default prefix)", store.lastPutKey)
	}
	if store.lastPutCT != "application/json" || !strings.Contains(store.lastPutCC, "max-age=60") {
		t.Errorf("contentType=%q cacheControl=%q", store.lastPutCT, store.lastPutCC)
	}

	// Second pass reads back the published object → version increments.
	m2, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if m2.Version != 2 {
		t.Errorf("second version = %d, want 2", m2.Version)
	}

	// The stored body is the manifest verbatim.
	var stored Manifest
	if err := json.Unmarshal(store.objects[p.Key()], &stored); err != nil {
		t.Fatalf("stored body unparseable: %v", err)
	}
	if stored.Version != 2 || stored.TotalScans != 100 {
		t.Errorf("stored = %+v", stored)
	}
}

func TestPublishCountsErrorAborts(t *testing.T) {
	store := newFakeStore()
	p := newTestPublisher(&fakeCounts{err: errors.New("db down")}, store, Config{})
	if _, err := p.Publish(context.Background()); err == nil {
		t.Fatal("want error when count source fails")
	}
	if store.putCalls != 0 {
		t.Error("must not Put after a failed aggregate read")
	}
}

// TestPublishGetErrorAborts: a transient R2 read failure must abort the pass
// (next tick retries) rather than resetting the version counter to 1.
func TestPublishGetErrorAborts(t *testing.T) {
	counts := &fakeCounts{m: map[string]int64{"AAA0001": 5}}
	store := newFakeStore()
	store.getErr = errors.New("r2 unreachable")
	p := newTestPublisher(counts, store, Config{})
	if _, err := p.Publish(context.Background()); err == nil {
		t.Fatal("want error when version read fails")
	}
	if store.putCalls != 0 {
		t.Error("must not Put when the version read failed")
	}
}

// TestPublishCorruptPreviousSelfHeals: unparseable published JSON restarts the
// version at 1 instead of wedging the pipeline.
func TestPublishCorruptPreviousSelfHeals(t *testing.T) {
	counts := &fakeCounts{m: map[string]int64{"AAA0001": 5}}
	store := newFakeStore()
	p := newTestPublisher(counts, store, Config{MinSample: 1})
	store.objects[p.Key()] = []byte("{not json")

	m, err := p.Publish(context.Background())
	if err != nil {
		t.Fatalf("publish over corrupt state: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("version = %d, want 1 (self-heal)", m.Version)
	}
}

func TestNewPublisherNormalization(t *testing.T) {
	p := newTestPublisher(&fakeCounts{}, newFakeStore(), Config{
		Prefix:   " /content-staging/ ",
		Interval: time.Second, // below floor → clamped
	})
	if p.Key() != "content-staging/dex/rarity.json" {
		t.Errorf("key = %q", p.Key())
	}
	if p.cfg.Interval != minInterval {
		t.Errorf("interval = %v, want clamped %v", p.cfg.Interval, minInterval)
	}
	if p.cfg.MinSample != 1 {
		t.Errorf("MinSample = %d, want floor 1", p.cfg.MinSample)
	}
	if p.cfg.Interval <= 0 || p.cfg.TierCuts == ([4]float64{}) {
		t.Error("defaults not applied")
	}

	p2 := newTestPublisher(&fakeCounts{}, newFakeStore(), Config{})
	if p2.cfg.Interval != DefaultInterval {
		t.Errorf("zero interval → %v, want default %v", p2.cfg.Interval, DefaultInterval)
	}
}

func rebuildReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/internal/rarity/rebuild", nil)
	if token != "" {
		r.Header.Set(AdminTokenHeader, token)
	}
	return r
}

func TestHandleRebuildAuth(t *testing.T) {
	counts := &fakeCounts{m: map[string]int64{"AAA0001": 5}}
	p := newTestPublisher(counts, newFakeStore(), Config{MinSample: 1, AdminToken: "sekrit"})
	h := p.HandleRebuild()

	// Missing token.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, rebuildReq(""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: %d", rec.Code)
	}
	// Wrong token.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, rebuildReq("wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", rec.Code)
	}
	// Correct token → publishes and reports.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, rebuildReq("sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("good token: %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "ok" || out["version"].(float64) != 1 {
		t.Errorf("body = %v", out)
	}
}

// TestHandleRebuildEmptyConfiguredTokenDeniesAll: defense in depth — an empty
// configured token must never behave as "no auth required".
func TestHandleRebuildEmptyConfiguredTokenDeniesAll(t *testing.T) {
	p := newTestPublisher(&fakeCounts{}, newFakeStore(), Config{AdminToken: ""})
	rec := httptest.NewRecorder()
	p.HandleRebuild().ServeHTTP(rec, rebuildReq("anything"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty configured token must 401, got %d", rec.Code)
	}
}

func TestHandleRebuildPublishFailure(t *testing.T) {
	p := newTestPublisher(&fakeCounts{err: errors.New("db down")}, newFakeStore(), Config{AdminToken: "sekrit"})
	rec := httptest.NewRecorder()
	p.HandleRebuild().ServeHTTP(rec, rebuildReq("sekrit"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("publish failure: %d, want 502", rec.Code)
	}
}
