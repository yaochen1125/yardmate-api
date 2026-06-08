package imageingest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yaochen1125/yardmate-api/proxy/imageingest/sources"
)

func TestCatalogKeyShape(t *testing.T) {
	if got := catalogKey("AAA0001", 3); got != "plant_images/AAA0001/external/3.png" {
		t.Errorf("catalogKey = %q", got)
	}
	if got := catalogIndexKey("AAA0001"); got != "plant_images/AAA0001/external/index.json" {
		t.Errorf("catalogIndexKey = %q", got)
	}
}

func TestIsCatalogID(t *testing.T) {
	ok := []string{"AAA0001", "ZZZ9999", "ABC1234"}
	for _, s := range ok {
		if !isCatalogID(s) {
			t.Errorf("isCatalogID(%q) = false, want true", s)
		}
	}
	// Reject lowercase, wrong digit count, embedded separators / traversal — the
	// charset is a security boundary because the id is interpolated into the R2 key.
	bad := []string{"", "aaa0001", "AAA001", "AAA00001", "AA0001", "AAA0001/x", "AAA-001", "../etc", "AAA0001 "}
	for _, s := range bad {
		if isCatalogID(s) {
			t.Errorf("isCatalogID(%q) = true, want false", s)
		}
	}
}

func TestBuildExternalIndex(t *testing.T) {
	imgs := []ImageOutcome{
		{Index: 1, Status: ImgIngested, Source: "inaturalist", LicenseShort: "CC BY 4.0", LicenseURL: "https://creativecommons.org/licenses/by/4.0/", Author: "Jane", SourceURL: "https://inat/photos/1"},
		{Index: 2, Status: ImgIngested, Source: "wikimedia_commons", LicenseShort: "CC0", Author: "Bob", SourceURL: "https://commons/File:x"},
	}
	body, err := BuildExternalIndex(imgs)
	if err != nil {
		t.Fatal(err)
	}
	var idx ExternalIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Count != 2 || len(idx.Images) != 2 {
		t.Fatalf("count=%d images=%d", idx.Count, len(idx.Images))
	}
	if idx.Images[0].Index != 1 || idx.Images[0].Source != "inaturalist" || idx.Images[0].Author != "Jane" {
		t.Errorf("img0 = %+v", idx.Images[0])
	}
	if idx.Images[1].LicenseShort != "CC0" {
		t.Errorf("img1 license = %q", idx.Images[1].LicenseShort)
	}
}

// catalogTestIngestor wires the cascade with a mock source + store and a NIL
// ledger — proving the in-catalog path never touches the database (it would
// panic on any ledger call if it did). CatalogNames injects a 1-entry
// authoritative map so IngestCatalogSpecies can resolve AAA0001's server-side
// scientific name (SPEC §2.8); the test source ignores the query, so the value
// matters only where a test asserts the searched name (DerivesAuthoritativeName).
func catalogTestIngestor(src *mockSource, store *mockStore) *Ingestor {
	return NewIngestor(src, nil, store, nil, Config{
		AllowAttributionLicenses: true,
		DefaultImageCount:        4,
		CatalogNames:             map[string]string{"AAA0001": "Abelia chinensis"},
	})
}

func cc0Cands(n int) []sources.Candidate {
	names := []string{"a", "b", "c", "d", "e", "f"}
	out := make([]sources.Candidate, 0, n)
	for i := 0; i < n && i < len(names); i++ {
		out = append(out, cand("inaturalist", names[i], "https://dl/"+names[i], "image/jpeg", "cc0", 1200-i*50, 900-i*40))
	}
	return out
}

func TestIngestCatalogSpecies_HappyPath(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if out.AlreadyDone || out.Coalesced {
		t.Fatalf("unexpected skip: %+v", out)
	}
	if len(out.PerImage) != 4 {
		t.Fatalf("ingested %d, want 4", len(out.PerImage))
	}
	for i := 1; i <= 4; i++ {
		if _, ok := store.puts[catalogKey("AAA0001", i)]; !ok {
			t.Errorf("missing upload for slot %d", i)
		}
	}
	body, ok := store.puts[catalogIndexKey("AAA0001")]
	if !ok {
		t.Fatal("index.json not written")
	}
	var idx ExternalIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Count != 4 || len(idx.Images) != 4 {
		t.Errorf("index count=%d images=%d, want 4", idx.Count, len(idx.Images))
	}
}

func TestIngestCatalogSpecies_Idempotent(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	store.existsRet[catalogIndexKey("AAA0001")] = true // already done
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !out.AlreadyDone {
		t.Errorf("want AlreadyDone, got %+v", out)
	}
	if len(store.puts) != 0 {
		t.Errorf("should not upload when index.json exists, got %d puts", len(store.puts))
	}
}

func TestIngestCatalogSpecies_NoCandidates(t *testing.T) {
	src := &mockSource{searchRet: nil}
	store := newMockStore()
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 0 {
		t.Errorf("no candidates should ingest 0 images, got %d", len(out.PerImage))
	}
	if _, ok := store.puts[catalogIndexKey("AAA0001")]; ok {
		t.Error("index.json must NOT be written when nothing was ingested (so a retrigger retries)")
	}
}

func TestIngestCatalogSpecies_PartialFill(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(3)} // only 3 candidates for a 4-image request
	store := newMockStore()
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 3 {
		t.Errorf("ingested %d, want 3 (got-what-we-could)", len(out.PerImage))
	}
	// contiguous keys 1..3, no 4th
	for i := 1; i <= 3; i++ {
		if _, ok := store.puts[catalogKey("AAA0001", i)]; !ok {
			t.Errorf("missing contiguous slot %d", i)
		}
	}
	if _, ok := store.puts[catalogKey("AAA0001", 4)]; ok {
		t.Error("should not have a 4th image")
	}
	body, ok := store.puts[catalogIndexKey("AAA0001")]
	if !ok {
		t.Fatal("index.json should be written with the 3 that succeeded")
	}
	var idx ExternalIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Count != 3 {
		t.Errorf("index count = %d, want 3", idx.Count)
	}
}

// TestIngestCatalogSpecies_InteriorDownloadFail asserts two guarantees when a
// candidate in the MIDDLE of the list 404s: (P1) stored keys are CONTIGUOUS
// (1,2,3 — no hole, never 1,_,3), and (P2) because a retryable failure occurred
// and we fell short of n, the manifest is NOT committed — so a later trigger
// retries instead of locking in the short gallery via AlreadyDone.
func TestIngestCatalogSpecies_InteriorDownloadFail(t *testing.T) {
	src := &mockSource{
		searchRet:      cc0Cands(4),
		downloadErrFor: map[string]error{"https://dl/b": errors.New("404")},
	}
	store := newMockStore()
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 3 {
		t.Fatalf("ingested %d, want 3 (one of four 404'd)", len(out.PerImage))
	}
	// (P1) contiguous keys 1..3, no hole, no 4th
	for i := 1; i <= 3; i++ {
		if _, ok := store.puts[catalogKey("AAA0001", i)]; !ok {
			t.Errorf("missing contiguous slot %d", i)
		}
	}
	if _, ok := store.puts[catalogKey("AAA0001", 4)]; ok {
		t.Error("4th image should not exist")
	}
	// (P2) retryable shortfall → no commit marker, so it retries next time
	if _, ok := store.puts[catalogIndexKey("AAA0001")]; ok {
		t.Error("index.json must NOT be committed after a retryable failure (so it retries)")
	}
}

func TestIngestCatalogSpecies_RejectBadID(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "bad/id", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 0 {
		t.Errorf("bad catalog id should return empty outcome, got %+v", out)
	}
	if len(store.puts) != 0 {
		t.Error("bad catalog id must never write to R2 (path-injection guard)")
	}
}

// TestIngestCatalogSpecies_IndexHeadError asserts a transient HEAD failure on
// index.json is a RETRYABLE skip (no ingest, nothing written) — not a re-ingest
// that burns a full cascade on a possibly-already-done species (Codex P2).
func TestIngestCatalogSpecies_IndexHeadError(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	store.existsErr = errors.New("head boom")
	in := catalogTestIngestor(src, store)

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 0 {
		t.Errorf("HEAD error should skip ingest, got %d images", len(out.PerImage))
	}
	if len(store.puts) != 0 {
		t.Error("HEAD error must not write to R2 (retryable skip)")
	}
}

// TestIngestCatalogSpecies_DerivesAuthoritativeName is the core P1 test: the
// cascade is searched with the SERVER's authoritative scientific_name for the
// catalog id (from Config.CatalogNames), never a caller-supplied value. The
// function takes no name parameter, so there is no way to pass a poisoned name —
// this asserts the resolved value actually reaches Search (SPEC §2.8).
func TestIngestCatalogSpecies_DerivesAuthoritativeName(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	in := catalogTestIngestor(src, store) // maps AAA0001 -> "Abelia chinensis"

	out, err := in.IngestCatalogSpecies(context.Background(), "AAA0001", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 4 {
		t.Fatalf("ingested %d, want 4", len(out.PerImage))
	}
	if got := src.lastQuery(); got != "Abelia chinensis" {
		t.Errorf("cascade searched %q, want the authoritative %q (client name must never drive the search)", got, "Abelia chinensis")
	}
}

// TestIngestCatalogSpecies_UnknownID_NoOp asserts a well-formed catalog id that
// is NOT one of the curated 1522 (absent from Config.CatalogNames) is refused: no
// search, no upload. Defense in depth behind the handler's 400 unknown_catalog_id
// — an attacker-fabricated id can never seed an external/ gallery (SPEC §2.8).
func TestIngestCatalogSpecies_UnknownID_NoOp(t *testing.T) {
	src := &mockSource{searchRet: cc0Cands(4)}
	store := newMockStore()
	in := catalogTestIngestor(src, store) // only AAA0001 is known

	out, err := in.IngestCatalogSpecies(context.Background(), "ZZZ9999", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.PerImage) != 0 {
		t.Errorf("unknown catalog id should ingest 0 images, got %+v", out)
	}
	if len(store.puts) != 0 {
		t.Error("unknown catalog id must never write to R2")
	}
	if q := src.lastQuery(); q != "" {
		t.Errorf("unknown catalog id must not search the cascade, but searched %q", q)
	}
}
