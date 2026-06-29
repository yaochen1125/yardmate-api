package enrichment

import (
	"context"
	"errors"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy"
)

func diseaseTestContent(t *testing.T) *proxy.ContentIndex {
	t.Helper()
	c, err := proxy.LoadContent()
	if err != nil {
		t.Fatalf("LoadContent: %v", err)
	}
	return c
}

type mockDiseaseDB struct {
	lookupDetail       *proxy.StructuredDiseaseDetail
	lookupID           string
	lookupErr          error
	insertID           string
	inserted           bool
	insertErr          error
	lookupCalls        int
	insertCalls        int
	lookupAnyCalls     int
	secondLookupDetail *proxy.StructuredDiseaseDetail
	secondLookupID     string
}

func (m *mockDiseaseDB) LookupDisease(_ context.Context, _, _ string) (*proxy.StructuredDiseaseDetail, string, error) {
	m.lookupCalls++
	if m.lookupCalls >= 2 && m.secondLookupDetail != nil {
		return m.secondLookupDetail, m.secondLookupID, nil
	}
	return m.lookupDetail, m.lookupID, m.lookupErr
}

// LookupDiseaseAny defaults to a miss (no other-language master) so the existing
// tests exercise the generate path; the en-fallback / race tests rely on the
// exact-lang LookupDisease above. lookupErr (DB-down) is surfaced here too so
// "DB down still generates" continues to fall through to Generate.
func (m *mockDiseaseDB) LookupDiseaseAny(_ context.Context, _ string) (*proxy.StructuredDiseaseDetail, string, string, error) {
	m.lookupAnyCalls++
	if m.lookupErr != nil {
		return nil, "", "", m.lookupErr
	}
	return nil, "", "", nil
}

func (m *mockDiseaseDB) InsertDisease(_ context.Context, _ DiseaseInsertParams) (string, bool, error) {
	m.insertCalls++
	return m.insertID, m.inserted, m.insertErr
}

type mockDiseaseLLM struct {
	result *diseaseGenResult
	err    error
	calls  int
}

func (m *mockDiseaseLLM) Generate(_ context.Context, _, _, _ string, _, _ []proxy.DiseaseNameRef) (*diseaseGenResult, string, error) {
	m.calls++
	return m.result, "req-1", m.err
}

func (m *mockDiseaseLLM) DiseaseTranslate(_ context.Context, source *proxy.StructuredDiseaseDetail, _ string) (*proxy.StructuredDiseaseDetail, string, error) {
	if source == nil {
		return nil, "", nil
	}
	cp := *source
	return &cp, "req-tr", nil
}

// sampleGen references real S/K ids plus one bogus id each (dropped on back-fill).
func sampleGen() *diseaseGenResult {
	return &diseaseGenResult{
		Name:             "Drought Stress",
		ShortDescription: "Leaves wilt from insufficient water.",
		SymptomAnalysis:  "Browning, wilting leaves.",
		Cause:            "Underwatering.",
		Treatment: diseaseGenGroups{Groups: []diseaseGenGroup{
			{Label: "For mild cases", Severity: "mild", StepRefs: []string{"S01", "S99", "S02"}}, // S99 bogus
		}},
		HomeRemedyRefs: []string{"K01", "K99"}, // K99 bogus
		Prevention:     diseaseGenGroups{Groups: []diseaseGenGroup{{Label: "", Severity: "", StepRefs: []string{"S03"}}}},
	}
}

func TestDiseaseService_GenerateBackfillInsert(t *testing.T) {
	content := diseaseTestContent(t)
	db := &mockDiseaseDB{insertID: "O5", inserted: true}
	llm := &mockDiseaseLLM{result: sampleGen()}
	svc := NewDiseaseService(content, db, llm, NewDiseaseCache(0, 0))

	detail, catalogID, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "Rosa", "en")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if catalogID != "O5" {
		t.Errorf("catalogID = %q, want O5", catalogID)
	}
	if detail == nil {
		t.Fatal("nil detail")
	}
	// back-fill: S99 dropped → group keeps S01, S02 with pool titles + num 1,2.
	if len(detail.Treatment.Groups) != 1 || len(detail.Treatment.Groups[0].Steps) != 2 {
		t.Fatalf("treatment = %+v", detail.Treatment.Groups)
	}
	s0 := detail.Treatment.Groups[0].Steps[0]
	if s0.Ref != "S01" || s0.Title == "" || s0.Body == "" || s0.Num != 1 {
		t.Errorf("step0 = %+v (want ref S01, non-empty title/body, num 1)", s0)
	}
	s1 := detail.Treatment.Groups[0].Steps[1]
	if s1.Ref != "S02" || s1.Num != 2 {
		t.Errorf("step1 = %+v (want ref S02, num 2)", s1)
	}
	// severity is carried verbatim from the gen result (language-independent badge
	// key); the ungrouped prevention group ("" severity) backfills to nil.
	if sv := detail.Treatment.Groups[0].Severity; sv == nil || *sv != "mild" {
		t.Errorf("treatment severity = %v, want \"mild\"", sv)
	}
	if sv := detail.Prevention.Groups[0].Severity; sv != nil {
		t.Errorf("prevention severity = %v, want nil (ungrouped)", sv)
	}
	// home remedies: K99 dropped → only K01 (with pool fields).
	if len(detail.HomeRemedies) != 1 || detail.HomeRemedies[0].Ref != "K01" || detail.HomeRemedies[0].Title == "" {
		t.Errorf("homeRemedies = %+v (want only K01 with title)", detail.HomeRemedies)
	}
	if len(detail.Prevention.Groups) != 1 || len(detail.Prevention.Groups[0].Steps) != 1 {
		t.Errorf("prevention = %+v", detail.Prevention.Groups)
	}
	if detail.ShortDescription == "" || detail.SymptomAnalysis == "" {
		t.Errorf("prose not carried: %+v", detail)
	}
}

func TestDiseaseService_CacheHit(t *testing.T) {
	content := diseaseTestContent(t)
	db := &mockDiseaseDB{insertID: "O5", inserted: true}
	llm := &mockDiseaseLLM{result: sampleGen()}
	svc := NewDiseaseService(content, db, llm, NewDiseaseCache(0, 0))

	if _, _, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "", "en"); err != nil {
		t.Fatalf("first: %v", err)
	}
	llmAfter1, lookupAfter1 := llm.calls, db.lookupCalls
	_, id, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "", "en")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if id != "O5" {
		t.Errorf("id = %q, want O5", id)
	}
	if llm.calls != llmAfter1 || db.lookupCalls != lookupAfter1 {
		t.Errorf("cache miss on 2nd call: llm %d→%d, lookup %d→%d", llmAfter1, llm.calls, lookupAfter1, db.lookupCalls)
	}
}

func TestDiseaseService_DBHit(t *testing.T) {
	content := diseaseTestContent(t)
	hit := &proxy.StructuredDiseaseDetail{ShortDescription: "cached"}
	db := &mockDiseaseDB{lookupDetail: hit, lookupID: "O2"}
	llm := &mockDiseaseLLM{result: sampleGen()}
	svc := NewDiseaseService(content, db, llm, NewDiseaseCache(0, 0))

	detail, id, err := svc.GetOrGenerate(context.Background(), "Some Disease", "", "en")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "O2" || detail != hit {
		t.Errorf("got (%v,%q), want (hit,O2)", detail, id)
	}
	if llm.calls != 0 {
		t.Errorf("LLM called on DB hit (calls=%d)", llm.calls)
	}
}

func TestDiseaseService_RaceLoser(t *testing.T) {
	content := diseaseTestContent(t)
	winner := &proxy.StructuredDiseaseDetail{ShortDescription: "winner"}
	db := &mockDiseaseDB{inserted: false, secondLookupDetail: winner, secondLookupID: "O3"}
	llm := &mockDiseaseLLM{result: sampleGen()}
	svc := NewDiseaseService(content, db, llm, NewDiseaseCache(0, 0))

	detail, id, err := svc.GetOrGenerate(context.Background(), "Race Disease", "", "en")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "O3" || detail != winner {
		t.Errorf("got (%v,%q), want (winner,O3)", detail, id)
	}
}

func TestDiseaseService_DBDownStillGenerates(t *testing.T) {
	content := diseaseTestContent(t)
	db := &mockDiseaseDB{lookupErr: ErrDBUnavailable, insertErr: ErrDBUnavailable}
	llm := &mockDiseaseLLM{result: sampleGen()}
	svc := NewDiseaseService(content, db, llm, NewDiseaseCache(0, 0))

	detail, id, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "", "en")
	if err != nil {
		t.Fatalf("DB down must not fail: %v", err)
	}
	if detail == nil {
		t.Fatal("nil detail on DB down")
	}
	if id != "" {
		t.Errorf("id = %q, want empty (no O id minted when DB down)", id)
	}
}

func TestDiseaseService_InvalidName(t *testing.T) {
	content := diseaseTestContent(t)
	svc := NewDiseaseService(content, &mockDiseaseDB{}, &mockDiseaseLLM{result: sampleGen()}, NewDiseaseCache(0, 0))
	if _, _, err := svc.GetOrGenerate(context.Background(), "   ", "", "en"); !errors.Is(err, ErrInvalidDiseaseName) {
		t.Errorf("err = %v, want ErrInvalidDiseaseName", err)
	}
}

// The translation pass localizes group LABELS but must leave SEVERITY untouched
// (it's a language-independent badge key, like step refs). Guards the iOS PR #661
// invariant on the enrichment translate path (applyDiseaseProse → applyGroupLabels).
func TestApplyDiseaseProse_TranslatesLabelKeepsSeverity(t *testing.T) {
	mild, severe := "mild", "severe"
	enMild, enSevere := "For mild cases", "For severe cases"
	src := &proxy.StructuredDiseaseDetail{
		ShortDescription: "Leaves wilt from drought.",
		Treatment: proxy.DiseaseStepGroups{Groups: []proxy.DiseaseStepGroup{
			{Label: &enMild, Severity: &mild, Steps: []proxy.DiseaseStep{{Ref: "S01"}}},
			{Label: &enSevere, Severity: &severe, Steps: []proxy.DiseaseStep{{Ref: "S02"}}},
		}},
	}
	tr := map[string]string{
		diseaseProseShort:                    "Las hojas se marchitan.",
		diseaseGroupLabelKey("treatment", 0): "Para casos leves",
		diseaseGroupLabelKey("treatment", 1): "Para casos graves",
	}

	out := applyDiseaseProse(src, tr)

	if out.Treatment.Groups[0].Label == nil || *out.Treatment.Groups[0].Label != "Para casos leves" {
		t.Errorf("label[0] = %v, want translated 'Para casos leves'", out.Treatment.Groups[0].Label)
	}
	if sv := out.Treatment.Groups[0].Severity; sv == nil || *sv != "mild" {
		t.Errorf("severity[0] = %v, want \"mild\" (untouched by translation)", sv)
	}
	if sv := out.Treatment.Groups[1].Severity; sv == nil || *sv != "severe" {
		t.Errorf("severity[1] = %v, want \"severe\" (untouched by translation)", sv)
	}
	// The master's in-memory copy must not be mutated by the translate.
	if *src.Treatment.Groups[0].Label != "For mild cases" {
		t.Errorf("source label mutated: %q", *src.Treatment.Groups[0].Label)
	}
}
