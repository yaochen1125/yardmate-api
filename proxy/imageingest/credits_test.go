package imageingest

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestBuildCreditsManifest_Shape(t *testing.T) {
	rows := []FileRow{
		{
			Slug:                "rosa-regina-sueciae",
			ImageIndex:          1,
			ScientificName:      "Rosa regina sueciae",
			Source:              "inaturalist",
			LicenseShort:        "CC BY-SA 4.0",
			LicenseURL:          "https://creativecommons.org/licenses/by-sa/4.0/",
			AttributionAuthor:   "Jane Doe",
			AttributionRequired: true,
			SourceURL:           "https://www.inaturalist.org/photos/123",
		},
	}
	fixed := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	m := BuildCreditsManifest(rows, fixed)

	if m.GeneratedAt != "2026-05-27T12:00:00Z" {
		t.Errorf("GeneratedAt = %q", m.GeneratedAt)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(m.Entries))
	}
	e := m.Entries[0]
	if e.Slug != "rosa-regina-sueciae" || e.ImageIndex != 1 || e.ScientificName != "Rosa regina sueciae" {
		t.Errorf("entry slug/index/name wrong: %+v", e)
	}
	if e.Source != "inaturalist" || e.LicenseShort != "CC BY-SA 4.0" || e.LicenseURL == "" || e.Author != "Jane Doe" || e.SourceURL == "" {
		t.Errorf("entry missing fields: %+v", e)
	}

	// Round-trip JSON to confirm the exact wire keys (SPEC §2.7).
	b, _ := json.Marshal(m)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("marshal/unmarshal: %v", err)
	}
	if _, ok := raw["generated_at"]; !ok {
		t.Errorf("missing generated_at key")
	}
	entries, ok := raw["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries key wrong")
	}
	entry := entries[0].(map[string]any)
	for _, k := range []string{"slug", "image_index", "scientific_name", "source", "license_short", "license_url", "author", "source_url"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("entry missing key %q", k)
		}
	}
}

func TestBuildCreditsManifest_Empty(t *testing.T) {
	m := BuildCreditsManifest(nil, time.Unix(0, 0))
	if m.Entries == nil {
		t.Errorf("Entries should be non-nil (empty slice) for stable [] JSON")
	}
	if len(m.Entries) != 0 {
		t.Errorf("expected 0 entries")
	}
}

// TestBuildCreditsManifest_SkipsBlankLicense locks the review fix: a minimal
// "skipped_exists" marker for an orphan R2 object (no ledger attribution) has an
// empty license and must NOT appear as a blank-attribution credits entry.
func TestBuildCreditsManifest_SkipsBlankLicense(t *testing.T) {
	rows := []FileRow{
		{Slug: "a", ImageIndex: 1, ScientificName: "A plant", LicenseShort: "CC0"},
		{Slug: "b", ImageIndex: 1, ScientificName: "B plant", LicenseShort: ""}, // orphan marker
	}
	m := BuildCreditsManifest(rows, time.Unix(0, 0))
	if len(m.Entries) != 1 || m.Entries[0].Slug != "a" {
		t.Fatalf("entries = %+v, want only the licensed row 'a'", m.Entries)
	}
}

func TestRebuildCredits_FullRebuild(t *testing.T) {
	// First rebuild with two rows; then the ledger drops one → the next
	// rebuild reflects only the remaining row (full rebuild, not diff-append).
	store := newMockStore()
	ledger := newMockLedger()
	ledger.ingested = []FileRow{
		{Slug: "a", ImageIndex: 1, ScientificName: "A plant", LicenseShort: "CC0"},
		{Slug: "b", ImageIndex: 1, ScientificName: "B plant", LicenseShort: "CC0"},
	}
	in := newIngestor(&mockSource{}, store, ledger, Config{})

	if err := in.rebuildCredits(context.Background()); err != nil {
		t.Fatalf("rebuild 1: %v", err)
	}
	var m1 CreditsManifest
	_ = json.Unmarshal(store.puts[creditsKey], &m1)
	if len(m1.Entries) != 2 {
		t.Fatalf("rebuild 1 entries = %d, want 2", len(m1.Entries))
	}

	ledger.ingested = []FileRow{{Slug: "a", ImageIndex: 1, ScientificName: "A plant", LicenseShort: "CC0"}}
	if err := in.rebuildCredits(context.Background()); err != nil {
		t.Fatalf("rebuild 2: %v", err)
	}
	var m2 CreditsManifest
	_ = json.Unmarshal(store.puts[creditsKey], &m2)
	if len(m2.Entries) != 1 || m2.Entries[0].Slug != "a" {
		t.Fatalf("rebuild 2 entries = %d (want 1, slug a): %+v", len(m2.Entries), m2.Entries)
	}
}
