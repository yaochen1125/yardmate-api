package enrichment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy"
)

type kingdomPatchCall struct {
	normalized, lang string
	patch            map[string]any
}

type stubKingdomDB struct {
	rows    []KingdomBackfillRow
	listErr error
	patchN  int64
	patchFn func(normalized, lang string) (int64, error)
	calls   []kingdomPatchCall
}

func (s *stubKingdomDB) ListKingdomBackfillRows(_ context.Context) ([]KingdomBackfillRow, error) {
	return s.rows, s.listErr
}

func (s *stubKingdomDB) PatchKingdom(_ context.Context, normalized, lang string, patch map[string]any) (int64, error) {
	s.calls = append(s.calls, kingdomPatchCall{normalized, lang, patch})
	if s.patchFn != nil {
		return s.patchFn(normalized, lang)
	}
	return s.patchN, nil
}

// stubResolver answers LookupTaxonErr from a lowercase-name → kingdom map. A name
// in failures errors that many times first (-1 = always) — a rate-limited iNat.
type stubResolver struct {
	kingdoms map[string]string
	failures map[string]int
	queries  []string
	at       []time.Time
}

func (s *stubResolver) LookupTaxonErr(_ context.Context, sciName string) (proxy.INatTaxon, bool, error) {
	s.queries = append(s.queries, sciName)
	s.at = append(s.at, time.Now())
	key := strings.ToLower(sciName)
	if n := s.failures[key]; n != 0 {
		if n > 0 {
			s.failures[key] = n - 1
		}
		return proxy.INatTaxon{}, false, &proxy.INatStatusError{Status: 429}
	}
	k, ok := s.kingdoms[key]
	return proxy.INatTaxon{Kingdom: k}, ok, nil
}

func kingdomBackfillFixture() *stubKingdomDB {
	return &stubKingdomDB{
		patchN: 1,
		rows: []KingdomBackfillRow{
			{Normalized: "amanita muscaria", Lang: "de", ScientificName: "Amanita muscaria", Data: foodyDetail(nil)},
			{Normalized: "amanita muscaria", Lang: "en", ScientificName: "Amanita muscaria", Data: &proxy.PlantDetail{Attributes: []string{"shade-tolerant"}}},
			{Normalized: "monstera deliciosa", Lang: "en", ScientificName: "Monstera deliciosa", Data: foodyDetail(nil)},
			{Normalized: "obscurus unknownus", Lang: "en", ScientificName: "Obscurus unknownus", Data: &proxy.PlantDetail{}},
		},
	}
}

func kingdomBackfillResolver() *stubResolver {
	return &stubResolver{kingdoms: map[string]string{"amanita muscaria": "Fungi", "monstera deliciosa": "Plantae"}}
}

func TestRunKingdomBackfill_Apply(t *testing.T) {
	db := kingdomBackfillFixture()
	res := kingdomBackfillResolver()

	rep, err := RunKingdomBackfill(context.Background(), db, res, true, 0, nil)
	if err != nil {
		t.Fatalf("RunKingdomBackfill: %v", err)
	}
	want := KingdomBackfillReport{Plants: 3, Rows: 4, Fungi: 1, Plantae: 1, Undetermined: 1, Updated: 3, Sanitized: 1}
	if rep != want {
		t.Errorf("report = %+v, want %+v", rep, want)
	}
	if len(db.calls) != 3 {
		t.Fatalf("patch calls = %d, want 3 (the undetermined plant is left alone)", len(db.calls))
	}
	// Fungal row holding food content: kingdom + filtered lists.
	de := db.calls[0]
	if de.normalized != "amanita muscaria" || de.lang != "de" || de.patch["kingdom"] != "Fungi" {
		t.Errorf("first patch = %+v", de)
	}
	if got := de.patch["attributes"]; !reflect.DeepEqual(got, []string{"shade-tolerant"}) {
		t.Errorf("fungal patch attributes = %v", got)
	}
	if got := de.patch["uses_list"]; !reflect.DeepEqual(got, []proxy.UseItem{{Icon: "ornamental", Text: "Iconic red cap"}}) {
		t.Errorf("fungal patch uses_list = %+v", got)
	}
	// Fungal row with nothing to strip: ONLY the kingdom key (surgical patch).
	if en := db.calls[1]; len(en.patch) != 1 || en.patch["kingdom"] != "Fungi" {
		t.Errorf("clean fungal row patch = %+v, want kingdom only", en.patch)
	}
	// A plant keeps its edible content: kingdom only, even though the row has it.
	if pl := db.calls[2]; len(pl.patch) != 1 || pl.patch["kingdom"] != "Plantae" {
		t.Errorf("plant patch = %+v, want kingdom only", pl.patch)
	}
	// One iNat lookup per PLANT (not per language row); the unresolved plant gets
	// a second try with its normalized name.
	wantQueries := []string{"Amanita muscaria", "Monstera deliciosa", "Obscurus unknownus", "obscurus unknownus"}
	if !reflect.DeepEqual(res.queries, wantQueries) {
		t.Errorf("resolver queries = %v, want %v", res.queries, wantQueries)
	}
	// The listed rows must not be mutated by building the patch.
	if db.rows[0].Data.Kingdom != nil || len(db.rows[0].Data.UsesList) != 3 {
		t.Errorf("row data mutated: %+v", db.rows[0].Data)
	}
}

func TestRunKingdomBackfill_DryRunWritesNothing(t *testing.T) {
	db := kingdomBackfillFixture()
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, format) }

	rep, err := RunKingdomBackfill(context.Background(), db, kingdomBackfillResolver(), false, 0, logf)
	if err != nil {
		t.Fatalf("RunKingdomBackfill: %v", err)
	}
	if len(db.calls) != 0 {
		t.Fatalf("dry-run issued %d writes", len(db.calls))
	}
	if rep.Updated != 3 || rep.Sanitized != 1 || rep.Undetermined != 1 {
		t.Errorf("dry-run report = %+v, want the same counts an apply would produce", rep)
	}
	if len(lines) == 0 {
		t.Error("dry-run logged nothing")
	}
}

func TestRunKingdomBackfill_RowFailuresDoNotAbort(t *testing.T) {
	db := kingdomBackfillFixture()
	db.patchFn = func(normalized, lang string) (int64, error) {
		switch {
		case normalized == "amanita muscaria" && lang == "de":
			return 0, errors.New("boom")
		case normalized == "amanita muscaria" && lang == "en":
			return 0, nil // stamped / rejected between list and write
		}
		return 1, nil
	}
	rep, err := RunKingdomBackfill(context.Background(), db, kingdomBackfillResolver(), true, 0, nil)
	if err != nil {
		t.Fatalf("RunKingdomBackfill: %v", err)
	}
	if rep.Failed != 1 || rep.Vanished != 1 || rep.Updated != 1 {
		t.Errorf("report = %+v, want failed1 vanished1 updated1", rep)
	}
}

func TestRunKingdomBackfill_FatalAndGuards(t *testing.T) {
	if _, err := RunKingdomBackfill(context.Background(), nil, kingdomBackfillResolver(), true, 0, nil); err == nil {
		t.Error("nil db: want error")
	}
	if _, err := RunKingdomBackfill(context.Background(), kingdomBackfillFixture(), nil, true, 0, nil); err == nil {
		t.Error("nil resolver: want error")
	}
	db := &stubKingdomDB{listErr: ErrDBUnavailable}
	if _, err := RunKingdomBackfill(context.Background(), db, kingdomBackfillResolver(), true, 0, nil); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("list error = %v, want ErrDBUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db2 := kingdomBackfillFixture()
	if _, err := RunKingdomBackfill(ctx, db2, kingdomBackfillResolver(), true, 0, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx = %v, want context.Canceled", err)
	}
	if len(db2.calls) != 0 {
		t.Errorf("cancelled run wrote %d rows", len(db2.calls))
	}
}

// A rate-limited / failing iNat is NOT "undetermined": transient failures are
// retried, and a plant whose lookup keeps failing is counted as LookupFailed so
// the run is reported incomplete instead of looking like iNat has no such taxon.
func TestRunKingdomBackfill_LookupFailureIsNotUndetermined(t *testing.T) {
	db := kingdomBackfillFixture()
	res := kingdomBackfillResolver()
	res.failures = map[string]int{
		"amanita muscaria":   2,  // 429, 429, then answers → recovered by retry
		"monstera deliciosa": -1, // down for the whole run
	}
	rep, err := RunKingdomBackfill(context.Background(), db, res, true, 0, nil)
	if err != nil {
		t.Fatalf("RunKingdomBackfill: %v", err)
	}
	if rep.Fungi != 1 || rep.LookupFailed != 1 || rep.Undetermined != 1 || rep.Plantae != 0 {
		t.Errorf("report = %+v, want fungi1 lookupFailed1 undetermined1 (only the genuine no-match)", rep)
	}
	if rep.Updated != 2 {
		t.Errorf("updated = %d, want 2 (both Amanita rows after the retry succeeded)", rep.Updated)
	}
	for _, c := range db.calls {
		if c.normalized == "monstera deliciosa" {
			t.Errorf("a plant whose lookup failed was written: %+v", c)
		}
	}
	// 3 tries for Amanita (2 failures + success); Monstera exhausts its attempts
	// on the FIRST name form and must not fall through to the normalized form.
	monstera := 0
	for _, q := range res.queries {
		if strings.EqualFold(q, "monstera deliciosa") {
			monstera++
		}
	}
	if monstera != kingdomBackfillLookupAttempts {
		t.Errorf("monstera lookups = %d, want %d", monstera, kingdomBackfillLookupAttempts)
	}
}

// Every iNat request is paced — including the second (normalized-name) query for
// the SAME plant, and retries back off further.
func TestRunKingdomBackfill_PacesEveryRequest(t *testing.T) {
	const interval = 30 * time.Millisecond
	db := &stubKingdomDB{patchN: 1, rows: []KingdomBackfillRow{
		{Normalized: "obscurus unknownus", Lang: "en", ScientificName: "Obscurus unknownus", Data: &proxy.PlantDetail{}},
		{Normalized: "amanita muscaria", Lang: "en", ScientificName: "Amanita muscaria", Data: &proxy.PlantDetail{}},
	}}
	res := kingdomBackfillResolver()
	res.failures = map[string]int{"amanita muscaria": 1}

	if _, err := RunKingdomBackfill(context.Background(), db, res, false, interval, nil); err != nil {
		t.Fatalf("RunKingdomBackfill: %v", err)
	}
	// Obscurus (stored form), obscurus (normalized form), Amanita (429), Amanita (retry).
	if len(res.at) != 4 {
		t.Fatalf("lookups = %d (%v), want 4", len(res.at), res.queries)
	}
	for i := 1; i < len(res.at); i++ {
		if gap := res.at[i].Sub(res.at[i-1]); gap < interval {
			t.Errorf("gap before request %d (%s) = %v, want >= %v", i, res.queries[i], gap, interval)
		}
	}
	// The retry waits interval + 1×factor×interval.
	if gap, want := res.at[3].Sub(res.at[2]), time.Duration(1+kingdomBackfillBackoffFactor)*interval; gap < want {
		t.Errorf("retry backoff gap = %v, want >= %v", gap, want)
	}
}
