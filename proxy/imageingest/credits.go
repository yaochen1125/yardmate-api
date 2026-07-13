package imageingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	creditsKey          = "plant_images/ingested/credits.json" // 库外 ingest 图分目录（SPEC §4）
	creditsCacheControl = "public, max-age=3600"               // short — changes as ingest runs (SPEC §2.7)
)

// CreditsManifest is the public credits.json shape (SPEC §2.7) the iOS Settings
// → Credits page reads from the CDN. No auth / no endpoint — same read-from-CDN
// pattern the app uses for catalog JSON.
type CreditsManifest struct {
	GeneratedAt string         `json:"generated_at"`
	Entries     []CreditsEntry `json:"entries"`
}

// CreditsEntry is one credited image (SPEC §2.7) — one per (slug, image_index)
// so a multi-image gallery carries row-distinct attribution.
type CreditsEntry struct {
	Slug           string `json:"slug"`
	ImageIndex     int    `json:"image_index"`
	ScientificName string `json:"scientific_name"`
	Source         string `json:"source"`
	LicenseShort   string `json:"license_short"`
	LicenseURL     string `json:"license_url"`
	Author         string `json:"author"`
	SourceURL      string `json:"source_url"`
}

// BuildCreditsManifest assembles the manifest from ingested file rows. FULL
// rebuild from current status='ingested' rows (SPEC §2.7 / §9 #15 — never a
// diff-append) so deleted / re-ingested slots drop or refresh cleanly. now is
// injectable for deterministic tests.
func BuildCreditsManifest(rows []FileRow, now time.Time) CreditsManifest {
	entries := make([]CreditsEntry, 0, len(rows))
	for _, r := range rows {
		// Skip rows with no license info (e.g. a minimal "skipped_exists" marker
		// for an orphan R2 object with no ledger attribution): a credits entry
		// with blank license/author is meaningless noise, and publishing it would
		// look like an attribution-less credit for a possibly BY/SA image.
		if strings.TrimSpace(r.LicenseShort) == "" {
			continue
		}
		entries = append(entries, CreditsEntry{
			Slug:           r.Slug,
			ImageIndex:     r.ImageIndex,
			ScientificName: r.ScientificName,
			Source:         r.Source,
			LicenseShort:   r.LicenseShort,
			LicenseURL:     r.LicenseURL,
			Author:         r.AttributionAuthor,
			SourceURL:      r.SourceURL,
		})
	}
	return CreditsManifest{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Entries:     entries,
	}
}

// rebuildCredits regenerates credits.json from the ledger and uploads it to R2
// (SPEC §2.7). Called at the end of every ingest so the public credits stay in
// sync with what is live in R2. A failure is returned (caller logs, does not
// abort the ingest).
func (in *Ingestor) rebuildCredits(ctx context.Context) error {
	// Serialize the read-modify-write across ALL slugs (finding #6). Single-flight
	// only mutexes per-slug, so two different slugs can both reach here at once; if
	// slug-A reads IngestedFiles, then slug-B reads + Puts, then slug-A Puts, A's
	// stale snapshot silently drops B's just-added credits. Holding creditsMu over
	// IngestedFiles + Put makes "newest snapshot wins" and keeps every live image
	// present in the published manifest.
	in.creditsMu.Lock()
	defer in.creditsMu.Unlock()

	rows, err := in.ledger.IngestedFiles(ctx)
	if err != nil {
		return fmt.Errorf("imageingest/credits: load ingested files: %w", err)
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
