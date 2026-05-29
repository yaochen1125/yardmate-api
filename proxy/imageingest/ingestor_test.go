package imageingest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy/imageingest/sources"
)

// --- mocks ---

// mockSource implements ImageSource. downloadErrFor lets a specific DownloadURL
// fail (download fall-through tests); any other url returns downloadBytes/MIME.
type mockSource struct {
	searchRet      []sources.Candidate
	searchErr      error
	downloadErrFor map[string]error
	downloadBytes  []byte
	downloadMIME   string
}

func (m *mockSource) Search(_ context.Context, _ string, _ int) ([]sources.Candidate, error) {
	return m.searchRet, m.searchErr
}

func (m *mockSource) Download(_ context.Context, rawURL string) ([]byte, string, error) {
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

// mockLedger implements LedgerStore over the two v2 tables in memory.
type mockLedger struct {
	mu               sync.Mutex
	species          map[string]*SpeciesRow
	files            map[string]*FileRow // key: fileKey(slug, index)
	ingested         []FileRow           // returned by IngestedFiles
	upsertSpeciesErr error
	lookupErr        error
}

func newMockLedger() *mockLedger {
	return &mockLedger{species: map[string]*SpeciesRow{}, files: map[string]*FileRow{}}
}

func fileKey(slug string, i int) string { return fmt.Sprintf("%s#%d", slug, i) }

func (m *mockLedger) UpsertSpecies(_ context.Context, row SpeciesRow) error {
	if m.upsertSpeciesErr != nil {
		return m.upsertSpeciesErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := row
	m.species[row.Slug] = &cp
	return nil
}

func (m *mockLedger) UpsertFile(_ context.Context, row FileRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := row
	m.files[fileKey(row.Slug, row.ImageIndex)] = &cp
	return nil
}

func (m *mockLedger) LookupFile(_ context.Context, slug string, i int) (*FileRow, error) {
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.files[fileKey(slug, i)]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// RecomputeCount mirrors the real UPDATE ... RETURNING: it errors when the
// species row is missing (so a test can catch a recompute-before-upsert bug).
func (m *mockLedger) RecomputeCount(_ context.Context, slug string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.species[slug]
	if !ok {
		return 0, fmt.Errorf("%w: no species row %q", ErrLedgerUnavailable, slug)
	}
	count := 0
	for _, f := range m.files {
		if f.Slug == slug && f.Status == StatusIngested {
			count++
		}
	}
	sp.ImageCountFilled = count
	return count, nil
}

func (m *mockLedger) IngestedFiles(_ context.Context) ([]FileRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ingested, nil
}

// --- candidate fixtures ---

func cand(source, title, dlURL, mime, code string, w, h int) sources.Candidate {
	return sources.Candidate{
		Source:      source,
		Title:       title,
		PageURL:     "https://example.org/" + title,
		DownloadURL: dlURL,
		DedupKey:    title, // unique per fixture
		MIME:        mime,
		Width:       w,
		Height:      h,
		LicenseCode: code,
		Author:      "Jane",
	}
}

func newIngestor(src *mockSource, store *mockStore, ledger *mockLedger, cfg Config) *Ingestor {
	return NewIngestor(src, nil, store, ledger, cfg)
}

// pngBytes returns a tiny valid PNG header so http.DetectContentType (in the
// real source clients) and the mocks agree the bytes are image/png.
func pngBytes() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0, 0, 0}
}

// --- IngestSpecies single-slot table tests ---

func TestIngestSpecies_PerImageStatus(t *testing.T) {
	cases := []struct {
		name        string
		allowAttrib bool
		setup       func(*mockSource, *mockStore)
		wantStatus  ImageStatus
		wantPut     bool
	}{
		{
			name: "skipped_exists when R2 already has the key",
			setup: func(c *mockSource, s *mockStore) {
				s.existsRet[galleryKey("rosa-regina", 1)] = true
			},
			wantStatus: ImgSkippedExists, wantPut: false,
		},
		{
			name: "ingested CC0",
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = []sources.Candidate{cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 4000, 3000)}
			},
			wantStatus: ImgIngested, wantPut: true,
		},
		{
			name: "no_acceptable when search empty",
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = nil
			},
			wantStatus: ImgNoAcceptable, wantPut: false,
		},
		{
			name: "no_acceptable when only rejected (ARR) licenses",
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = []sources.Candidate{cand(sources.SourceINaturalist, "x", "https://dl/x", "image/jpeg", "", 100, 100)}
			},
			wantStatus: ImgNoAcceptable, wantPut: false,
		},
		{
			name:        "deferred_attribution when gate OFF and only BY/SA",
			allowAttrib: false,
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = []sources.Candidate{cand(sources.SourceWikimediaCommons, "bysa", "https://dl/bysa", "image/jpeg", "cc-by-sa-4.0", 2000, 1500)}
			},
			wantStatus: ImgDeferredAttrib, wantPut: false,
		},
		{
			name:        "ingested BY/SA when gate ON",
			allowAttrib: true,
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = []sources.Candidate{cand(sources.SourceWikimediaCommons, "bysa", "https://dl/bysa", "image/jpeg", "cc-by-sa-4.0", 2000, 1500)}
			},
			wantStatus: ImgIngested, wantPut: true,
		},
		{
			name: "source_error when HEAD fails",
			setup: func(c *mockSource, s *mockStore) {
				s.existsErr = errors.New("r2 down")
			},
			wantStatus: ImgSourceError, wantPut: false,
		},
		{
			name: "source_error when all downloads fail",
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = []sources.Candidate{cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 4000, 3000)}
				c.downloadErrFor = map[string]error{"https://dl/a": sources.ErrUnavailable}
			},
			wantStatus: ImgSourceError, wantPut: false,
		},
		{
			name: "upload_error when PutObject fails",
			setup: func(c *mockSource, s *mockStore) {
				c.searchRet = []sources.Candidate{cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 4000, 3000)}
				s.putErr = errors.New("put failed")
			},
			wantStatus: ImgUploadError, wantPut: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &mockSource{}
			store := newMockStore()
			if c.setup != nil {
				c.setup(src, store)
			}
			in := newIngestor(src, store, newMockLedger(), Config{AllowAttributionLicenses: c.allowAttrib})
			out, err := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina", ImageCount: 1})
			if err != nil {
				t.Fatalf("IngestSpecies hard error: %v", err)
			}
			if out.Slug != "rosa-regina" {
				t.Errorf("slug = %q, want rosa-regina", out.Slug)
			}
			if len(out.PerImage) != 1 {
				t.Fatalf("PerImage len = %d, want 1", len(out.PerImage))
			}
			if got := out.PerImage[0].Status; got != c.wantStatus {
				t.Errorf("Status = %q, want %q (note=%q)", got, c.wantStatus, out.PerImage[0].Note)
			}
			_, didPut := store.puts[galleryKey("rosa-regina", 1)]
			if didPut != c.wantPut {
				t.Errorf("put = %v, want %v", didPut, c.wantPut)
			}
		})
	}
}

func TestIngestSpecies_MultiImageDistinctSlots(t *testing.T) {
	src := &mockSource{searchRet: []sources.Candidate{
		cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 4000, 3000),
		cand(sources.SourceINaturalist, "b", "https://dl/b", "image/jpeg", "cc0", 3000, 2000),
	}}
	store := newMockStore()
	ledger := newMockLedger()
	in := newIngestor(src, store, ledger, Config{})
	out, err := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina", ImageCount: 2})
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(out.PerImage) != 2 || out.PerImage[0].Status != ImgIngested || out.PerImage[1].Status != ImgIngested {
		t.Fatalf("want 2 ingested, got %+v", out.PerImage)
	}
	if _, ok := store.puts[galleryKey("rosa-regina", 1)]; !ok {
		t.Errorf("missing slot 1 put")
	}
	if _, ok := store.puts[galleryKey("rosa-regina", 2)]; !ok {
		t.Errorf("missing slot 2 put")
	}
	// Within-gallery dedup: the two slots used distinct candidates.
	if out.PerImage[0].SourceURL == out.PerImage[1].SourceURL {
		t.Errorf("slots reused the same candidate: %q", out.PerImage[0].SourceURL)
	}
	// Species aggregate recomputed AFTER files (review item a).
	if sp := ledger.species["rosa-regina"]; sp == nil || sp.ImageCountFilled != 2 {
		t.Errorf("species image_count_filled = %+v, want 2", sp)
	}
}

func TestIngestSpecies_DownloadFallThrough(t *testing.T) {
	// Best candidate's download 404s; the next succeeds for the same slot.
	src := &mockSource{
		searchRet: []sources.Candidate{
			cand(sources.SourceINaturalist, "big", "https://dl/big", "image/jpeg", "cc0", 5000, 4000),
			cand(sources.SourceINaturalist, "small", "https://dl/small", "image/jpeg", "cc0", 2000, 1500),
		},
		downloadErrFor: map[string]error{"https://dl/big": sources.ErrUnavailable},
	}
	store := newMockStore()
	in := newIngestor(src, store, newMockLedger(), Config{})
	out, err := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina", ImageCount: 1})
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if out.PerImage[0].Status != ImgIngested {
		t.Fatalf("Status = %q, want ingested", out.PerImage[0].Status)
	}
	if _, ok := store.puts[galleryKey("rosa-regina", 1)]; !ok {
		t.Errorf("expected put after fall-through")
	}
}

// TestIngestSpecies_DownloadFailureNotPermanentNegativeCache locks the review
// fix: when the eligible pool is depleted by transient download failures, slots
// that get no candidate are recorded as failed (retryable), NOT
// no_acceptable_image (terminal negative cache that an outage could park
// forever — the §3 / §9 #13 anti-pattern).
func TestIngestSpecies_DownloadFailureNotPermanentNegativeCache(t *testing.T) {
	src := &mockSource{
		searchRet: []sources.Candidate{
			cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 4000, 3000),
		},
		downloadErrFor: map[string]error{"https://dl/a": sources.ErrUnavailable},
	}
	ledger := newMockLedger()
	in := newIngestor(src, newMockStore(), ledger, Config{})
	out, _ := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina", ImageCount: 2})
	if len(out.PerImage) != 2 {
		t.Fatalf("PerImage = %d, want 2", len(out.PerImage))
	}
	for i, oc := range out.PerImage {
		if oc.Status != ImgSourceError {
			t.Errorf("slot %d Status = %q, want source_error", i+1, oc.Status)
		}
	}
	// Both file rows must persist as 'failed' (retryable), never no_acceptable.
	for i := 1; i <= 2; i++ {
		if r := ledger.files[fileKey("rosa-regina", i)]; r == nil || r.Status != StatusFailed {
			t.Errorf("slot %d ledger = %+v, want StatusFailed", i, r)
		}
	}
}

func TestIngestSpecies_DeferredCapturesProvenance(t *testing.T) {
	src := &mockSource{searchRet: []sources.Candidate{
		cand(sources.SourceWikimediaCommons, "bysa", "https://dl/bysa", "image/jpeg", "cc-by-sa-4.0", 2000, 1500),
	}}
	ledger := newMockLedger()
	in := newIngestor(src, newMockStore(), ledger, Config{AllowAttributionLicenses: false})
	out, _ := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina", ImageCount: 1})
	if out.PerImage[0].Status != ImgDeferredAttrib {
		t.Fatalf("Status = %q, want deferred_attribution", out.PerImage[0].Status)
	}
	row := ledger.files[fileKey("rosa-regina", 1)]
	if row == nil || row.Status != StatusDeferredAttrib {
		t.Fatalf("deferred row missing: %+v", row)
	}
	if row.LicenseCode != "cc-by-sa-4.0" || row.AttributionAuthor != "Jane" || !row.AttributionRequired {
		t.Errorf("deferred row missing attribution: %+v", row)
	}
	if row.PendingURL != "https://dl/bysa" {
		t.Errorf("deferred row must store pending_url, got %q", row.PendingURL)
	}
}

// TestIngestSpecies_PriorIngestedPreservesAttribution: a slot already ingested
// (with required CC-BY/SA credit) is skipped on the next trigger WITHOUT a
// ledger rewrite — so the credits.json rebuild keeps the live image's credit.
func TestIngestSpecies_PriorIngestedPreservesAttribution(t *testing.T) {
	ledger := newMockLedger()
	ledger.species["rosa-regina"] = &SpeciesRow{Slug: "rosa-regina"}
	prior := &FileRow{
		Slug: "rosa-regina", ImageIndex: 1, Status: StatusIngested,
		R2Key: galleryKey("rosa-regina", 1), LicenseCode: "cc-by-sa-4.0",
		AttributionAuthor: "Jane", AttributionRequired: true,
	}
	ledger.files[fileKey("rosa-regina", 1)] = prior
	src := &mockSource{searchRet: []sources.Candidate{
		cand(sources.SourceINaturalist, "a", "https://dl/a", "image/jpeg", "cc0", 100, 100),
	}}
	in := newIngestor(src, newMockStore(), ledger, Config{})
	out, _ := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina", ImageCount: 1})
	if out.PerImage[0].Status != ImgSkippedExists {
		t.Fatalf("Status = %q, want skipped_exists", out.PerImage[0].Status)
	}
	got := ledger.files[fileKey("rosa-regina", 1)]
	if got.AttributionAuthor != "Jane" || got.LicenseCode != "cc-by-sa-4.0" || !got.AttributionRequired {
		t.Errorf("prior attribution wiped: %+v", got)
	}
}

func TestIngestSpecies_Coalesced(t *testing.T) {
	in := newIngestor(&mockSource{}, newMockStore(), newMockLedger(), Config{})
	if !in.acquire("rosa-regina") {
		t.Fatal("first acquire should succeed")
	}
	out, err := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "Rosa regina"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !out.Coalesced || len(out.PerImage) != 0 {
		t.Errorf("expected coalesced empty outcome, got %+v", out)
	}
}

func TestIngestSpecies_EmptySlugNoOp(t *testing.T) {
	in := newIngestor(&mockSource{}, newMockStore(), newMockLedger(), Config{})
	out, err := in.IngestSpecies(context.Background(), IngestRequest{ScientificName: "×"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Slug != "" || len(out.PerImage) != 0 {
		t.Errorf("expected empty no-op outcome, got %+v", out)
	}
}

func TestShouldSkip(t *testing.T) {
	off := newIngestor(&mockSource{}, newMockStore(), newMockLedger(), Config{AllowAttributionLicenses: false})
	on := newIngestor(&mockSource{}, newMockStore(), newMockLedger(), Config{AllowAttributionLicenses: true})

	if !off.shouldSkip(&FileRow{Status: StatusIngested}) {
		t.Error("ingested should skip")
	}
	if !off.shouldSkip(&FileRow{Status: StatusNoAcceptableImg}) {
		t.Error("no_acceptable_image should skip")
	}
	if off.shouldSkip(&FileRow{Status: StatusFailed, Attempts: 99}) {
		t.Error("failed must always retry (no permanent negative cache)")
	}
	if !off.shouldSkip(&FileRow{Status: StatusDeferredAttrib}) {
		t.Error("deferred should skip while gate OFF")
	}
	if on.shouldSkip(&FileRow{Status: StatusDeferredAttrib}) {
		t.Error("deferred should re-process while gate ON")
	}
}

// --- ranking tests ---

func scored(source, title, mime, code string, w, h int) scoredCandidate {
	c := cand(source, title, "https://dl/"+title, mime, code, w, h)
	return scoredCandidate{cand: c, lic: classifyCandidate(c)}
}

func TestRankScored_LicenseThenSourceThenSize(t *testing.T) {
	cands := []scoredCandidate{
		scored(sources.SourceWikimediaCommons, "bysa", "image/jpeg", "cc-by-sa-4.0", 5000, 4000),
		scored(sources.SourceWikimediaCommons, "cc0small", "image/jpeg", "cc0", 1000, 800),
		scored(sources.SourceINaturalist, "cc0inat", "image/jpeg", "cc0", 1200, 900),
		scored(sources.SourceWikimediaCommons, "cc0big", "image/jpeg", "cc0", 4000, 3000),
		scored(sources.SourceWikimediaCommons, "by", "image/jpeg", "cc-by-4.0", 6000, 5000),
	}
	rankScored(cands)
	// Tier 0 (CC0) first; within tier iNat beats Wikimedia regardless of size.
	if cands[0].cand.Title != "cc0inat" {
		t.Errorf("ranked[0] = %q, want cc0inat (iNat source tier)", cands[0].cand.Title)
	}
	// Then CC0 Wikimedia by larger area.
	if cands[1].cand.Title != "cc0big" || cands[2].cand.Title != "cc0small" {
		t.Errorf("ranked[1..2] = %q,%q, want cc0big,cc0small", cands[1].cand.Title, cands[2].cand.Title)
	}
	if cands[3].cand.Title != "by" || cands[4].cand.Title != "bysa" {
		t.Errorf("ranked[3..4] = %q,%q, want by,bysa", cands[3].cand.Title, cands[4].cand.Title)
	}
}

func TestGatherCandidates_ExcludesFormatsAndDeprioritizes(t *testing.T) {
	src := &mockSource{searchRet: []sources.Candidate{
		cand(sources.SourceWikimediaCommons, "svgmap.svg", "https://dl/svg", "image/svg+xml", "cc0", 9000, 9000),
		cand(sources.SourceWikimediaCommons, "distribution map.jpg", "https://dl/map", "image/jpeg", "cc0", 8000, 8000),
		cand(sources.SourceWikimediaCommons, "photo.jpg", "https://dl/photo", "image/jpeg", "cc0", 2000, 1500),
	}}
	in := newIngestor(src, newMockStore(), newMockLedger(), Config{})
	eligible, _ := in.gatherCandidates(context.Background(), "Rosa regina", 4)
	if len(eligible) != 2 {
		t.Fatalf("want svg excluded → 2 eligible, got %d", len(eligible))
	}
	if eligible[0].cand.Title != "photo.jpg" {
		t.Errorf("ranked[0] = %q, want photo.jpg (map deprioritized)", eligible[0].cand.Title)
	}
}

func TestCcDeedURL(t *testing.T) {
	cases := []struct{ code, want string }{
		{"cc0", "https://creativecommons.org/publicdomain/zero/1.0/"},
		{"cc-by-4.0", "https://creativecommons.org/licenses/by/4.0/"},
		{"cc-by-sa-4.0", "https://creativecommons.org/licenses/by-sa/4.0/"},
		{"cc-by-sa-3.0", "https://creativecommons.org/licenses/by-sa/3.0/"},
		{"cc-by", "https://creativecommons.org/licenses/by/4.0/"},
		{"pd", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ccDeedURL(c.code); got != c.want {
			t.Errorf("ccDeedURL(%q) = %q, want %q", c.code, got, c.want)
		}
	}
}
