package enrichment

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// inatStub returns a *proxy.INatClient pointed at a test server that replies
// with the given JSON body and 200 — mirrors the helper in proxy/inat_test.go
// (the production INatClient lives in the proxy package).
func inatStub(t *testing.T, body string) (*proxy.INatClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return &proxy.INatClient{HTTP: srv.Client(), BaseURL: srv.URL}, srv.Close
}

// inatBodyPrettyface fakes iNat returning the curated species-level name for
// the worked example from PR #24 (Triteleia ixioides → "Prettyface", not the
// upstream engine's "Ixia").
const inatBodyPrettyface = `{"results":[
	{"name":"Triteleia ixioides","rank":"species","preferred_common_name":"Prettyface"}
]}`

// TestGetOrGenerate_CacheHitGetsAuthoritativeNameOnce: a cache hit is served
// with the authoritative name (SPEC §7 common_name C — cached rows carry the
// LLM name), and the name lookup is itself cached, so a second hit does NOT
// contact iNat again (hot path stays free after the first resolution).
func TestGetOrGenerate_CacheHitGetsAuthoritativeNameOnce(t *testing.T) {
	var inatCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&inatCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(inatBodyPrettyface))
	}))
	defer srv.Close()
	inat := &proxy.INatClient{HTTP: srv.Client(), BaseURL: srv.URL}

	cache := NewCache(10, time.Hour)
	pre := &proxy.PlantDetail{CommonName: "Ixia", CommonNameSource: "plantnet"}
	cache.Set("triteleia ixioides|en", pre)

	svc := NewService(nil, nil, nil, cache, inat)
	for i := 0; i < 2; i++ {
		got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides"})
		if err != nil {
			t.Fatalf("GetOrGenerate: %v", err)
		}
		if src != SourceCache {
			t.Errorf("source = %q, want %q (cache hit path)", src, SourceCache)
		}
		if got.CommonName != "Prettyface" || got.CommonNameSource != NameSourceINat {
			t.Errorf("call %d: name=%q source=%q, want Prettyface/%s", i, got.CommonName, got.CommonNameSource, NameSourceINat)
		}
	}
	if pre.CommonName != "Ixia" {
		t.Errorf("cached pointer mutated: %q", pre.CommonName)
	}
	if n := atomic.LoadInt32(&inatCalls); n != 1 {
		t.Errorf("iNat contacted %d times across two cache hits, want 1 (name cache)", n)
	}
}

// TestGetOrGenerate_INatDoesNotOverrideCatalogHit pins the invariant that
// curated 1522 catalog rows are NEVER replaced by iNat — matches the
// identify-side priority (catalog > iNat > upstream) from PR #24.
// Even when iNat returns a string for the same scientific_name, a catalog
// hit returns the curated common_name unchanged.
func TestGetOrGenerate_INatDoesNotOverrideCatalogHit(t *testing.T) {
	inat, done := inatStub(t, `{"results":[
		{"name":"Abelia chinensis","rank":"species","preferred_common_name":"FAKE_OVERRIDE"}
	]}`)
	defer done()

	content := loadTestContent(t)
	cache := NewCache(10, time.Hour)
	svc := NewService(content, nil, nil, cache, inat)

	// AAA0001 = "Abelia chinensis" in plants_index.json — must resolve via
	// Step 1 catalog (no DB / LLM needed).
	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Abelia chinensis"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceCatalog {
		t.Fatalf("source = %q, want %q (catalog hit path)", src, SourceCatalog)
	}
	if got.CommonName == "FAKE_OVERRIDE" {
		t.Error("catalog hit was overridden by iNat — invariant broken (catalog must win, PR #24 priority)")
	}
	if got.CommonNameSource == "inaturalist" {
		t.Error("catalog hit commonNameSource was rewritten to inaturalist — must stay curated")
	}
}

// TestGetOrGenerate_INatNoMatchFallsBackToScientificName: iNat ANSWERED with
// no matching taxon (fuzzy q hit another plant) and no other source knows the
// plant → the scientific name is shown, never the LLM's invented name.
func TestGetOrGenerate_INatNoMatchFallsBackToScientificName(t *testing.T) {
	inat, done := inatStub(t, `{"results":[
		{"name":"Ixia polystachya","rank":"species","preferred_common_name":"Wand Flower"}
	]}`)
	defer done()

	cache := NewCache(10, time.Hour)
	cache.Set("triteleia ixioides|en", &proxy.PlantDetail{CommonName: "Original", CommonNameSource: "plantnet"})

	svc := NewService(nil, nil, nil, cache, inat)
	got, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if got.CommonName != "Triteleia ixioides" || got.CommonNameSource != NameSourceScientificName {
		t.Errorf("name=%q source=%q, want scientific-name fallback", got.CommonName, got.CommonNameSource)
	}
}

// TestGetOrGenerate_INatOutageKeepsRowName: iNat could NOT be asked (5xx) →
// the row's own name passes through untouched and nothing is cached, so the
// next request retries (a transient outage must not flip names to Latin).
func TestGetOrGenerate_INatOutageKeepsRowName(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	inat := &proxy.INatClient{HTTP: srv.Client(), BaseURL: srv.URL}

	cache := NewCache(10, time.Hour)
	cache.Set("triteleia ixioides|en", &proxy.PlantDetail{CommonName: "Original", CommonNameSource: "plantnet"})
	svc := NewService(nil, nil, nil, cache, inat)
	for i := 0; i < 2; i++ {
		got, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides"})
		if err != nil {
			t.Fatalf("GetOrGenerate: %v", err)
		}
		if got.CommonName != "Original" || got.CommonNameSource != "plantnet" {
			t.Errorf("outage must keep row name, got %q/%q", got.CommonName, got.CommonNameSource)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("iNat calls = %d, want 2 (unresolved results must not be cached)", n)
	}
}

// TestGetOrGenerate_INatNoOverrideOnSecondCatalogCall is the regression test
// for the PR #25 self-review P0: catalog hits were being LRU-cached, so the
// SECOND same-name request would come back as SourceCache and be eligible for
// the iNat override — silently breaking the catalog > iNat priority. Fix:
// catalog hits no longer write LRU. This test does two identical catalog
// requests under the SAME Service and asserts the second one is still curated
// (commonName != iNat fake, commonNameSource != "inaturalist").
func TestGetOrGenerate_INatNoOverrideOnSecondCatalogCall(t *testing.T) {
	inat, done := inatStub(t, `{"results":[
		{"name":"Abelia chinensis","rank":"species","preferred_common_name":"FAKE_OVERRIDE"}
	]}`)
	defer done()

	content := loadTestContent(t)
	cache := NewCache(10, time.Hour)
	svc := NewService(content, nil, nil, cache, inat)
	req := Request{ScientificName: "Abelia chinensis"}

	// First call: catalog hit, NOT cached by design.
	_, src1, err := svc.GetOrGenerate(context.Background(), req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if src1 != SourceCatalog {
		t.Fatalf("first call source = %q, want %q", src1, SourceCatalog)
	}

	// Second call: MUST be catalog again (NOT cache), and MUST NOT be
	// overridden by iNat. If catalog leaks into LRU, this comes back as
	// SourceCache and overrideINat patches it — invariant broken.
	got2, src2, err := svc.GetOrGenerate(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if src2 != SourceCatalog {
		t.Errorf("second call source = %q, want %q (catalog must NOT leak into LRU)", src2, SourceCatalog)
	}
	if got2.CommonName == "FAKE_OVERRIDE" {
		t.Error("second catalog call was overridden by iNat — PR #24 priority broken")
	}
	if got2.CommonNameSource == "inaturalist" {
		t.Error("second catalog call commonNameSource rewritten to inaturalist — must stay curated")
	}
}

// TestGetOrGenerate_NilINatIsNoOp confirms that wiring inat=nil keeps the
// service behaving exactly like before this PR (graceful disable when iNat is
// not configured / unreachable on startup).
func TestGetOrGenerate_NilINatIsNoOp(t *testing.T) {
	cache := NewCache(10, time.Hour)
	pre := &proxy.PlantDetail{CommonName: "Original", CommonNameSource: "plantnet"}
	cache.Set("triteleia ixioides|en", pre)

	svc := NewService(nil, nil, nil, cache, nil) // nil inat
	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceCache {
		t.Errorf("source = %q, want %q", src, SourceCache)
	}
	if got.CommonName != "Original" {
		t.Errorf("nil iNat should be a no-op: commonName = %q, want Original", got.CommonName)
	}
}
