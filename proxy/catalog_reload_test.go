package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// syntheticName is a scientific name guaranteed NOT to be in the embedded
// catalog, used to prove a hot-reload made a brand-new plant resolvable.
const (
	syntheticName = "Testus syntheticus"
	syntheticID   = "ZZZ9001"
)

// freshIndexBytes returns a plants_index.json payload = every embedded row PLUS
// one synthetic new plant. Building on top of the real embed keeps the row count
// above the truncation guard (a small hand-written payload would be refused) and
// mirrors the real-world case: catalog-promote appends a row, the CDN publishes
// the whole file, the backend reloads it.
func freshIndexBytes(t *testing.T) []byte {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(plantsIndexRaw, &rows); err != nil {
		t.Fatalf("unmarshal embedded plants_index: %v", err)
	}
	rows = append(rows, map[string]any{
		"id":              syntheticID,
		"scientific_name": syntheticName,
		"common_name":     "Synthetic Test Plant",
	})
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal fresh index: %v", err)
	}
	return b
}

// TestReloadCatalogNames_HotSwap is the core zero-downtime contract: a fresh
// plants_index that adds a new plant becomes resolvable via LookupPlantID after
// an atomic swap, WITHOUT rebuilding the ContentIndex — and the pre-existing
// entries keep resolving.
func TestReloadCatalogNames_HotSwap(t *testing.T) {
	c := loadContentForTests(t)

	// Baseline: the synthetic plant is unknown before the reload.
	if _, ok := c.LookupPlantID(syntheticName); ok {
		t.Fatalf("%q resolved before reload — test precondition broken", syntheticName)
	}
	baseCount := c.currentNames().count

	n, err := c.ReloadCatalogNames(freshIndexBytes(t), "etag-v2")
	if err != nil {
		t.Fatalf("ReloadCatalogNames: %v", err)
	}
	if n != baseCount+1 {
		t.Errorf("reloaded count = %d, want %d (embed + 1 synthetic)", n, baseCount+1)
	}

	// The new plant now resolves — no restart, no ContentIndex rebuild.
	if id, ok := c.LookupPlantID(syntheticName); !ok || id != syntheticID {
		t.Errorf("after hot-swap LookupPlantID(%q) = (%q,%v), want (%q,true)", syntheticName, id, ok, syntheticID)
	}
	// A pre-existing entry still resolves through the swapped table.
	if id, ok := c.LookupPlantID("Abelia chinensis"); !ok || id != "AAA0001" {
		t.Errorf("after hot-swap existing lookup = (%q,%v), want (AAA0001,true)", id, ok)
	}
	// Provenance tag updated for observability.
	if v := c.currentNames().version; v != "etag-v2" {
		t.Errorf("version tag = %q, want etag-v2", v)
	}
}

// TestReloadCatalogNames_RejectsGarbage: a JSON parse error must leave the
// current table untouched (fail-safe) and return an error.
func TestReloadCatalogNames_RejectsGarbage(t *testing.T) {
	c := loadContentForTests(t)
	before, _ := c.LookupPlantID("Abelia chinensis")

	if _, err := c.ReloadCatalogNames([]byte("{ not json"), "bad"); err == nil {
		t.Fatal("expected parse error for garbage payload")
	}
	// Table preserved: Abelia still resolves to the same id.
	if id, ok := c.LookupPlantID("Abelia chinensis"); !ok || id != before {
		t.Errorf("garbage reload corrupted the live table: got (%q,%v), want (%q,true)", id, ok, before)
	}
}

// TestReloadCatalogNames_RejectsTruncation: a valid but degenerate payload (far
// fewer rows than the current table) is refused by the truncation guard so a
// half-published CDN object can't shrink identify coverage.
func TestReloadCatalogNames_RejectsTruncation(t *testing.T) {
	c := loadContentForTests(t)
	tiny := []byte(`[{"id":"AAA0001","scientific_name":"Abelia chinensis","common_name":"Chinese Abelia"}]`)

	if _, err := c.ReloadCatalogNames(tiny, "trunc"); err == nil {
		t.Fatal("expected truncation guard to reject a 1-row payload")
	}
	// Still the full embed table.
	if c.currentNames().count < 1000 {
		t.Errorf("truncation guard failed to protect the table: count now %d", c.currentNames().count)
	}
}

// TestReloadCatalogNames_RejectsEmpty: an empty array parses fine but must be
// refused (count == 0) so identify can never end up with an empty index.
func TestReloadCatalogNames_RejectsEmpty(t *testing.T) {
	c := loadContentForTests(t)
	if _, err := c.ReloadCatalogNames([]byte(`[]`), "empty"); err == nil {
		t.Fatal("expected empty-payload rejection")
	}
	if c.currentNames().count == 0 {
		t.Error("empty reload emptied the live table")
	}
}

// TestReloadCatalogNames_ConcurrentReadsDuringSwap is the zero-downtime race
// guard: request-path readers (LookupPlantID) must never be blocked by, nor
// observe a torn state from, a concurrent hot-swap. Run under `-race` this
// proves the atomic.Pointer swap is data-race-free; every read must also return
// a well-formed result (a real id or a clean miss — never a panic).
func TestReloadCatalogNames_ConcurrentReadsDuringSwap(t *testing.T) {
	c := loadContentForTests(t)
	fresh := freshIndexBytes(t)

	done := make(chan struct{})
	readers := 8
	readC := make(chan int, readers)
	for i := 0; i < readers; i++ {
		go func() {
			n := 0
			for {
				select {
				case <-done:
					readC <- n
					return
				default:
					// A known-present species must resolve to its id throughout
					// the swaps; a swap never makes an existing plant vanish.
					if id, ok := c.LookupPlantID("Abelia chinensis"); !ok || id != "AAA0001" {
						t.Errorf("existing lookup torn during swap: (%q,%v)", id, ok)
					}
					c.LookupPlantID(syntheticName) // exercises the newly-added key too
					n++
				}
			}
		}()
	}

	// Hammer the swap while readers run.
	for i := 0; i < 200; i++ {
		if _, err := c.ReloadCatalogNames(fresh, "race"); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}
	close(done)
	for i := 0; i < readers; i++ {
		<-readC
	}
}

// TestReloadCatalogNames_RejectsWrongShape: a full-length, valid-JSON payload
// whose rows all LOST their scientific_name (a field rename / export bug)
// indexes 0 usable plants and must be refused — the guard keys off usable
// indexed plants, not the raw parsed-array length, so this can't slip past.
func TestReloadCatalogNames_RejectsWrongShape(t *testing.T) {
	c := loadContentForTests(t)
	before, _ := c.LookupPlantID("Abelia chinensis")

	// Reserialize every embedded row but drop the scientific_name field: a full
	// ~1633-row array that still parses, yet yields an all-empty name index.
	var rows []map[string]any
	if err := json.Unmarshal(plantsIndexRaw, &rows); err != nil {
		t.Fatalf("unmarshal embed: %v", err)
	}
	for _, r := range rows {
		delete(r, "scientific_name")
	}
	wrong, _ := json.Marshal(rows)

	if _, err := c.ReloadCatalogNames(wrong, "wrongshape"); err == nil {
		t.Fatalf("expected wrong-shape rejection for a %d-row all-empty-name payload", len(rows))
	}
	// Table preserved: identify still resolves through the prior good table.
	if id, ok := c.LookupPlantID("Abelia chinensis"); !ok || id != before {
		t.Errorf("wrong-shape reload corrupted the live table: (%q,%v), want (%q,true)", id, ok, before)
	}
}

// etagIndexHandler serves a plants_index.json body with a fixed strong ETag and
// honors If-None-Match with 304 — the exact Cloudflare behaviour the poller's
// conditional-GET version gate relies on (SPEC §9.3).
func etagIndexHandler(body []byte, etag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// TestCatalogReloader_PollSwapsThenNotModified drives the poller end-to-end: the
// first poll fetches + swaps (new plant resolvable), the second poll sends
// If-None-Match and gets 304 → no work, table unchanged. Proves the ETag gate
// and the zero-restart swap together.
func TestCatalogReloader_PollSwapsThenNotModified(t *testing.T) {
	c := loadContentForTests(t)
	srv := httptest.NewServer(etagIndexHandler(freshIndexBytes(t), `"v2etag"`))
	defer srv.Close()

	r := NewCatalogReloader(c, srv.URL, time.Minute)
	ctx := context.Background()

	// First poll: 200 → swap.
	changed, err := r.pollOnce(ctx)
	if err != nil {
		t.Fatalf("first pollOnce: %v", err)
	}
	if !changed {
		t.Fatal("first pollOnce reported no change")
	}
	if id, ok := c.LookupPlantID(syntheticName); !ok || id != syntheticID {
		t.Errorf("after poll LookupPlantID(%q) = (%q,%v), want (%q,true)", syntheticName, id, ok, syntheticID)
	}
	if r.etag != `"v2etag"` {
		t.Errorf("reloader etag = %q, want \"v2etag\"", r.etag)
	}

	// Second poll: unchanged ETag → 304 → no swap, no error.
	changed, err = r.pollOnce(ctx)
	if err != nil {
		t.Fatalf("second pollOnce (304 path): %v", err)
	}
	if changed {
		t.Error("second pollOnce swapped despite 304 Not Modified")
	}
}

// noETagIndexHandler serves the body with NO ETag header (simulates a CDN that
// strips ETag, SPEC §9.3) — every request gets a fresh 200.
func noETagIndexHandler(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// TestCatalogReloader_NoETagFingerprintDedup: with no ETag the conditional-GET
// gate can't fire (every poll is 200), so the FNV-64 body fingerprint must
// short-circuit the redundant re-parse + re-swap when the bytes are unchanged.
func TestCatalogReloader_NoETagFingerprintDedup(t *testing.T) {
	c := loadContentForTests(t)
	srv := httptest.NewServer(noETagIndexHandler(freshIndexBytes(t)))
	defer srv.Close()

	r := NewCatalogReloader(c, srv.URL, time.Minute)
	ctx := context.Background()

	// First poll: 200, new content → swap.
	changed, err := r.pollOnce(ctx)
	if err != nil || !changed {
		t.Fatalf("first pollOnce: changed=%v err=%v, want changed=true nil", changed, err)
	}
	// Second poll: 200 again (no ETag → no 304), identical body → fingerprint
	// dedup skips the swap.
	changed, err = r.pollOnce(ctx)
	if err != nil {
		t.Fatalf("second pollOnce: %v", err)
	}
	if changed {
		t.Error("second pollOnce re-swapped identical body despite the fingerprint gate")
	}
}

// TestCatalogReloader_KeepsTableOnServerError: a 5xx must be a non-fatal poll
// error that keeps the current table (fail-safe) and does NOT record an ETag.
func TestCatalogReloader_KeepsTableOnServerError(t *testing.T) {
	c := loadContentForTests(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewCatalogReloader(c, srv.URL, time.Minute)
	if _, err := r.pollOnce(context.Background()); err == nil {
		t.Fatal("expected error on 500 response")
	}
	if r.etag != "" {
		t.Errorf("etag recorded despite failed poll: %q", r.etag)
	}
	if _, ok := c.LookupPlantID("Abelia chinensis"); !ok {
		t.Error("server error corrupted the live table")
	}
}

// TestCatalogReloader_KeepsTableOnTruncatedBody: a 200 whose body is valid JSON
// but far too small must be refused by ReloadCatalogNames' truncation guard, the
// poll reported as an error, the table kept, and the ETag NOT recorded — so the
// next tick re-fetches instead of 304-skipping the bad object.
func TestCatalogReloader_KeepsTableOnTruncatedBody(t *testing.T) {
	c := loadContentForTests(t)
	tiny := []byte(`[{"id":"AAA0001","scientific_name":"Abelia chinensis"}]`)
	srv := httptest.NewServer(etagIndexHandler(tiny, `"truncetag"`))
	defer srv.Close()

	r := NewCatalogReloader(c, srv.URL, time.Minute)
	if _, err := r.pollOnce(context.Background()); err == nil {
		t.Fatal("expected truncation guard to fail the poll")
	}
	if r.etag != "" {
		t.Errorf("etag recorded for a rejected payload: %q (would 304-skip the fix)", r.etag)
	}
	if c.currentNames().count < 1000 {
		t.Errorf("truncated body shrank the live table: count now %d", c.currentNames().count)
	}
}
