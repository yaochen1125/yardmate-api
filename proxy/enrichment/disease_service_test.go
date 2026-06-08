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
	secondLookupDetail *proxy.StructuredDiseaseDetail
	secondLookupID     string
}

func (m *mockDiseaseDB) LookupDisease(_ context.Context, _ string) (*proxy.StructuredDiseaseDetail, string, error) {
	m.lookupCalls++
	if m.lookupCalls >= 2 && m.secondLookupDetail != nil {
		return m.secondLookupDetail, m.secondLookupID, nil
	}
	return m.lookupDetail, m.lookupID, m.lookupErr
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

func (m *mockDiseaseLLM) Generate(_ context.Context, _, _ string, _, _ []proxy.DiseaseNameRef) (*diseaseGenResult, string, error) {
	m.calls++
	return m.result, "req-1", m.err
}

// sampleGen references real S/K ids plus one bogus id each (dropped on back-fill).
func sampleGen() *diseaseGenResult {
	return &diseaseGenResult{
		Name:             "Drought Stress",
		ShortDescription: "Leaves wilt from insufficient water.",
		SymptomAnalysis:  "Browning, wilting leaves.",
		Cause:            "Underwatering.",
		Treatment: diseaseGenGroups{Groups: []diseaseGenGroup{
			{Label: "For mild cases", StepRefs: []string{"S01", "S99", "S02"}}, // S99 bogus
		}},
		HomeRemedyRefs: []string{"K01", "K99"}, // K99 bogus
		Prevention:     diseaseGenGroups{Groups: []diseaseGenGroup{{Label: "", StepRefs: []string{"S03"}}}},
	}
}

func TestDiseaseService_GenerateBackfillInsert(t *testing.T) {
	content := diseaseTestContent(t)
	db := &mockDiseaseDB{insertID: "O5", inserted: true}
	llm := &mockDiseaseLLM{result: sampleGen()}
	svc := NewDiseaseService(content, db, llm, NewDiseaseCache(0, 0))

	detail, catalogID, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "Rosa")
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

	if _, _, err := svc.GetOrGenerate(context.Background(), "Drought Stress", ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	llmAfter1, lookupAfter1 := llm.calls, db.lookupCalls
	_, id, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "")
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

	detail, id, err := svc.GetOrGenerate(context.Background(), "Some Disease", "")
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

	detail, id, err := svc.GetOrGenerate(context.Background(), "Race Disease", "")
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

	detail, id, err := svc.GetOrGenerate(context.Background(), "Drought Stress", "")
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
	if _, _, err := svc.GetOrGenerate(context.Background(), "   ", ""); !errors.Is(err, ErrInvalidDiseaseName) {
		t.Errorf("err = %v, want ErrInvalidDiseaseName", err)
	}
}
