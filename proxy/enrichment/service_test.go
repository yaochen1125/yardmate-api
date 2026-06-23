package enrichment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// stubDB is a sequence-aware ServiceDB stub. Lookups and Inserts pop queued
// responses; running off the end returns zero values (miss / inserted=true).
type stubDB struct {
	lookupCalls   []string
	lookupLangs   []string
	lookupQ       []dbLookupResult
	insertCalls   []InsertParams
	insertQ       []dbInsertResult
	lookupAnyPD   *proxy.PlantDetail // returned by LookupAny (nil → miss → first-caller generate)
	lookupAnyLang string
	lookupAnyErr  error
	lookupAnyHits int
}

type dbLookupResult struct {
	pd  *proxy.PlantDetail
	err error
}

type dbInsertResult struct {
	inserted bool
	err      error
}

func (s *stubDB) Lookup(_ context.Context, normalized, lang string) (*proxy.PlantDetail, error) {
	s.lookupCalls = append(s.lookupCalls, normalized)
	s.lookupLangs = append(s.lookupLangs, lang)
	if len(s.lookupQ) == 0 {
		return nil, nil
	}
	r := s.lookupQ[0]
	s.lookupQ = s.lookupQ[1:]
	return r.pd, r.err
}

func (s *stubDB) LookupAny(_ context.Context, normalized string) (*proxy.PlantDetail, string, error) {
	s.lookupAnyHits++
	return s.lookupAnyPD, s.lookupAnyLang, s.lookupAnyErr
}

func (s *stubDB) Insert(_ context.Context, p InsertParams) (bool, error) {
	s.insertCalls = append(s.insertCalls, p)
	if len(s.insertQ) == 0 {
		return true, nil
	}
	r := s.insertQ[0]
	s.insertQ = s.insertQ[1:]
	return r.inserted, r.err
}

// stubLLM is a ServiceLLM stub.
type stubLLM struct {
	calls          []struct{ Sci, Common, Lang string }
	ret            *proxy.PlantDetail
	err            error
	translateCalls []string           // toLang values, in order
	translateRet   *proxy.PlantDetail // when set, returned instead of a copy of source
	translateErr   error
}

func (s *stubLLM) Generate(_ context.Context, sci, common, lang string) (*proxy.PlantDetail, string, error) {
	s.calls = append(s.calls, struct{ Sci, Common, Lang string }{sci, common, lang})
	return s.ret, "stub-chatcmpl-id", s.err
}

func (s *stubLLM) Translate(_ context.Context, source *proxy.PlantDetail, toLang string) (*proxy.PlantDetail, string, error) {
	s.translateCalls = append(s.translateCalls, toLang)
	if s.translateErr != nil {
		return nil, "", s.translateErr
	}
	if source == nil {
		return nil, "", nil
	}
	cp := *source
	if s.translateRet != nil {
		cp = *s.translateRet
	}
	return &cp, "stub-translate-id", nil
}

func loadTestContent(t *testing.T) *proxy.ContentIndex {
	t.Helper()
	c, err := proxy.LoadContent()
	if err != nil {
		t.Fatalf("load content: %v", err)
	}
	return c
}

func TestService_InvalidScientificName(t *testing.T) {
	svc := NewService(nil, nil, nil, NewCache(10, time.Hour), nil)
	for _, name := range []string{"", "   ", "\t\n", "12345", "!!!"} {
		_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: name})
		if !errors.Is(err, ErrInvalidScientificName) {
			t.Errorf("name=%q: expected ErrInvalidScientificName, got %v", name, err)
		}
	}
}

func TestService_ScientificNameTooLong(t *testing.T) {
	svc := NewService(nil, nil, nil, NewCache(10, time.Hour), nil)
	longName := strings.Repeat("a", 201)
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: longName})
	if !errors.Is(err, ErrScientificNameTooLong) {
		t.Errorf("expected ErrScientificNameTooLong, got %v", err)
	}
}

func TestService_Path0_CacheHit_ShortCircuitsEverything(t *testing.T) {
	cache := NewCache(10, time.Hour)
	cached := &proxy.PlantDetail{ScientificName: "Cached species"}
	key := proxy.NormalizeScientificNamePrecise("Cached species") + "|en"
	cache.Set(key, cached)

	db := &stubDB{}
	llm := &stubLLM{}
	svc := NewService(loadTestContent(t), db, llm, cache, nil)

	got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Cached species"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != cached {
		t.Error("expected same pointer from cache")
	}
	if source != SourceCache {
		t.Errorf("expected SourceCache, got %q", source)
	}
	if len(db.lookupCalls) != 0 {
		t.Error("DB.Lookup should not run on cache hit")
	}
	if len(llm.calls) != 0 {
		t.Error("LLM.Generate should not run on cache hit")
	}
}

func TestService_Path1_CatalogHit_DoesNotPopulateCache(t *testing.T) {
	cache := NewCache(10, time.Hour)
	content := loadTestContent(t)
	// "Abelia chinensis" is the first row of the curated 1522 catalog (id AAA0001).
	svc := NewService(content, nil, nil, cache, nil)
	got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Abelia chinensis"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.PlantDetailID() != "AAA0001" {
		t.Errorf("expected id AAA0001, got %v", got)
	}
	if source != SourceCatalog {
		t.Errorf("expected SourceCatalog, got %q", source)
	}
	// PR #25 P0: catalog hits MUST NOT populate the LRU cache, otherwise they
	// come back as SourceCache on subsequent calls and become eligible for
	// the iNat override — silently breaking the catalog > iNat priority.
	if cache.Len() != 0 {
		t.Errorf("expected cache len 0 (catalog no longer writes LRU), got %d", cache.Len())
	}
	// Second call resolves through catalog again (Step 1), NOT cache.
	_, source2, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Abelia chinensis"})
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if source2 != SourceCatalog {
		t.Errorf("second call source = %q, want %q (must re-resolve through catalog, not LRU)", source2, SourceCatalog)
	}
}

// TestService_Path1_MultiVarietyCatalog_NoCacheCollision guards against a latent
// Step-0 cache-key collision. The five curated Brassica oleracea cultivars all
// fold to "brassica oleracea" under the species-level NormalizeScientificName
// (the Supabase plants_pending PK), but the catalog resolves each to a DISTINCT
// plantId via scientificNameToIDPrecise. If the in-process cache keys on the
// species-level normalization, the SECOND variety queried within the TTL returns
// the FIRST variety's cached *PlantDetail instead of its own catalog entry.
//
// iOS reaches this in practice: Pl@ntNet emits scientificNameWithoutAuthor (e.g.
// "Brassica oleracea var. italica") which the identify Suggestion carries to the
// detail page, which POSTs it verbatim to /v1/plants/enrichment (plantId is not
// trusted — SPEC §1.3). So two different cultivars identified within 30 min hit
// this path. Regression test for the cache collapse.
func TestService_Path1_MultiVarietyCatalog_NoCacheCollision(t *testing.T) {
	cache := NewCache(10, time.Hour)
	content := loadTestContent(t)
	// No DB / LLM: a catalog hit must be fully self-served. A collision would
	// still return a (wrong) answer rather than erroring, so assert on the id.
	svc := NewService(content, nil, nil, cache, nil)

	const (
		italica  = "Brassica oleracea var. italica"  // AAA0207
		acephala = "Brassica oleracea var. acephala" // AAA0203
	)

	// Precondition: the two inputs DO collide at the species level (shared
	// Supabase PK) — this is exactly what made the cache collapse possible.
	if proxy.NormalizeScientificName(italica) != proxy.NormalizeScientificName(acephala) {
		t.Fatalf("test premise broken: expected species-level normalization to collide, got %q vs %q",
			proxy.NormalizeScientificName(italica), proxy.NormalizeScientificName(acephala))
	}

	// First variety: catalog hit, populates the cache.
	got1, src1, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: italica})
	if err != nil {
		t.Fatalf("italica: unexpected err: %v", err)
	}
	if src1 != SourceCatalog {
		t.Errorf("italica: expected SourceCatalog, got %q", src1)
	}
	if got1.PlantDetailID() != "AAA0207" {
		t.Fatalf("italica: expected id AAA0207, got %q", got1.PlantDetailID())
	}

	// Second, DISTINCT variety queried within the TTL. Under a species-level
	// cache key this returned got1 (AAA0207) from SourceCache — the collision.
	// It must resolve its own catalog entry instead.
	got2, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: acephala})
	if err != nil {
		t.Fatalf("acephala: unexpected err: %v", err)
	}
	if got2.PlantDetailID() != "AAA0203" {
		t.Fatalf("acephala: expected id AAA0203, got %q (cache collision returned the first variety?)", got2.PlantDetailID())
	}
	if got1.PlantDetailID() == got2.PlantDetailID() {
		t.Fatalf("collision: both varieties returned the same id %q", got1.PlantDetailID())
	}

	// The first variety, re-queried, must still resolve to itself (the second
	// variety's write must not have overwritten it). PR #26 P0 changed the
	// path: catalog hits no longer populate LRU (catalog > iNat priority
	// invariant), so re-queries go back through Step 1 catalog — not LRU.
	got1b, src1b, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: italica})
	if err != nil {
		t.Fatalf("italica re-query: unexpected err: %v", err)
	}
	if src1b != SourceCatalog {
		t.Errorf("italica re-query: expected SourceCatalog (catalog re-resolves, no LRU per PR #26 P0), got %q", src1b)
	}
	if got1b.PlantDetailID() != "AAA0207" {
		t.Errorf("italica re-query: expected id AAA0207, got %q", got1b.PlantDetailID())
	}
}

func TestService_Path2_SupabaseHit_ReturnsRowAndCaches(t *testing.T) {
	cache := NewCache(10, time.Hour)
	pendingPD := &proxy.PlantDetail{
		ScientificName: "Madeup nonexistent",
		CommonName:     "Madeup",
		Description:    "stored in supabase",
	}
	db := &stubDB{lookupQ: []dbLookupResult{{pd: pendingPD}}}
	llm := &stubLLM{}
	svc := NewService(loadTestContent(t), db, llm, cache, nil)

	got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup nonexistent"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != pendingPD {
		t.Errorf("expected pendingPD pointer, got %v", got)
	}
	if source != SourceSupabaseHit {
		t.Errorf("expected SourceSupabaseHit, got %q", source)
	}
	if len(db.lookupCalls) != 1 {
		t.Errorf("expected 1 lookup, got %d", len(db.lookupCalls))
	}
	if len(llm.calls) != 0 {
		t.Errorf("LLM should not be called on path-2 hit, got %d calls", len(llm.calls))
	}
	if cache.Len() != 1 {
		t.Errorf("expected cache len 1, got %d", cache.Len())
	}
}

func TestService_Path3_FreshGeneration_WhitelistsDiseaseIDs(t *testing.T) {
	cache := NewCache(10, time.Hour)
	llmOut := &proxy.PlantDetail{
		ScientificName:     "Madeup another",
		CommonName:         "from LLM",
		CommonDiseasesList: []string{"L01", "ZZ99", "P05", "BADID"},
	}
	db := &stubDB{} // empty queues -> Lookup miss, Insert inserted=true
	llm := &stubLLM{ret: llmOut}
	svc := NewService(loadTestContent(t), db, llm, cache, nil)

	got, source, err := svc.GetOrGenerate(context.Background(), Request{
		ScientificName: "Madeup another",
		CommonName:     "lmm test",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != llmOut {
		t.Errorf("expected llmOut pointer, got %v", got)
	}
	if source != SourceSupabaseMissGenerate {
		t.Errorf("expected SourceSupabaseMissGenerate, got %q", source)
	}

	// Whitelist: ZZ99 + BADID dropped, L01 + P05 kept (assuming they exist in catalog).
	for _, id := range got.CommonDiseasesList {
		if id == "ZZ99" || id == "BADID" {
			t.Errorf("expected %q dropped by whitelist, still present: %v", id, got.CommonDiseasesList)
		}
	}

	if len(db.insertCalls) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(db.insertCalls))
	}
	ins := db.insertCalls[0]
	if ins.Source == "" || ins.SourceVersion == "" {
		t.Error("InsertParams missing source / source_version tags")
	}
	if ins.GenerationReqID != "stub-chatcmpl-id" {
		t.Errorf("expected generation request id stubbed, got %q", ins.GenerationReqID)
	}
}

func TestService_Path3_ConflictRetriesAndReturnsRaceWinner(t *testing.T) {
	cache := NewCache(10, time.Hour)
	llmOut := &proxy.PlantDetail{ScientificName: "Madeup race"}
	raceWinner := &proxy.PlantDetail{
		ScientificName: "Madeup race",
		Description:    "this is the winner row",
	}
	db := &stubDB{
		lookupQ: []dbLookupResult{
			{pd: nil},        // path-2 miss
			{pd: raceWinner}, // post-conflict re-lookup
		},
		insertQ: []dbInsertResult{
			{inserted: false}, // ON CONFLICT DO NOTHING
		},
	}
	llm := &stubLLM{ret: llmOut}
	svc := NewService(loadTestContent(t), db, llm, cache, nil)

	got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup race"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != raceWinner {
		t.Errorf("expected raceWinner pointer (from re-lookup), got %v", got)
	}
	if source != SourceSupabaseMissGenerateRaceWinner {
		t.Errorf("expected SourceSupabaseMissGenerateRaceWinner, got %q", source)
	}
	if len(db.lookupCalls) != 2 {
		t.Errorf("expected 2 lookups (path-2 miss + race re-lookup), got %d", len(db.lookupCalls))
	}
}

func TestService_DBLookupError_Propagates(t *testing.T) {
	db := &stubDB{lookupQ: []dbLookupResult{{err: ErrDBUnavailable}}}
	svc := NewService(loadTestContent(t), db, nil, NewCache(10, time.Hour), nil)
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup err"})
	if !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("expected ErrDBUnavailable, got %v", err)
	}
}

func TestService_LLMError_Propagates(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{err: ErrEnrichmentUnavailable}
	svc := NewService(loadTestContent(t), db, llm, NewCache(10, time.Hour), nil)
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup llmerr"})
	if !errors.Is(err, ErrEnrichmentUnavailable) {
		t.Errorf("expected ErrEnrichmentUnavailable, got %v", err)
	}
}

func TestService_NoDB_NoCatalog_ReturnsEnrichmentUnavailable(t *testing.T) {
	svc := NewService(loadTestContent(t), nil, nil, NewCache(10, time.Hour), nil)
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup nothing"})
	if !errors.Is(err, ErrEnrichmentUnavailable) {
		t.Errorf("expected ErrEnrichmentUnavailable, got %v", err)
	}
}

func TestService_FilterDiseaseIDs_Empty(t *testing.T) {
	svc := NewService(loadTestContent(t), nil, nil, NewCache(10, time.Hour), nil)
	out := svc.filterCatalogDiseaseIDs(nil)
	if out == nil {
		t.Error("expected non-nil empty slice (for JSON [] wire form)")
	}
	if len(out) != 0 {
		t.Errorf("expected length 0, got %d", len(out))
	}
}

func TestService_FilterDiseaseIDs_PreservesOrderDropsUnknowns(t *testing.T) {
	svc := NewService(loadTestContent(t), nil, nil, NewCache(10, time.Hour), nil)
	// L01 + P05 should be in the 70-entry catalog; ZZ99 should not.
	got := svc.filterCatalogDiseaseIDs([]string{"L01", "ZZ99", "P05"})
	for _, id := range got {
		if id == "ZZ99" {
			t.Error("ZZ99 should be filtered out")
		}
	}
	if len(got) > 3 {
		t.Errorf("output should not grow beyond input length: %v", got)
	}
}
