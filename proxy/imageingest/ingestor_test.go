package imageingest

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// --- mocks ---

type mockCommons struct {
	searchRet []Candidate
	searchErr error
	// downloadErrFor returns an error for a specific thumburl (to test
	// fall-through). downloadOK returns these bytes+mime for any other url.
	downloadErrFor map[string]error
	downloadBytes  []byte
	downloadMIME   string
}

func (m *mockCommons) Search(_ context.Context, _ string, _ int) ([]Candidate, error) {
	return m.searchRet, m.searchErr
}

func (m *mockCommons) Download(_ context.Context, rawURL string) ([]byte, string, error) {
	if err, ok := m.downloadErrFor[rawURL]; ok {
		return nil, "", err
	}
	b := m.downloadBytes
	if b == nil {
		b = pngBytes()
	}
	mime := m.downloadMIME
	if mime == "" {
		mime = "image/png"
	}
	return b, mime, nil
}

type mockStore struct {
	mu        sync.Mutex
	existsRet map[string]bool
	existsErr error
	puts      map[string][]byte
	putErr    error
}

func newMockStore() *mockStore {
	return &mockStore{existsRet: map[string]bool{}, puts: map[string][]byte{}}
}

func (m *mockStore) Exists(_ context.Context, key string) (bool, error) {
	if m.existsErr != nil {
		return false, m.existsErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.existsRet[key], nil
}

func (m *mockStore) Put(_ context.Context, key string, body []byte, _, _ string) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts[key] = body
	return nil
}

type mockLedger struct {
	mu        sync.Mutex
	rows      map[string]*LedgerRow
	ingested  []LedgerRow
	lookupErr error
	upsertErr error
}

func newMockLedger() *mockLedger {
	return &mockLedger{rows: map[string]*LedgerRow{}}
}

func (m *mockLedger) Lookup(_ context.Context, slug string) (*LedgerRow, error) {
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[slug]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (m *mockLedger) Upsert(_ context.Context, row LedgerRow) error {
	if m.upsertErr != nil {
		return m.upsertErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := row
	m.rows[row.Slug] = &cp
	return nil
}

func (m *mockLedger) IngestedRows(_ context.Context) ([]LedgerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ingested, nil
}

type mockSeeds struct {
	names []string
	err   error
}

func (m *mockSeeds) Seeds(_ context.Context) ([]string, error) { return m.names, m.err }

// --- candidate fixtures ---

func cand(title, thumb, mime string, w, h int, lic License) Candidate {
	return Candidate{
		Title:    title,
		PageURL:  "https://commons.wikimedia.org/wiki/" + title,
		URL:      "https://upload/" + title,
		ThumbURL: thumb,
		MIME:     mime,
		Width:    w,
		Height:   h,
		License:  lic,
	}
}

func cc0Lic() License {
	return License{Allowed: true, Family: FamilyCC0, Code: "cc0", ShortName: "CC0", AttributionRequired: false}
}
func bySaLic() License {
	return License{Allowed: true, Family: FamilyCCBYSA, Code: "cc-by-sa-4.0", ShortName: "CC BY-SA 4.0", Author: "Jane", AttributionRequired: true}
}
func byLic() License {
	return License{Allowed: true, Family: FamilyCCBY, Code: "cc-by-4.0", ShortName: "CC BY 4.0", Author: "Bob", AttributionRequired: true}
}
func rejectLic() License { return License{Allowed: false, Family: FamilyUnknown} }

// --- IngestOne table tests ---

func TestIngestOne(t *testing.T) {
	cases := []struct {
		name        string
		allowAttrib bool
		setup       func(*mockCommons, *mockStore)
		wantStatus  IngestOutcomeStatus
		wantPut     bool // expect an R2 PutObject of the hero
	}{
		{
			name: "skipped_exists when R2 already has the key",
			setup: func(c *mockCommons, s *mockStore) {
				s.existsRet[heroKey("rosa")] = true
			},
			wantStatus: OutcomeSkippedExists,
			wantPut:    false,
		},
		{
			name: "ingested CC0",
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())}
			},
			wantStatus: OutcomeIngested,
			wantPut:    true,
		},
		{
			name: "no_acceptable_image when search empty",
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = nil
			},
			wantStatus: OutcomeNoAcceptableImg,
			wantPut:    false,
		},
		{
			name: "no_acceptable_image when only rejected licenses",
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = []Candidate{cand("File:x.jpg", "https://thumb/x", "image/jpeg", 100, 100, rejectLic())}
			},
			wantStatus: OutcomeNoAcceptableImg,
			wantPut:    false,
		},
		{
			name: "no_acceptable_image when the only candidate has an empty thumburl",
			setup: func(c *mockCommons, s *mockStore) {
				// Commons couldn't render a 1600px rendition → unusable (we only
				// store the scaled rendition, D2); must be skipped, not soft-failed
				// as source_error via a Download("") attempt.
				c.searchRet = []Candidate{cand("File:nothumb.jpg", "", "image/jpeg", 4000, 3000, cc0Lic())}
			},
			wantStatus: OutcomeNoAcceptableImg,
			wantPut:    false,
		},
		{
			name:        "deferred_attribution when gate OFF and only BY/SA",
			allowAttrib: false,
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = []Candidate{cand("File:bysa.jpg", "https://thumb/bysa", "image/jpeg", 2000, 1500, bySaLic())}
			},
			wantStatus: OutcomeDeferredAttrib,
			wantPut:    false,
		},
		{
			name:        "ingested BY/SA when gate ON",
			allowAttrib: true,
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = []Candidate{cand("File:bysa.jpg", "https://thumb/bysa", "image/jpeg", 2000, 1500, bySaLic())}
			},
			wantStatus: OutcomeIngested,
			wantPut:    true,
		},
		{
			name: "source_error when search fails",
			setup: func(c *mockCommons, s *mockStore) {
				c.searchErr = ErrCommonsUnavailable
			},
			wantStatus: OutcomeSourceError,
			wantPut:    false,
		},
		{
			name: "source_error when HEAD fails",
			setup: func(c *mockCommons, s *mockStore) {
				s.existsErr = errors.New("r2 down")
			},
			wantStatus: OutcomeSourceError,
			wantPut:    false,
		},
		{
			name: "source_error when all downloads fail",
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())}
				c.downloadErrFor = map[string]error{"https://thumb/a": ErrCommonsUnavailable}
			},
			wantStatus: OutcomeSourceError,
			wantPut:    false,
		},
		{
			name: "upload_error when PutObject fails",
			setup: func(c *mockCommons, s *mockStore) {
				c.searchRet = []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())}
				s.putErr = errors.New("put failed")
			},
			wantStatus: OutcomeUploadError,
			wantPut:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			commons := &mockCommons{}
			store := newMockStore()
			if c.setup != nil {
				c.setup(commons, store)
			}
			in := NewIngestor(commons, store, newMockLedger(), &mockSeeds{}, Config{
				AllowAttributionLicenses: c.allowAttrib,
			})
			out, err := in.IngestOne(context.Background(), "rosa", "Rosa regina")
			if err != nil {
				t.Fatalf("IngestOne returned hard error: %v", err)
			}
			if out.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q (note=%q)", out.Status, c.wantStatus, out.Note)
			}
			_, didPut := store.puts[heroKey("rosa")]
			if didPut != c.wantPut {
				t.Errorf("put hero = %v, want %v", didPut, c.wantPut)
			}
		})
	}
}

func TestIngestOne_DownloadFallThrough(t *testing.T) {
	// First (best) candidate's download fails; the second succeeds.
	commons := &mockCommons{
		searchRet: []Candidate{
			cand("File:big.jpg", "https://thumb/big", "image/jpeg", 5000, 4000, cc0Lic()),
			cand("File:small.jpg", "https://thumb/small", "image/jpeg", 2000, 1500, cc0Lic()),
		},
		downloadErrFor: map[string]error{"https://thumb/big": ErrCommonsUnavailable},
	}
	store := newMockStore()
	in := NewIngestor(commons, store, newMockLedger(), &mockSeeds{}, Config{})
	out, err := in.IngestOne(context.Background(), "rosa", "Rosa regina")
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if out.Status != OutcomeIngested {
		t.Fatalf("Status = %q, want ingested", out.Status)
	}
	if _, ok := store.puts[heroKey("rosa")]; !ok {
		t.Errorf("expected hero put after fall-through")
	}
}

func TestIngestOne_DeferredCapturesProvenance(t *testing.T) {
	commons := &mockCommons{
		searchRet: []Candidate{cand("File:bysa.jpg", "https://thumb/bysa", "image/jpeg", 2000, 1500, bySaLic())},
	}
	in := NewIngestor(commons, newMockStore(), newMockLedger(), &mockSeeds{}, Config{AllowAttributionLicenses: false})
	out, _ := in.IngestOne(context.Background(), "rosa", "Rosa regina")
	if out.Status != OutcomeDeferredAttrib {
		t.Fatalf("Status = %q, want deferred_attribution", out.Status)
	}
	if out.License != "cc-by-sa-4.0" || out.Author != "Jane" || out.FilePage == "" {
		t.Errorf("deferred outcome missing provenance: %+v", out)
	}
	if out.ThumbURL != "https://thumb/bysa" {
		t.Errorf("deferred outcome must capture the chosen 1600px thumburl, got %q", out.ThumbURL)
	}
}

// TestRecordOutcome_SkippedExistsPreservesPriorAttribution locks the fix for the
// code-review finding: re-running an already-live slug (HEAD hit →
// skipped_exists, which carries NO license/author) must NOT wipe the prior
// attribution row, or the next credits.json rebuild would drop a live CC-BY/SA
// image's required credit (a CC §3(a) violation).
func TestRecordOutcome_SkippedExistsPreservesPriorAttribution(t *testing.T) {
	ledger := newMockLedger()
	prior := &LedgerRow{
		Slug: "rosa", ScientificName: "Rosa regina", Status: StatusIngested,
		R2Key: heroKey("rosa"), LicenseCode: "cc-by-sa-4.0", LicenseShort: "CC BY-SA 4.0",
		AttributionAuthor: "Jane", AttributionRequired: true,
		SourceFilePage: "https://commons.wikimedia.org/wiki/File:rosa.jpg",
	}
	ledger.rows["rosa"] = prior
	in := NewIngestor(&mockCommons{}, newMockStore(), ledger, &mockSeeds{}, Config{})

	// skipped_exists carries no license/author (we never searched).
	out := IngestOutcome{Status: OutcomeSkippedExists, Slug: "rosa", ScientificName: "Rosa regina", R2Key: heroKey("rosa")}
	in.recordOutcome(context.Background(), "rosa", "Rosa regina", out, prior)

	got := ledger.rows["rosa"]
	if got.AttributionAuthor != "Jane" || got.LicenseCode != "cc-by-sa-4.0" || !got.AttributionRequired {
		t.Errorf("skipped_exists wiped prior attribution: %+v", got)
	}
}

// TestShouldSkip_FailedAlwaysRetries locks the fix: a `failed` row (transient /
// infra error) must always retry — never a permanent negative cache, so an
// R2 / Wikimedia outage cannot park a healthy species forever.
func TestShouldSkip_FailedAlwaysRetries(t *testing.T) {
	in := NewIngestor(&mockCommons{}, newMockStore(), newMockLedger(), &mockSeeds{}, Config{})
	if in.shouldSkip(&LedgerRow{Status: StatusFailed, Attempts: 99}) {
		t.Errorf("failed row with high attempts must still retry (no permanent negative cache)")
	}
	if !in.shouldSkip(&LedgerRow{Status: StatusNoAcceptableImg}) {
		t.Errorf("no_acceptable_image should stay cached/skipped")
	}
	if !in.shouldSkip(&LedgerRow{Status: StatusIngested}) {
		t.Errorf("ingested should be skipped")
	}
}

// --- RunBatch tests ---

func TestRunBatch_MixedOutcomes(t *testing.T) {
	commons := &mockCommons{
		searchRet: []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())},
	}
	store := newMockStore()
	ledger := newMockLedger()
	// "×" is a non-empty name that slugs to "" (the hybrid sign is a separator);
	// it is skipped (no valid R2 key) and NOT counted as attempted.
	seeds := &mockSeeds{names: []string{"Rosa regina", "Tulipa gesneriana", "×"}}
	in := NewIngestor(commons, store, ledger, seeds, Config{})

	summary, err := in.RunBatch(context.Background(), 0)
	if err != nil {
		t.Fatalf("RunBatch err: %v", err)
	}
	if summary.Seen != 3 {
		t.Errorf("Seen = %d, want 3", summary.Seen)
	}
	// Two real names ingest; the slugs-to-empty name is skipped (not counted
	// as attempted, not written to the ledger).
	if summary.Ingested != 2 {
		t.Errorf("Ingested = %d, want 2", summary.Ingested)
	}
	if summary.Attempted != 2 {
		t.Errorf("Attempted = %d, want 2", summary.Attempted)
	}
	// Ledger should hold rows for both real slugs.
	if _, ok := ledger.rows["rosa-regina"]; !ok {
		t.Errorf("expected ledger row for rosa-regina")
	}
	if _, ok := ledger.rows["tulipa-gesneriana"]; !ok {
		t.Errorf("expected ledger row for tulipa-gesneriana")
	}
	// The slugs-to-empty name ("×") is skipped (no valid R2 key) and NOT written
	// to the ledger: a row keyed by the raw name is never read (lookups key on
	// the empty slug — Codex #23), so it must not be created.
	if _, ok := ledger.rows["×"]; ok {
		t.Errorf("did not expect a ledger row for the slugs-to-empty name '×'")
	}
	if _, ok := ledger.rows[""]; ok {
		t.Errorf("did not expect a ledger row under the empty slug key")
	}
}

func TestRunBatch_LimitCaps(t *testing.T) {
	commons := &mockCommons{
		searchRet: []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())},
	}
	seeds := &mockSeeds{names: []string{"A plant", "B plant", "C plant", "D plant"}}
	in := NewIngestor(commons, newMockStore(), newMockLedger(), seeds, Config{})

	summary, err := in.RunBatch(context.Background(), 2)
	if err != nil {
		t.Fatalf("RunBatch err: %v", err)
	}
	if summary.Attempted != 2 {
		t.Errorf("Attempted = %d, want 2 (limit)", summary.Attempted)
	}
}

func TestRunBatch_SkipsAlreadyIngested(t *testing.T) {
	commons := &mockCommons{
		searchRet: []Candidate{cand("File:a.jpg", "https://thumb/a", "image/jpeg", 4000, 3000, cc0Lic())},
	}
	ledger := newMockLedger()
	ledger.rows["rosa-regina"] = &LedgerRow{Slug: "rosa-regina", Status: StatusIngested}
	seeds := &mockSeeds{names: []string{"Rosa regina"}}
	in := NewIngestor(commons, newMockStore(), ledger, seeds, Config{})

	summary, err := in.RunBatch(context.Background(), 0)
	if err != nil {
		t.Fatalf("RunBatch err: %v", err)
	}
	if summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", summary.Skipped)
	}
	if summary.Attempted != 0 {
		t.Errorf("Attempted = %d, want 0", summary.Attempted)
	}
}

func TestRunBatch_DeferredReprocessedWhenGateOn(t *testing.T) {
	commons := &mockCommons{
		searchRet: []Candidate{cand("File:bysa.jpg", "https://thumb/bysa", "image/jpeg", 2000, 1500, bySaLic())},
	}
	ledger := newMockLedger()
	ledger.rows["rosa-regina"] = &LedgerRow{Slug: "rosa-regina", Status: StatusDeferredAttrib}
	seeds := &mockSeeds{names: []string{"Rosa regina"}}

	// Gate OFF → the deferred row is skipped.
	off := NewIngestor(commons, newMockStore(), ledger, seeds, Config{AllowAttributionLicenses: false})
	sOff, _ := off.RunBatch(context.Background(), 0)
	if sOff.Skipped != 1 || sOff.Attempted != 0 {
		t.Errorf("gate OFF: skipped=%d attempted=%d, want 1/0", sOff.Skipped, sOff.Attempted)
	}

	// Gate ON → the deferred row is re-processed (uploads now).
	ledger2 := newMockLedger()
	ledger2.rows["rosa-regina"] = &LedgerRow{Slug: "rosa-regina", Status: StatusDeferredAttrib}
	on := NewIngestor(commons, newMockStore(), ledger2, seeds, Config{AllowAttributionLicenses: true})
	sOn, _ := on.RunBatch(context.Background(), 0)
	if sOn.Attempted != 1 || sOn.Ingested != 1 {
		t.Errorf("gate ON: attempted=%d ingested=%d, want 1/1", sOn.Attempted, sOn.Ingested)
	}
}

func TestRunBatch_RebuildsCredits(t *testing.T) {
	commons := &mockCommons{searchRet: nil}
	store := newMockStore()
	ledger := newMockLedger()
	ledger.ingested = []LedgerRow{{Slug: "rosa", ScientificName: "Rosa regina", LicenseShort: "CC0"}}
	seeds := &mockSeeds{names: []string{}}
	in := NewIngestor(commons, store, ledger, seeds, Config{})

	_, err := in.RunBatch(context.Background(), 0)
	if err != nil {
		t.Fatalf("RunBatch err: %v", err)
	}
	if _, ok := store.puts[creditsKey]; !ok {
		t.Errorf("expected credits.json put after batch")
	}
}

func TestRunBatch_SingleFlight(t *testing.T) {
	// Verify a concurrent second call returns an empty summary while one is
	// running. We can't easily force overlap deterministically, so we assert
	// the guard field directly via the running flag with a manual lock.
	in := NewIngestor(&mockCommons{}, newMockStore(), newMockLedger(), &mockSeeds{}, Config{})
	in.runMu.Lock()
	in.running = true
	in.runMu.Unlock()
	summary, err := in.RunBatch(context.Background(), 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if summary.Seen != 0 || summary.Attempted != 0 {
		t.Errorf("expected empty summary while running, got %+v", summary)
	}
}

// --- ranking tests ---

func TestRankCandidates_LicenseTierThenSize(t *testing.T) {
	cands := []Candidate{
		cand("File:bysa.jpg", "t1", "image/jpeg", 5000, 4000, bySaLic()),
		cand("File:cc0small.jpg", "t2", "image/jpeg", 1000, 800, cc0Lic()),
		cand("File:cc0big.jpg", "t3", "image/jpeg", 4000, 3000, cc0Lic()),
		cand("File:by.jpg", "t4", "image/jpeg", 6000, 5000, byLic()),
	}
	ranked := rankCandidates(cands)
	// CC0 first (tier 0), larger area within tier wins → cc0big before cc0small.
	if ranked[0].Title != "File:cc0big.jpg" {
		t.Errorf("ranked[0] = %q, want cc0big", ranked[0].Title)
	}
	if ranked[1].Title != "File:cc0small.jpg" {
		t.Errorf("ranked[1] = %q, want cc0small", ranked[1].Title)
	}
	if ranked[2].Title != "File:by.jpg" {
		t.Errorf("ranked[2] = %q, want by", ranked[2].Title)
	}
	if ranked[3].Title != "File:bysa.jpg" {
		t.Errorf("ranked[3] = %q, want bysa", ranked[3].Title)
	}
}

func TestRankCandidates_ExcludesFormatsAndDeprioritizes(t *testing.T) {
	cands := []Candidate{
		cand("File:svgmap.svg", "t1", "image/svg+xml", 9000, 9000, cc0Lic()),
		cand("File:distribution map.jpg", "t2", "image/jpeg", 8000, 8000, cc0Lic()),
		cand("File:photo.jpg", "t3", "image/jpeg", 2000, 1500, cc0Lic()),
	}
	ranked := rankCandidates(cands)
	if len(ranked) != 2 {
		t.Fatalf("expected svg excluded → 2 candidates, got %d", len(ranked))
	}
	// The plain photo (not a "map") ranks above the distribution map even though
	// the map has a larger pixel area.
	if ranked[0].Title != "File:photo.jpg" {
		t.Errorf("ranked[0] = %q, want photo (map deprioritized)", ranked[0].Title)
	}
}

func TestCcDeedURL(t *testing.T) {
	cases := []struct{ code, want string }{
		{"cc0", "https://creativecommons.org/publicdomain/zero/1.0/"},
		{"cc-by-4.0", "https://creativecommons.org/licenses/by/4.0/"},
		{"cc-by-sa-4.0", "https://creativecommons.org/licenses/by-sa/4.0/"},
		{"cc-by-sa-3.0", "https://creativecommons.org/licenses/by-sa/3.0/"},
		{"pd", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ccDeedURL(c.code); got != c.want {
			t.Errorf("ccDeedURL(%q) = %q, want %q", c.code, got, c.want)
		}
	}
}
