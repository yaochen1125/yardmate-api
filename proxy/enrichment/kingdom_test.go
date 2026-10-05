package enrichment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

func kPtr(s string) *string { return &s }

func kStr(k *string) string {
	if k == nil {
		return "<nil>"
	}
	return *k
}

// inatCountingStub is inatStub plus a call counter.
func inatCountingStub(t *testing.T, body string) (*proxy.INatClient, *int32, func()) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return &proxy.INatClient{HTTP: srv.Client(), BaseURL: srv.URL}, &calls, srv.Close
}

const inatBodyFlyAgaric = `{"results":[
	{"name":"Amanita muscaria","rank":"species","preferred_common_name":"Fly Agaric","iconic_taxon_name":"Fungi"}
]}`

// foodyDetail is what a careless model returns for a mushroom: a food attribute
// and (legacy v1 shape) culinary / edible uses next to harmless ones.
func foodyDetail(kingdom *string) *proxy.PlantDetail {
	return &proxy.PlantDetail{
		ScientificName:   "Amanita muscaria",
		CommonName:       "Fly agaric",
		CommonNameSource: "llm",
		Kingdom:          kingdom,
		Attributes:       []string{"edible", "shade-tolerant"},
		UsesList: []proxy.UseItem{
			{Icon: "culinary", Text: "Parboiled and eaten in some regions"},
			{Icon: "ornamental", Text: "Iconic red cap"},
			{Icon: "edible", Text: "Edible after preparation"},
		},
	}
}

func assertFungiSanitized(t *testing.T, where string, d *proxy.PlantDetail) {
	t.Helper()
	if d == nil {
		t.Fatalf("%s: nil detail", where)
	}
	if kStr(d.Kingdom) != "Fungi" {
		t.Errorf("%s: kingdom = %s, want Fungi", where, kStr(d.Kingdom))
	}
	if want := []string{"shade-tolerant"}; !reflect.DeepEqual(d.Attributes, want) {
		t.Errorf("%s: attributes = %v, want %v (edible stripped)", where, d.Attributes, want)
	}
	if want := []proxy.UseItem{{Icon: "ornamental", Text: "Iconic red cap"}}; !reflect.DeepEqual(d.UsesList, want) {
		t.Errorf("%s: uses_list = %+v, want %+v (edible/culinary stripped)", where, d.UsesList, want)
	}
}

// iNat (authoritative) says Fungi while the model self-reports Plantae: Fungi
// wins, the master is hard-filtered BEFORE it is persisted, and the kingdom rode
// on the one iNat request the English path already makes.
func TestGetOrGenerate_INatFungiOverridesLLMAndFiltersBeforeInsert(t *testing.T) {
	inat, calls, done := inatCountingStub(t, inatBodyFlyAgaric)
	defer done()
	db := &stubDB{}
	llm := &stubLLM{ret: foodyDetail(kPtr("Plantae"))}
	content := loadTestContent(t)
	svc := NewService(content, db, llm, NewCache(10, time.Hour), inat)

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Amanita muscaria"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceSupabaseMissGenerate {
		t.Errorf("source = %q, want %q", src, SourceSupabaseMissGenerate)
	}
	assertFungiSanitized(t, "response", got)
	if got.CommonName != "Fly Agaric" || got.CommonNameSource != "inaturalist" {
		t.Errorf("common name = %q/%q, want the iNat name (fungi are in scope now)", got.CommonName, got.CommonNameSource)
	}
	if len(db.insertCalls) != 1 {
		t.Fatalf("insert calls = %d, want 1", len(db.insertCalls))
	}
	assertFungiSanitized(t, "persisted row", db.insertCalls[0].Data)
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("iNat calls = %d, want 1 (kingdom must reuse the existing lookup)", n)
	}
	// The resolved kingdom is remembered for /v1/identify + /v1/diagnose.
	if k := content.KingdomFor("Amanita muscaria"); kStr(k) != "Fungi" {
		t.Errorf("content.KingdomFor = %s, want Fungi", kStr(k))
	}
	// Second call is a cache hit: already-finalized row, no further iNat.
	again, src2, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Amanita muscaria"})
	if err != nil || src2 != SourceCache {
		t.Fatalf("second call = (%q, %v), want cache hit", src2, err)
	}
	assertFungiSanitized(t, "cached row", again)
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("iNat calls after cache hit = %d, want 1", n)
	}
}

// No iNat at all: the model's own Fungi self-report is enough to trigger the filter.
func TestGetOrGenerate_LLMSelfReportFungiFiltersWithoutINat(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{ret: foodyDetail(kPtr("Fungi"))}
	svc := NewService(nil, db, llm, NewCache(10, time.Hour), nil)

	got, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Amanita muscaria"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	assertFungiSanitized(t, "response", got)
	assertFungiSanitized(t, "persisted row", db.insertCalls[0].Data)
}

// The filter must not touch plants: an edible plant keeps its tags.
func TestGetOrGenerate_PlantKeepsEdibleContent(t *testing.T) {
	inat, _, done := inatCountingStub(t, `{"results":[
		{"name":"Ocimum basilicum","rank":"species","preferred_common_name":"Sweet Basil","iconic_taxon_name":"Plantae"}
	]}`)
	defer done()
	d := foodyDetail(kPtr("Plantae"))
	d.ScientificName = "Ocimum fakeum"
	db := &stubDB{}
	svc := NewService(nil, db, &stubLLM{ret: d}, NewCache(10, time.Hour), inat)

	got, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Ocimum basilicum"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if kStr(got.Kingdom) != "Plantae" {
		t.Errorf("kingdom = %s, want Plantae", kStr(got.Kingdom))
	}
	if len(got.Attributes) != 2 || len(got.UsesList) != 3 {
		t.Errorf("plant content was filtered: attrs=%v uses=%+v", got.Attributes, got.UsesList)
	}
}

// iNat failing (5xx / timeout) must never fail or stall the request: the model's
// self-report stands, and with no source at all kingdom is simply nil.
func TestGetOrGenerate_INatFailureDoesNotAffectMainFlow(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // outlive the client timeout, but let the server shut down
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hang.Close()
	defer close(release)

	cases := []struct {
		name string
		inat *proxy.INatClient
		lang string
		llm  *string
		want string
	}{
		{"en 5xx, llm plantae", &proxy.INatClient{HTTP: fail.Client(), BaseURL: fail.URL}, "en", kPtr("Plantae"), "Plantae"},
		{"en 5xx, llm silent", &proxy.INatClient{HTTP: fail.Client(), BaseURL: fail.URL}, "en", nil, "<nil>"},
		{"en 5xx, llm fungi", &proxy.INatClient{HTTP: fail.Client(), BaseURL: fail.URL}, "en", kPtr("Fungi"), "Fungi"},
		{"en timeout, llm plantae", &proxy.INatClient{HTTP: &http.Client{Timeout: 50 * time.Millisecond}, BaseURL: hang.URL}, "en", kPtr("Plantae"), "Plantae"},
		{"de timeout, llm fungi", &proxy.INatClient{HTTP: &http.Client{Timeout: 50 * time.Millisecond}, BaseURL: hang.URL}, "de", kPtr("Fungi"), "Fungi"},
		{"de 5xx, llm silent", &proxy.INatClient{HTTP: fail.Client(), BaseURL: fail.URL}, "de", nil, "<nil>"},
	}
	for _, c := range cases {
		d := &proxy.PlantDetail{ScientificName: "Fakeus plantus", CommonName: "Fake", Kingdom: c.llm}
		db := &stubDB{}
		svc := NewService(nil, db, &stubLLM{ret: d}, NewCache(10, time.Hour), c.inat)

		start := time.Now()
		got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Fakeus plantus", Lang: c.lang})
		if err != nil {
			t.Errorf("%s: GetOrGenerate failed: %v", c.name, err)
			continue
		}
		if src != SourceSupabaseMissGenerate || len(db.insertCalls) != 1 {
			t.Errorf("%s: source=%q inserts=%d, want a normal generate+insert", c.name, src, len(db.insertCalls))
		}
		if kStr(got.Kingdom) != c.want {
			t.Errorf("%s: kingdom = %s, want %s", c.name, kStr(got.Kingdom), c.want)
		}
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("%s: took %v — a failing iNat must not stall the request", c.name, el)
		}
	}
}

// Non-English requests never asked iNat before (its common name is English). The
// kingdom lookup now runs alongside generation: the master gets the authoritative
// kingdom, but the localized common name is NOT replaced by the English iNat one.
func TestGetOrGenerate_NonEnglishMasterGetsINatKingdom(t *testing.T) {
	inat, calls, done := inatCountingStub(t, inatBodyFlyAgaric)
	defer done()
	d := foodyDetail(nil) // model did not self-report
	d.CommonName = "Fliegenpilz"
	db := &stubDB{}
	svc := NewService(nil, db, &stubLLM{ret: d}, NewCache(10, time.Hour), inat)

	got, _, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Amanita muscaria", Lang: "de"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	assertFungiSanitized(t, "response", got)
	assertFungiSanitized(t, "persisted row", db.insertCalls[0].Data)
	if got.CommonName != "Fliegenpilz" || got.CommonNameSource != "llm" {
		t.Errorf("common name = %q/%q, want the localized LLM name untouched", got.CommonName, got.CommonNameSource)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Errorf("iNat calls = %d, want exactly 1", n)
	}
}

// A row cached in Supabase before `kingdom` existed (a legacy v1 row with culinary
// uses) is stamped + filtered on read once iNat identifies it as a fungus — no
// backfill needed for English — and the stored pointer is not mutated.
func TestGetOrGenerate_LegacySupabaseRowFilteredOnRead(t *testing.T) {
	inat, _, done := inatCountingStub(t, inatBodyFlyAgaric)
	defer done()
	stored := foodyDetail(nil)
	db := &stubDB{lookupQ: []dbLookupResult{{pd: stored}}}
	llm := &stubLLM{}
	svc := NewService(nil, db, llm, NewCache(10, time.Hour), inat)

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Amanita muscaria"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceSupabaseHit {
		t.Errorf("source = %q, want %q", src, SourceSupabaseHit)
	}
	assertFungiSanitized(t, "response", got)
	if len(llm.calls) != 0 || len(db.insertCalls) != 0 {
		t.Errorf("a Supabase hit must not regenerate/insert (llm=%d inserts=%d)", len(llm.calls), len(db.insertCalls))
	}
	if stored.Kingdom != nil || len(stored.UsesList) != 3 || len(stored.Attributes) != 2 {
		t.Errorf("the DB layer's row was mutated in place: %+v", stored)
	}
}

// Non-English request served from the English fallback row: no iNat is consulted,
// but a stored Fungi kingdom still triggers the filter.
func TestGetOrGenerate_EnglishFallbackRowWithStoredFungiFiltered(t *testing.T) {
	inat, calls, done := inatCountingStub(t, inatBodyFlyAgaric)
	defer done()
	db := &stubDB{lookupQ: []dbLookupResult{{pd: nil}, {pd: foodyDetail(kPtr("Fungi"))}}}
	svc := NewService(nil, db, &stubLLM{}, NewCache(10, time.Hour), inat)

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Amanita muscaria", Lang: "ja"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceSupabaseFallbackEn {
		t.Errorf("source = %q, want %q", src, SourceSupabaseFallbackEn)
	}
	assertFungiSanitized(t, "response", got)
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("iNat calls = %d, want 0 (no extra round-trip on a non-English DB hit)", n)
	}
}

// Catalog rows are curated and never pass through the fungi filter / iNat.
func TestGetOrGenerate_CatalogHitUntouchedByKingdom(t *testing.T) {
	inat, calls, done := inatCountingStub(t, `{"results":[
		{"name":"Abelia chinensis","rank":"species","preferred_common_name":"X","iconic_taxon_name":"Fungi"}
	]}`)
	defer done()
	content := loadTestContent(t)
	svc := NewService(content, nil, nil, NewCache(10, time.Hour), inat)

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Abelia chinensis"})
	if err != nil || src != SourceCatalog {
		t.Fatalf("GetOrGenerate = (%q, %v), want catalog hit", src, err)
	}
	if got.Kingdom != nil {
		t.Errorf("catalog kingdom = %s, want nil (plants_detail.json ships none)", *got.Kingdom)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("iNat calls = %d, want 0 on a catalog hit", n)
	}
}

// --- prompt / schema ---

func TestBuildResponseSchema_KingdomRequiredEnum(t *testing.T) {
	for _, lang := range []string{"en", "de"} {
		s := buildResponseSchema(lang)
		props := s["properties"].(map[string]any)
		k, ok := props["kingdom"].(map[string]any)
		if !ok {
			t.Fatalf("lang=%s: kingdom property missing", lang)
		}
		if got := k["enum"].([]string); !reflect.DeepEqual(got, []string{"Plantae", "Fungi", "Other"}) {
			t.Errorf("lang=%s: kingdom enum = %v", lang, got)
		}
		// Canonical token — must never pick up the "write this in <lang>" directive.
		if desc := k["description"].(string); strings.Contains(desc, "Write this field in") {
			t.Errorf("lang=%s: kingdom description carries a localization directive: %q", lang, desc)
		}
		required := s["required"].([]string)
		if len(required) != len(props) {
			t.Errorf("lang=%s: required=%d properties=%d — strict json_schema needs every property required", lang, len(required), len(props))
		}
		if !strings.Contains(strings.Join(required, ","), "kingdom") {
			t.Errorf("lang=%s: kingdom not required", lang)
		}
	}
}

func TestSystemPrompt_FungiSafetyRule(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans"} {
		p := systemPrompt(lang)
		for _, want := range []string{"FUNGI SAFETY", `"kingdom"`, "NEVER present the organism as food", `Do NOT include "edible"`} {
			if !strings.Contains(p, want) {
				t.Errorf("lang=%s: system prompt missing %q", lang, want)
			}
		}
		// systemPrompt is a fmt format string: a stray % would render as "%!…".
		if strings.Contains(p, "%!") {
			t.Errorf("lang=%s: system prompt has a broken format verb: %q", lang, p)
		}
	}
}

func TestPromptVersion_BumpedForKingdom(t *testing.T) {
	if PromptVersion != "v6" {
		t.Errorf("PromptVersion = %q, want v6 (kingdom + fungi safety revision)", PromptVersion)
	}
	// The native_region stale marker must still sort BEFORE the current version.
	if !(nativeRegionStaleVersion < PromptVersion) {
		t.Errorf("nativeRegionStaleVersion %q must sort before PromptVersion %q", nativeRegionStaleVersion, PromptVersion)
	}
}

// Generate canonicalizes the model's self-report: "Other" (and anything
// unexpected) becomes nil → JSON null, never a third wire value.
func TestGenerate_NormalizesSelfReportedKingdom(t *testing.T) {
	cases := map[string]string{`"Fungi"`: "Fungi", `"Plantae"`: "Plantae", `"Other"`: "<nil>", `"fungi"`: "Fungi", `null`: "<nil>"}
	for raw, want := range cases {
		content, _ := json.Marshal(`{"scientific_name":"Amanita muscaria","common_name":"Fly agaric","kingdom":` + raw + `}`)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-k","choices":[{"message":{"content":` + string(content) + `}}]}`))
		}))
		c := &LLMClient{APIKey: "k", Endpoint: srv.URL, Model: "t", HTTP: srv.Client()}
		pd, _, err := c.Generate(context.Background(), "Amanita muscaria", "", "en")
		srv.Close()
		if err != nil {
			t.Fatalf("kingdom=%s: Generate: %v", raw, err)
		}
		if got := kStr(pd.Kingdom); got != want {
			t.Errorf("kingdom=%s: got %s, want %s", raw, got, want)
		}
	}
}

// A translated copy inherits the master's kingdom verbatim (it is not prose).
func TestTranslate_PreservesKingdom(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chatcmpl-t","choices":[{"message":{"content":"{\"common_name\":\"Fliegenpilz\"}"}}]}`))
	}))
	defer srv.Close()
	c := &LLMClient{APIKey: "k", Endpoint: srv.URL, Model: "t", HTTP: srv.Client()}
	out, _, err := c.Translate(context.Background(), &proxy.PlantDetail{CommonName: "Fly agaric", Kingdom: kPtr("Fungi")}, "de")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if kStr(out.Kingdom) != "Fungi" || out.CommonName != "Fliegenpilz" {
		t.Errorf("translated = kingdom %s / name %q, want Fungi / Fliegenpilz", kStr(out.Kingdom), out.CommonName)
	}
}
