package enrichment

import (
	"context"
	"fmt"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy"
)

func sevStrptr(s string) *string { return &s }

// sevGrp builds a treatment/prevention group: empty label → nil *string
// (ungrouped), sev "" → nil severity. Steps is a single ref so the group is
// non-empty. (Names are sev-prefixed to avoid colliding with other enrichment
// test helpers in this package.)
func sevGrp(label, sev string) proxy.DiseaseStepGroup {
	var lp, sp *string
	if label != "" {
		lp = sevStrptr(label)
	}
	if sev != "" {
		sp = sevStrptr(sev)
	}
	return proxy.DiseaseStepGroup{Label: lp, Severity: sp, Steps: []proxy.DiseaseStep{{Ref: "S01"}}}
}

func sevRow(norm, lang string, treat, prev []proxy.DiseaseStepGroup) DiseaseSeverityRow {
	return DiseaseSeverityRow{
		Normalized: norm,
		Lang:       lang,
		CatalogID:  "O1",
		Detail: &proxy.StructuredDiseaseDetail{
			Treatment:  proxy.DiseaseStepGroups{Groups: treat},
			Prevention: proxy.DiseaseStepGroups{Groups: prev},
		},
	}
}

type severityUpdate struct {
	normalized string
	lang       string
	detail     *proxy.StructuredDiseaseDetail
	version    string
}

type stubSeverityDB struct {
	rows    []DiseaseSeverityRow
	updates []severityUpdate
	failOn  string // lang whose UPDATE should error (transient-failure path)
	vanish  string // lang whose UPDATE matched 0 rows (vanished path)
}

func (s *stubSeverityDB) ListDiseaseSeverityBackfillRows(_ context.Context) ([]DiseaseSeverityRow, error) {
	return s.rows, nil
}

func (s *stubSeverityDB) UpdateDiseaseSeverity(_ context.Context, normalized, lang string, detail *proxy.StructuredDiseaseDetail, version string) (int64, error) {
	if s.failOn == lang {
		return 0, fmt.Errorf("boom")
	}
	if s.vanish == lang {
		return 0, nil
	}
	s.updates = append(s.updates, severityUpdate{normalized, lang, detail, version})
	return 1, nil
}

func sevOf(g proxy.DiseaseStepGroup) string {
	if g.Severity == nil {
		return "<nil>"
	}
	return *g.Severity
}

func updateFor(db *stubSeverityDB, lang string) *severityUpdate {
	for i := range db.updates {
		if db.updates[i].lang == lang {
			return &db.updates[i]
		}
	}
	return nil
}

// English labels drive the per-group target; that target is stamped onto EVERY
// language row positionally — including the Spanish row whose own labels are
// unsniffable. This is the core invariant the badge fix needs.
func TestRunDiseaseSeverityBackfill_FillsAllLanguagesFromEnglish(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("blight", "en",
			[]proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For severe cases", "")},
			[]proxy.DiseaseStepGroup{sevGrp("", "")}),
		sevRow("blight", "es",
			[]proxy.DiseaseStepGroup{sevGrp("Para casos leves", ""), sevGrp("Para casos graves", "")},
			[]proxy.DiseaseStepGroup{sevGrp("", "")}),
	}}

	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Updated != 2 || rep.NoEnglish != 0 || rep.Failed != 0 || rep.Undetermined != 0 || rep.Mismatch != 0 {
		t.Fatalf("report = %+v, want Updated=2 NoEnglish=0 Failed=0 Undetermined=0 Mismatch=0", rep)
	}
	if len(db.updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(db.updates))
	}
	for _, u := range db.updates {
		if u.version != DiseasePromptVersion {
			t.Errorf("lang=%s version=%q, want %q", u.lang, u.version, DiseasePromptVersion)
		}
		tg := u.detail.Treatment.Groups
		// The Spanish row's own labels are unsniffable — proving the value came
		// from the ENGLISH-derived target, positionally.
		if sevOf(tg[0]) != "mild" || sevOf(tg[1]) != "severe" {
			t.Errorf("lang=%s treatment severity = [%s,%s], want [mild,severe]", u.lang, sevOf(tg[0]), sevOf(tg[1]))
		}
		if sevOf(u.detail.Prevention.Groups[0]) != "<nil>" {
			t.Errorf("lang=%s prevention severity = %s, want <nil> (ungrouped)", u.lang, sevOf(u.detail.Prevention.Groups[0]))
		}
	}
}

// A v3 English row already carries severity; it is used as the target verbatim,
// and a sibling missing it gets filled while the English row is not rewritten.
func TestRunDiseaseSeverityBackfill_UsesExistingEnglishSeverityField(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		// English already filled (v3); label deliberately unsniffable to prove the
		// existing field — not the label — is the source.
		sevRow("rust", "en", []proxy.DiseaseStepGroup{sevGrp("Early stage", "mild")}, nil),
		sevRow("rust", "fr", []proxy.DiseaseStepGroup{sevGrp("Stade précoce", "")}, nil),
	}}

	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// One group → not a severity split; sectionUndetermined is false even though
	// the label is unsniffable, because the existing field supplies the target.
	if rep.Updated != 1 || rep.AlreadyOK != 1 || rep.Undetermined != 0 {
		t.Fatalf("report = %+v, want Updated=1 AlreadyOK=1 Undetermined=0", rep)
	}
	if u := updateFor(db, "fr"); u == nil || sevOf(u.detail.Treatment.Groups[0]) != "mild" {
		t.Fatalf("expected fr filled with mild from English field, got %+v", db.updates)
	}
	if updateFor(db, "en") != nil {
		t.Errorf("English row should not be rewritten (already correct)")
	}
}

// No English row → the disease is skipped (counts NoEnglish), nothing written.
func TestRunDiseaseSeverityBackfill_NoEnglishRowSkipped(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("mildew", "de", []proxy.DiseaseStepGroup{sevGrp("Leichte Fälle", ""), sevGrp("Schwere Fälle", "")}, nil),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.NoEnglish != 1 || rep.Updated != 0 || len(db.updates) != 0 {
		t.Fatalf("report = %+v, updates=%d; want NoEnglish=1 Updated=0 no writes", rep, len(db.updates))
	}
}

// Already-conformant rows produce no writes (idempotent re-run).
func TestRunDiseaseSeverityBackfill_Idempotent(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("spot", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", "mild"), sevGrp("For severe cases", "severe")}, nil),
		sevRow("spot", "ja", []proxy.DiseaseStepGroup{sevGrp("軽症", "mild"), sevGrp("重症", "severe")}, nil),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Updated != 0 || rep.AlreadyOK != 2 || len(db.updates) != 0 {
		t.Fatalf("report = %+v, updates=%d; want Updated=0 AlreadyOK=2 no writes", rep, len(db.updates))
	}
}

// An ungrouped disease (single empty-label group) never gets a severity stamp and
// is not flagged Undetermined (one group is not a severity split).
func TestRunDiseaseSeverityBackfill_UngroupedNoOp(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("generic", "en", []proxy.DiseaseStepGroup{sevGrp("", "")}, []proxy.DiseaseStepGroup{sevGrp("", "")}),
		sevRow("generic", "it", []proxy.DiseaseStepGroup{sevGrp("", "")}, []proxy.DiseaseStepGroup{sevGrp("", "")}),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Updated != 0 || rep.Undetermined != 0 || len(db.updates) != 0 {
		t.Fatalf("report = %+v, updates=%d; want no writes / no Undetermined for ungrouped disease", rep, len(db.updates))
	}
}

// A severity SPLIT (>=2 groups) whose English labels lack the mild/severe keyword
// is surfaced as Undetermined, NOT silently folded into AlreadyOK. Partially
// keyworded → the classifiable group is still filled.
func TestRunDiseaseSeverityBackfill_UndeterminedSurfaced(t *testing.T) {
	// Fully unsniffable English labels → no fill, flagged Undetermined.
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("canker", "en", []proxy.DiseaseStepGroup{sevGrp("For early infection", ""), sevGrp("For advanced infection", "")}, nil),
		sevRow("canker", "es", []proxy.DiseaseStepGroup{sevGrp("Infección temprana", ""), sevGrp("Infección avanzada", "")}, nil),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Undetermined != 1 || rep.Updated != 0 || len(db.updates) != 0 {
		t.Fatalf("fully-unsniffable: report = %+v, updates=%d; want Undetermined=1 Updated=0 no writes", rep, len(db.updates))
	}

	// Partially keyworded English: group 0 sniffs mild, group 1 is unsniffable →
	// still flagged Undetermined, but group 0 gets filled on every language.
	db2 := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("scab", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For advanced cases", "")}, nil),
		sevRow("scab", "ko", []proxy.DiseaseStepGroup{sevGrp("경미", ""), sevGrp("진행", "")}, nil),
	}}
	rep2, err := RunDiseaseSeverityBackfill(context.Background(), db2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep2.Undetermined != 1 || rep2.Updated != 2 {
		t.Fatalf("partial: report = %+v, want Undetermined=1 Updated=2", rep2)
	}
	for _, lang := range []string{"en", "ko"} {
		u := updateFor(db2, lang)
		if u == nil {
			t.Fatalf("partial: expected %s update", lang)
		}
		if sevOf(u.detail.Treatment.Groups[0]) != "mild" || sevOf(u.detail.Treatment.Groups[1]) != "<nil>" {
			t.Errorf("partial lang=%s severity = [%s,%s], want [mild,<nil>]", lang,
				sevOf(u.detail.Treatment.Groups[0]), sevOf(u.detail.Treatment.Groups[1]))
		}
	}
}

// A sibling whose group COUNT diverges from the English row is left unfilled
// (Mismatch) rather than positionally mislabeled — the one place a wrong badge
// could otherwise be produced.
func TestRunDiseaseSeverityBackfill_GroupCountMismatchNotMislabeled(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("anthrac", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For severe cases", "")}, nil),
		// Corrupt/edited sibling: only ONE treatment group where English has two.
		sevRow("anthrac", "pt", []proxy.DiseaseStepGroup{sevGrp("Casos graves", "")}, nil),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Mismatch != 1 {
		t.Fatalf("report = %+v, want Mismatch=1", rep)
	}
	if u := updateFor(db, "pt"); u != nil {
		t.Errorf("pt row must NOT be updated (count mismatch → no positional mislabel), got %+v", u.detail.Treatment.Groups)
	}
	if u := updateFor(db, "en"); u == nil {
		t.Errorf("en row (matching count) should still be filled")
	}
}

// An existing severity is NEVER overwritten — even when it disagrees with the
// English-derived target. Only nil groups are filled.
func TestRunDiseaseSeverityBackfill_NeverOverwritesExisting(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("leafspot", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For severe cases", "")}, nil),
		// group 0 already (inconsistently) "severe"; group 1 nil.
		sevRow("leafspot", "zh-Hans", []proxy.DiseaseStepGroup{sevGrp("轻症", "severe"), sevGrp("重症", "")}, nil),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	u := updateFor(db, "zh-Hans")
	if u == nil {
		t.Fatalf("expected zh-Hans update (group 1 filled), report=%+v", rep)
	}
	if sevOf(u.detail.Treatment.Groups[0]) != "severe" {
		t.Errorf("group 0 = %s, want severe (existing value, NOT overwritten by target mild)", sevOf(u.detail.Treatment.Groups[0]))
	}
	if sevOf(u.detail.Treatment.Groups[1]) != "severe" {
		t.Errorf("group 1 = %s, want severe (filled from English target)", sevOf(u.detail.Treatment.Groups[1]))
	}
}

// The Vanished path: UPDATE matched 0 rows (deleted between list and update) is
// counted benign, not Updated and not Failed.
func TestRunDiseaseSeverityBackfill_VanishedCounted(t *testing.T) {
	db := &stubSeverityDB{
		vanish: "es",
		rows: []DiseaseSeverityRow{
			sevRow("wilt", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For severe cases", "")}, nil),
			sevRow("wilt", "es", []proxy.DiseaseStepGroup{sevGrp("Leves", ""), sevGrp("Graves", "")}, nil),
		},
	}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Vanished != 1 || rep.Updated != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want Vanished=1 Updated=1 Failed=0", rep)
	}
}

// A failed UPDATE is counted and does not abort the run.
func TestRunDiseaseSeverityBackfill_UpdateFailureCounted(t *testing.T) {
	db := &stubSeverityDB{
		failOn: "es",
		rows: []DiseaseSeverityRow{
			sevRow("blast", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For severe cases", "")}, nil),
			sevRow("blast", "es", []proxy.DiseaseStepGroup{sevGrp("Leves", ""), sevGrp("Graves", "")}, nil),
		},
	}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Failed != 1 || rep.Updated != 1 {
		t.Fatalf("report = %+v, want Failed=1 Updated=1 (en patched, es errored)", rep)
	}
}

// Multiple diseases in one run are handled independently: one fills, one is
// skipped for no English row — neither affects the other's outcome.
func TestRunDiseaseSeverityBackfill_MultipleDiseasesIndependent(t *testing.T) {
	db := &stubSeverityDB{rows: []DiseaseSeverityRow{
		sevRow("fillme", "en", []proxy.DiseaseStepGroup{sevGrp("For mild cases", ""), sevGrp("For severe cases", "")}, nil),
		sevRow("fillme", "fr", []proxy.DiseaseStepGroup{sevGrp("Légers", ""), sevGrp("Graves", "")}, nil),
		sevRow("noeng", "de", []proxy.DiseaseStepGroup{sevGrp("Leicht", ""), sevGrp("Schwer", "")}, nil),
	}}
	rep, err := RunDiseaseSeverityBackfill(context.Background(), db)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rep.Diseases != 2 || rep.Updated != 2 || rep.NoEnglish != 1 {
		t.Fatalf("report = %+v, want Diseases=2 Updated=2 NoEnglish=1", rep)
	}
	if updateFor(db, "de") != nil {
		t.Errorf("noeng/de must not be updated (no English row)")
	}
}

func TestSniffSeverity(t *testing.T) {
	cases := []struct {
		label *string
		want  string
	}{
		{sevStrptr("For mild cases"), "mild"},
		{sevStrptr("For severe cases (advanced)"), "severe"},
		{sevStrptr("For mild cases (under 50% of leaves affected)"), "mild"},
		{sevStrptr("SEVERE infections"), "severe"},
		{sevStrptr("General care"), ""},
		{sevStrptr("mild but can turn severe"), ""}, // ambiguous → no badge
		{nil, ""},
		{sevStrptr(""), ""},
	}
	for _, c := range cases {
		got := sniffSeverity(c.label)
		if got != c.want {
			label := "<nil>"
			if c.label != nil {
				label = *c.label
			}
			t.Errorf("sniffSeverity(%q) = %q, want %q", label, got, c.want)
		}
	}
}
