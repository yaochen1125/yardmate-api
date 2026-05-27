package imageingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	creditsKey          = "plant_images/credits.json"
	creditsCacheControl = "public, max-age=3600" // short — changes as ingest runs (SPEC §2.7)
)

// CreditsManifest is the public credits.json shape (SPEC §2.7) the iOS Settings
// → Credits page reads from the CDN. No auth / no endpoint — same read-from-CDN
// pattern the app uses for catalog JSON.
type CreditsManifest struct {
	GeneratedAt string         `json:"generated_at"`
	Entries     []CreditsEntry `json:"entries"`
}

// CreditsEntry is one credited image (SPEC §2.7).
type CreditsEntry struct {
	Slug           string `json:"slug"`
	ScientificName string `json:"scientific_name"`
	LicenseShort   string `json:"license_short"`
	LicenseURL     string `json:"license_url"`
	Author         string `json:"author"`
	SourceURL      string `json:"source_url"`
}

// BuildCreditsManifest assembles the manifest from ingested ledger rows. FULL
// rebuild from current status='ingested' rows (SPEC §2.7 / §9 #15 — never a
// diff-append) so deleted / re-ingested slugs drop or refresh cleanly. now is
// injectable for deterministic tests.
func BuildCreditsManifest(rows []LedgerRow, now time.Time) CreditsManifest {
	entries := make([]CreditsEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, CreditsEntry{
			Slug:           r.Slug,
			ScientificName: r.ScientificName,
			LicenseShort:   r.LicenseShort,
			LicenseURL:     r.LicenseURL,
			Author:         r.AttributionAuthor,
			SourceURL:      r.SourceFilePage,
		})
	}
	return CreditsManifest{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Entries:     entries,
	}
}

// rebuildCredits regenerates credits.json from the ledger and uploads it to R2
// (SPEC §2.7). Called at the end of every batch so the public credits stay in
// sync with what is live in R2. A failure is returned (caller logs, does not
// abort the batch).
func (in *Ingestor) rebuildCredits(ctx context.Context) error {
	rows, err := in.ledger.IngestedRows(ctx)
	if err != nil {
		return fmt.Errorf("imageingest/credits: load ingested rows: %w", err)
	}
	manifest := BuildCreditsManifest(rows, time.Now())
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("imageingest/credits: marshal: %w", err)
	}
	if err := in.store.Put(ctx, creditsKey, body, "application/json", creditsCacheControl); err != nil {
		return fmt.Errorf("imageingest/credits: put: %w", err)
	}
	return nil
}
