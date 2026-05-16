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
	lookupCalls []string
	lookupQ     []dbLookupResult
	insertCalls []InsertParams
	insertQ     []dbInsertResult
}

type dbLookupResult struct {
	pd  *proxy.PlantDetail
	err error
}

type dbInsertResult struct {
	inserted bool
	err      error
}

func (s *stubDB) Lookup(_ context.Context, normalized string) (*proxy.PlantDetail, error) {
	s.lookupCalls = append(s.lookupCalls, normalized)
	if len(s.lookupQ) == 0 {
		return nil, nil
	}
	r := s.lookupQ[0]
	s.lookupQ = s.lookupQ[1:]
	return r.pd, r.err
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
	calls []struct{ Sci, Common string }
	ret   *proxy.PlantDetail
	err   error
}

func (s *stubLLM) Generate(_ context.Context, sci, common string) (*proxy.PlantDetail, string, error) {
	s.calls = append(s.calls, struct{ Sci, Common string }{sci, common})
	return s.ret, "stub-chatcmpl-id", s.err
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
	svc := NewService(nil, nil, nil, NewCache(10, time.Hour))
	for _, name := range []string{"", "   ", "\t\n", "12345", "!!!"} {
		_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: name})
		if !errors.Is(err, ErrInvalidScientificName) {
			t.Errorf("name=%q: expected ErrInvalidScientificName, got %v", name, err)
		}
	}
}

func TestService_ScientificNameTooLong(t *testing.T) {
	svc := NewService(nil, nil, nil, NewCache(10, time.Hour))
	longName := strings.Repeat("a", 201)
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: longName})
	if !errors.Is(err, ErrScientificNameTooLong) {
		t.Errorf("expected ErrScientificNameTooLong, got %v", err)
	}
}

func TestService_Path0_CacheHit_ShortCircuitsEverything(t *testing.T) {
	cache := NewCache(10, time.Hour)
	cached := &proxy.PlantDetail{ScientificName: "Cached species"}
	key := proxy.NormalizeScientificName("Cached species")
	cache.Set(key, cached)

	db := &stubDB{}
	llm := &stubLLM{}
	svc := NewService(loadTestContent(t), db, llm, cache)

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

func TestService_Path1_CatalogHit_PopulatesCache(t *testing.T) {
	cache := NewCache(10, time.Hour)
	content := loadTestContent(t)
	// "Abelia chinensis" is the first row of the curated 1522 catalog (id AAA0001).
	svc := NewService(content, nil, nil, cache)
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
	if cache.Len() != 1 {
		t.Errorf("expected cache len 1, got %d", cache.Len())
	}
	// Second call must hit cache (no DB needed because content is still set).
	if _, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Abelia chinensis"}); err != nil {
		t.Fatalf("second call err: %v", err)
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
	svc := NewService(loadTestContent(t), db, llm, cache)

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
	svc := NewService(loadTestContent(t), db, llm, cache)

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
	svc := NewService(loadTestContent(t), db, llm, cache)

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
	svc := NewService(loadTestContent(t), db, nil, NewCache(10, time.Hour))
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup err"})
	if !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("expected ErrDBUnavailable, got %v", err)
	}
}

func TestService_LLMError_Propagates(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{err: ErrEnrichmentUnavailable}
	svc := NewService(loadTestContent(t), db, llm, NewCache(10, time.Hour))
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup llmerr"})
	if !errors.Is(err, ErrEnrichmentUnavailable) {
		t.Errorf("expected ErrEnrichmentUnavailable, got %v", err)
	}
}

func TestService_NoDB_NoCatalog_ReturnsEnrichmentUnavailable(t *testing.T) {
	svc := NewService(loadTestContent(t), nil, nil, NewCache(10, time.Hour))
	_, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup nothing"})
	if !errors.Is(err, ErrEnrichmentUnavailable) {
		t.Errorf("expected ErrEnrichmentUnavailable, got %v", err)
	}
}

func TestService_FilterDiseaseIDs_Empty(t *testing.T) {
	svc := NewService(loadTestContent(t), nil, nil, NewCache(10, time.Hour))
	out := svc.filterCatalogDiseaseIDs(nil)
	if out == nil {
		t.Error("expected non-nil empty slice (for JSON [] wire form)")
	}
	if len(out) != 0 {
		t.Errorf("expected length 0, got %d", len(out))
	}
}

func TestService_FilterDiseaseIDs_PreservesOrderDropsUnknowns(t *testing.T) {
	svc := NewService(loadTestContent(t), nil, nil, NewCache(10, time.Hour))
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
