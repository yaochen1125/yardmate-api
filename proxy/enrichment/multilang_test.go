package enrichment

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

func mlContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestNormalizeLang pins the lang mapping (SPEC §1.3 + §9 #21). The critical
// case is Chinese: simplified vs traditional must NOT collapse to "zh".
func TestNormalizeLang(t *testing.T) {
	cases := map[string]string{
		"":        "en",
		"   ":     "en",
		"en":      "en",
		"EN":      "en",
		"en-US":   "en",
		"de":      "de",
		"de-DE":   "de",
		"fr-CA":   "fr",
		"es-419":  "es",
		"it":      "it",
		"ja":      "ja",
		"ko":      "ko",
		"vi":      "vi",
		"pt":      "pt",
		"pt-BR":   "pt",
		"pt-PT":   "pt",
		"zh":      "zh-Hans",
		"zh-Hans": "zh-Hans",
		"zh-CN":   "zh-Hans",
		"zh-SG":   "zh-Hans",
		"zh-Hant": "zh-Hant",
		"zh-TW":   "zh-Hant",
		"zh-HK":   "zh-Hant",
		"zh-MO":   "zh-Hant",
		"zh_Hant": "zh-Hant", // underscore form
		"xx":      "en",
		"klingon": "en",
	}
	for in, want := range cases {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBackfillTargets pins the ordering: English first (the fallback pivot),
// source language excluded (SPEC §7).
func TestBackfillTargets(t *testing.T) {
	// English master: en absent from targets.
	en := backfillTargets("en")
	if mlContains(en, "en") {
		t.Error("english master must not re-target en")
	}
	if len(en) != len(SupportedLangs)-1 {
		t.Errorf("en targets = %d, want %d", len(en), len(SupportedLangs)-1)
	}

	// Non-English master: en is FIRST, source excluded.
	ja := backfillTargets("ja")
	if len(ja) == 0 || ja[0] != "en" {
		t.Errorf("non-en master must backfill en first, got %v", ja)
	}
	if mlContains(ja, "ja") {
		t.Error("source lang ja must be excluded from its own backfill")
	}
	if len(ja) != len(SupportedLangs)-1 {
		t.Errorf("ja targets = %d, want %d", len(ja), len(SupportedLangs)-1)
	}
}

// TestApplyProse_CopiesNonProseVerbatim is the §9 #17 guard: translation must
// copy every enum / number / color-key field byte-for-byte from the master and
// only swap the prose fields.
func TestApplyProse_CopiesNonProseVerbatim(t *testing.T) {
	wn := 2
	master := &proxy.PlantDetail{
		ScientificName:     "Monstera adansonii",
		CommonName:         "Swiss cheese vine",
		Description:        "A climbing aroid.",
		NameOrigin:         "From Greek.",
		Sunlight:           4,
		WateringNote:       &wn,
		FlowerColor:        []string{"white"},
		FoliageColor:       []string{"green"},
		Locations:          []string{"Indoor"},
		Soil:               []string{"loamy"},
		Attributes:         []string{"climbing"},
		CommonDiseasesList: []string{"L08"},
		Genus:              "Monstera",
	}
	tr := map[string]string{
		"common_name": "ハニカズラ",
		"description": "つる性のサトイモ科。",
		"name_origin": "ギリシャ語由来。",
	}
	out := applyProse(master, tr)

	// Prose swapped.
	if out.CommonName != "ハニカズラ" || out.Description != "つる性のサトイモ科。" || out.NameOrigin != "ギリシャ語由来。" {
		t.Errorf("prose not applied: %+v", out)
	}
	// Non-prose verbatim.
	if out.Sunlight != 4 || out.Genus != "Monstera" {
		t.Errorf("number/genus changed: sunlight=%d genus=%q", out.Sunlight, out.Genus)
	}
	if out.WateringNote == nil || *out.WateringNote != 2 {
		t.Errorf("watering_note changed: %v", out.WateringNote)
	}
	for _, pair := range []struct {
		got  []string
		want []string
		name string
	}{
		{out.FlowerColor, []string{"white"}, "flower_color"},
		{out.FoliageColor, []string{"green"}, "foliage_color"},
		{out.Locations, []string{"Indoor"}, "locations"},
		{out.Soil, []string{"loamy"}, "soil"},
		{out.Attributes, []string{"climbing"}, "attributes"},
		{out.CommonDiseasesList, []string{"L08"}, "common_diseases_list"},
	} {
		if !reflect.DeepEqual(pair.got, pair.want) {
			t.Errorf("%s must be copied verbatim (English keys): got %v want %v", pair.name, pair.got, pair.want)
		}
	}
	// Master must NOT be mutated (shallow copy semantics).
	if master.CommonName != "Swiss cheese vine" {
		t.Errorf("master mutated: %q", master.CommonName)
	}
}

// TestCollectProse only gathers the 7 prose fields and skips empty ones.
func TestCollectProse(t *testing.T) {
	p := &proxy.PlantDetail{
		CommonName:  "Name",
		Description: "Desc",
		BloomTip:    "", // empty → skipped
		// FruitPeriodShort nil → skipped
	}
	got := collectProse(p)
	if _, ok := got["common_name"]; !ok {
		t.Error("common_name missing")
	}
	if _, ok := got["description"]; !ok {
		t.Error("description missing")
	}
	if _, ok := got["bloom_tip"]; ok {
		t.Error("empty bloom_tip should be skipped")
	}
	if _, ok := got["fruit_period_short"]; ok {
		t.Error("nil fruit_period_short should be skipped")
	}
}

// TestBuildTranslateSchema is strict, string-typed, sorted, and requires exactly
// the prose keys present.
func TestBuildTranslateSchema(t *testing.T) {
	sch := buildTranslateSchema(map[string]string{"description": "y", "common_name": "x"})
	if sch["additionalProperties"] != false {
		t.Error("schema must be strict (additionalProperties:false)")
	}
	req, _ := sch["required"].([]string)
	if !reflect.DeepEqual(req, []string{"common_name", "description"}) {
		t.Errorf("required must be sorted prose keys, got %v", req)
	}
}

// TestSystemPrompt_NonEnglish names the target language and keeps color keys English.
func TestSystemPrompt_NonEnglish(t *testing.T) {
	p := systemPrompt("ja")
	if !strings.Contains(p, "Japanese") {
		t.Error("non-en prompt must name the target language")
	}
	if !strings.Contains(p, "lowercased ENGLISH") {
		t.Error("non-en prompt must keep color names as English keys")
	}
	if strings.Contains(p, "Reply in English ONLY") {
		t.Error("non-en prompt must not force English-only")
	}
}

// TestGetOrGenerate_NonEnglishGeneratesMasterInLang: a non-en miss generates the
// master in that language and persists it under (normalized, lang) — after first
// probing the exact lang then the English fallback.
func TestGetOrGenerate_NonEnglishGeneratesMasterInLang(t *testing.T) {
	db := &stubDB{} // all Lookups miss; Insert inserted=true
	llm := &stubLLM{ret: &proxy.PlantDetail{ScientificName: "Madeup ja", CommonName: "X"}}
	svc := NewService(nil, db, llm, NewCache(10, time.Hour), nil)

	_, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup ja", Lang: "ja"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if source != SourceSupabaseMissGenerate {
		t.Errorf("source = %q, want %q", source, SourceSupabaseMissGenerate)
	}
	if len(llm.calls) != 1 || llm.calls[0].Lang != "ja" {
		t.Errorf("Generate must be called with lang=ja, got %+v", llm.calls)
	}
	if len(db.insertCalls) != 1 || db.insertCalls[0].Lang != "ja" {
		t.Errorf("Insert must carry lang=ja, got %+v", db.insertCalls)
	}
	// Exact-lang probe, then English fallback probe, before generating.
	if !reflect.DeepEqual(db.lookupLangs, []string{"ja", "en"}) {
		t.Errorf("lookup langs = %v, want [ja en]", db.lookupLangs)
	}
}

// TestGetOrGenerate_FallsBackToEnglish: exact lang missing but English present →
// serve English, no LLM call, and do NOT cache under the requested-lang key.
func TestGetOrGenerate_FallsBackToEnglish(t *testing.T) {
	enRow := &proxy.PlantDetail{ScientificName: "Madeup", CommonName: "English name"}
	db := &stubDB{lookupQ: []dbLookupResult{
		{pd: nil},   // ja miss
		{pd: enRow}, // en hit
	}}
	llm := &stubLLM{} // must NOT be called
	cache := NewCache(10, time.Hour)
	svc := NewService(nil, db, llm, cache, nil)

	got, source, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup", Lang: "ja"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if source != SourceSupabaseFallbackEn {
		t.Errorf("source = %q, want %q", source, SourceSupabaseFallbackEn)
	}
	if got != enRow {
		t.Error("must return the English row")
	}
	if len(llm.calls) != 0 {
		t.Error("LLM must not generate when the English fallback exists")
	}
	precise := proxy.NormalizeScientificNamePrecise("Madeup")
	if _, ok := cache.Get(precise + "|ja"); ok {
		t.Error("English fallback must NOT be cached under the requested-lang (ja) key — SPEC §9 #20")
	}
	if _, ok := cache.Get(precise + "|en"); !ok {
		t.Error("English fallback should be cached under the en key")
	}
}

// TestGetOrGenerate_CacheKeyIncludesLang: two languages of the same plant must
// not collide in the LRU (SPEC §9 #19).
func TestGetOrGenerate_CacheKeyIncludesLang(t *testing.T) {
	cache := NewCache(10, time.Hour)
	precise := proxy.NormalizeScientificNamePrecise("Madeup plant")
	cache.Set(precise+"|en", &proxy.PlantDetail{CommonName: "EN"})
	cache.Set(precise+"|ja", &proxy.PlantDetail{CommonName: "JA"})
	svc := NewService(nil, &stubDB{}, &stubLLM{}, cache, nil)

	gotEn, _, _ := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup plant", Lang: "en"})
	gotJa, _, _ := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Madeup plant", Lang: "ja"})
	if gotEn.CommonName != "EN" {
		t.Errorf("en cache hit = %q, want EN", gotEn.CommonName)
	}
	if gotJa.CommonName != "JA" {
		t.Errorf("ja cache hit = %q, want JA (lang must be part of the cache key)", gotJa.CommonName)
	}
}

// TestGetOrGenerate_NonEnglishSkipsINatOverride: the iNat English-name override
// is gated to English rows (SPEC §7 common_name B). A non-en row keeps its
// localized name even when iNat would have a match.
func TestGetOrGenerate_NonEnglishSkipsINatOverride(t *testing.T) {
	inat, done := inatStub(t, inatBodyPrettyface)
	defer done()

	cache := NewCache(10, time.Hour)
	cache.Set("triteleia ixioides|ja", &proxy.PlantDetail{CommonName: "プリティフェイス", CommonNameSource: "llm"})
	svc := NewService(nil, nil, nil, cache, inat)

	got, src, err := svc.GetOrGenerate(context.Background(), Request{ScientificName: "Triteleia ixioides", Lang: "ja"})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if src != SourceCache {
		t.Errorf("source = %q, want %q", src, SourceCache)
	}
	if got.CommonName != "プリティフェイス" || got.CommonNameSource != "llm" {
		t.Errorf("non-en row must not be iNat-overridden, got name=%q source=%q", got.CommonName, got.CommonNameSource)
	}
}

// TestBackfiller_RunTranslatesAllTargetsEnglishFirst: run() translates the
// master into every target language (English first) and persists each with the
// translated source tag + the right lang.
func TestBackfiller_RunTranslatesAllTargetsEnglishFirst(t *testing.T) {
	db := &stubDB{}
	llm := &stubLLM{}
	b := &Backfiller{db: db, llm: llm} // call run() directly; no worker goroutines

	master := &proxy.PlantDetail{ScientificName: "Madeup", CommonName: "Name", Description: "Desc"}
	b.run(BackfillJob{
		Normalized:     "madeup",
		ScientificName: "Madeup",
		CommonHint:     "Name",
		SourceLang:     "ja",
		Master:         master,
	})

	want := backfillTargets("ja")
	if len(llm.translateCalls) != len(want) {
		t.Fatalf("translate calls = %v, want %d targets", llm.translateCalls, len(want))
	}
	if llm.translateCalls[0] != "en" {
		t.Errorf("first translate target = %q, want en (fallback pivot first)", llm.translateCalls[0])
	}
	if len(db.insertCalls) != len(want) {
		t.Fatalf("insert calls = %d, want %d", len(db.insertCalls), len(want))
	}
	for i, p := range db.insertCalls {
		if p.Lang != want[i] {
			t.Errorf("insert[%d] lang = %q, want %q", i, p.Lang, want[i])
		}
		if p.Source != TranslatedSourceTag {
			t.Errorf("insert[%d] source = %q, want %q", i, p.Source, TranslatedSourceTag)
		}
		if p.Normalized != "madeup" {
			t.Errorf("insert[%d] normalized = %q, want madeup", i, p.Normalized)
		}
	}
}
