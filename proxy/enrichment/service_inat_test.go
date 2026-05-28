package enrichment

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestGetOrGenerate_INatOverridesCacheHit pins down the PR #25 fix to the
// symptom user saw on screen: a previously-cached PlantDetail (CommonName
// "Ixia", commonNameSource "plantnet") is now post-patched with the iNat
// preferred_common_name "Prettyface" on the way out, source stays SourceCache,
// and the cached row itself is NOT mutated (override must copy so other
// callers reading the same pointer don't see "Prettyface" written back).
func TestGetOrGenerate_INatOverridesCacheHit(t *testing.T) {
	inat, done := inatStub(t, inatBodyPrettyface)
	defer done()

	cache := NewCache(10, time.Hour)
	pre := &proxy.PlantDetail{CommonName: "Ixia", CommonNameSource: "plantnet"}
	cache.Set("triteleia ixioides", pre)

	svc := NewService(nil, nil, nil, cache, inat)
	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceCache {
		t.Errorf("source = %q, want %q (cache hit path)", src, SourceCache)
	}
	if got.CommonName != "Prettyface" {
		t.Errorf("commonName = %q, want Prettyface (iNat override)", got.CommonName)
	}
	if got.CommonNameSource != "inaturalist" {
		t.Errorf("commonNameSource = %q, want inaturalist", got.CommonNameSource)
	}

	// The cached row must NOT have been mutated — overrideINat copies before
	// writing. If this fails, every other caller reading the same pointer
	// would see the override leaked back.
	cached, ok := cache.Get("triteleia ixioides")
	if !ok {
		t.Fatal("cached row vanished after GetOrGenerate")
	}
	if cached.CommonName != "Ixia" {
		t.Errorf("cached row mutated: commonName = %q, want Ixia (override must copy)", cached.CommonName)
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

// TestGetOrGenerate_INatMissKeepsCachedName covers the best-effort guarantee:
// when iNat returns a mismatched / empty / non-200 result (or no client is
// wired), the original common name passes through untouched. This is the
// "iNat never blocks identify/enrichment" safety net.
func TestGetOrGenerate_INatMissKeepsCachedName(t *testing.T) {
	// iNat returns a DIFFERENT taxon (fuzzy q matched another plant) — the
	// name-match guard in PreferredCommonName rejects it.
	inat, done := inatStub(t, `{"results":[
		{"name":"Ixia polystachya","rank":"species","preferred_common_name":"Wand Flower"}
	]}`)
	defer done()

	cache := NewCache(10, time.Hour)
	pre := &proxy.PlantDetail{CommonName: "Original", CommonNameSource: "plantnet"}
	cache.Set("triteleia ixioides", pre)

	svc := NewService(nil, nil, nil, cache, inat)
	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceCache {
		t.Errorf("source = %q, want %q", src, SourceCache)
	}
	if got.CommonName != "Original" {
		t.Errorf("iNat miss should keep original: commonName = %q, want Original", got.CommonName)
	}
	if got.CommonNameSource != "plantnet" {
		t.Errorf("iNat miss should keep original source: commonNameSource = %q, want plantnet", got.CommonNameSource)
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
	cache.Set("triteleia ixioides", pre)

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
