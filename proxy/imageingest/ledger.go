package imageingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLedgerUnavailable wraps any pgx failure on the plant_image_ingest ledger
// (connect / query / scan). Per-species, the ingestor logs it and balances an
// outcome; it never aborts a whole batch.
var ErrLedgerUnavailable = errors.New("imageingest/ledger: db unavailable")

// SourceWikimediaCommons is the v1 image source + v2 cascade fallback (SPEC
// §2.4.2 / §6.1). The ledger's source column is forward-compatible (gbif/usda).
const SourceWikimediaCommons = "wikimedia_commons"

// SourceINaturalist is the v2 primary cascade source (SPEC §2.4.1 / §6.1).
const SourceINaturalist = "inaturalist"

// IngestStatus is the persisted per-slug ingest state (SPEC §3 / §6.1). It is
// the table's status column; the in-flight IngestOutcome.Status (SPEC §3) maps
// onto it (e.g. skipped_exists → ingested, source_error/upload_error → failed).
type IngestStatus string

const (
	StatusIngested        IngestStatus = "ingested"
	StatusNoAcceptableImg IngestStatus = "no_acceptable_image"
	StatusFailed          IngestStatus = "failed"
	StatusDeferredAttrib  IngestStatus = "deferred_attribution"
)

// LedgerRow mirrors a plant_image_ingest row (SPEC §6.1). Pointer/empty
// handling: optional columns map to "" / 0 in Go and NULL in SQL via NULLIF.
type LedgerRow struct {
	Slug                string
	ScientificName      string
	Status              IngestStatus
	R2Key               string
	PendingThumbURL     string // chosen BY/SA rendition stored while deferred_attribution
	Source              string
	SourceFilePage      string
	LicenseCode         string
	LicenseShort        string
	LicenseURL          string
	AttributionAuthor   string
	AttributionRequired bool
	MIME                string
	Bytes               int64
	Width               int
	Height              int
	Attempts            int
	LastError           string
}

// Ledger wraps a small pgx pool over plant_image_ingest. Built once at startup;
// safe for concurrent use. Mirrors enrichment.DB but owns a SEPARATE pool
// (MaxConns=2) — this is a low-frequency batch worker, not a request hot path
// (SPEC §1.5).
type Ledger struct {
	pool *pgxpool.Pool
}

// NewLedger connects via a small pgx pool (MaxConns clamped to 2 unless the DSN
// requests fewer). Caller closes at shutdown. Uses the Supabase Session-Pooler
// DSN — same secret enrichment uses (SPEC §1.5 / §9 there #15).
func NewLedger(ctx context.Context, dsn string) (*Ledger, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("imageingest/ledger: parse dsn: %w", err)
	}
	if cfg.MaxConns < 1 || cfg.MaxConns > 2 {
		cfg.MaxConns = 2
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("imageingest/ledger: connect: %w", err)
	}
	return &Ledger{pool: pool}, nil
}

// Close releases the pool. Safe on nil.
func (l *Ledger) Close() {
	if l == nil || l.pool == nil {
		return
	}
	l.pool.Close()
}

// Ping verifies the DSN at startup (fail fast vs silently erroring later).
func (l *Ledger) Ping(ctx context.Context) error {
	if l == nil || l.pool == nil {
		return errors.New("imageingest/ledger: nil ledger")
	}
	return l.pool.Ping(ctx)
}

// Lookup returns the ledger row for a slug, or (nil, nil) on miss. pgx.ErrNoRows
// collapses to a miss; real failures wrap ErrLedgerUnavailable.
func (l *Ledger) Lookup(ctx context.Context, slug string) (*LedgerRow, error) {
	if l == nil || l.pool == nil {
		return nil, ErrLedgerUnavailable
	}
	const q = `
		SELECT slug, scientific_name, status,
		       COALESCE(r2_key,''), COALESCE(pending_thumburl,''),
		       COALESCE(source,''), COALESCE(source_file_page,''),
		       COALESCE(license_code,''), COALESCE(license_short,''), COALESCE(license_url,''),
		       COALESCE(attribution_author,''), attribution_required,
		       COALESCE(mime,''), COALESCE(bytes,0), COALESCE(width,0), COALESCE(height,0),
		       attempts, COALESCE(last_error,'')
		FROM plant_image_ingest
		WHERE slug = $1`
	var row LedgerRow
	err := l.pool.QueryRow(ctx, q, slug).Scan(
		&row.Slug, &row.ScientificName, &row.Status,
		&row.R2Key, &row.PendingThumbURL,
		&row.Source, &row.SourceFilePage,
		&row.LicenseCode, &row.LicenseShort, &row.LicenseURL,
		&row.AttributionAuthor, &row.AttributionRequired,
		&row.MIME, &row.Bytes, &row.Width, &row.Height,
		&row.Attempts, &row.LastError,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: lookup: %v", ErrLedgerUnavailable, err)
	}
	return &row, nil
}

// Upsert writes the outcome of an ingest attempt: INSERT ... ON CONFLICT (slug)
// DO UPDATE (SPEC §6.1). attempts is set absolutely by the caller (it reads the
// prior row, increments, and passes the new value) so the write stays a single
// statement. updated_at is bumped to NOW() on every upsert.
func (l *Ledger) Upsert(ctx context.Context, row LedgerRow) error {
	if l == nil || l.pool == nil {
		return ErrLedgerUnavailable
	}
	if row.Slug == "" {
		return errors.New("imageingest/ledger: upsert empty slug")
	}
	source := row.Source
	if source == "" {
		source = SourceWikimediaCommons
	}
	const stmt = `
		INSERT INTO plant_image_ingest (
			slug, scientific_name, status, r2_key, pending_thumburl,
			source, source_file_page, license_code, license_short, license_url,
			attribution_author, attribution_required, mime, bytes, width, height,
			attempts, last_error, updated_at
		) VALUES (
			$1, $2, $3, NULLIF($4,''), NULLIF($5,''),
			$6, NULLIF($7,''), NULLIF($8,''), NULLIF($9,''), NULLIF($10,''),
			NULLIF($11,''), $12, NULLIF($13,''), NULLIF($14,0)::BIGINT, NULLIF($15,0)::INT, NULLIF($16,0)::INT,
			$17, NULLIF($18,''), NOW()
		)
		ON CONFLICT (slug) DO UPDATE SET
			scientific_name      = EXCLUDED.scientific_name,
			status               = EXCLUDED.status,
			r2_key               = EXCLUDED.r2_key,
			pending_thumburl     = EXCLUDED.pending_thumburl,
			source               = EXCLUDED.source,
			source_file_page     = EXCLUDED.source_file_page,
			license_code         = EXCLUDED.license_code,
			license_short        = EXCLUDED.license_short,
			license_url          = EXCLUDED.license_url,
			attribution_author   = EXCLUDED.attribution_author,
			attribution_required = EXCLUDED.attribution_required,
			mime                 = EXCLUDED.mime,
			bytes                = EXCLUDED.bytes,
			width                = EXCLUDED.width,
			height               = EXCLUDED.height,
			attempts             = EXCLUDED.attempts,
			last_error           = EXCLUDED.last_error,
			updated_at           = NOW()`
	_, err := l.pool.Exec(ctx, stmt,
		row.Slug, row.ScientificName, string(row.Status), row.R2Key, row.PendingThumbURL,
		source, row.SourceFilePage, row.LicenseCode, row.LicenseShort, row.LicenseURL,
		row.AttributionAuthor, row.AttributionRequired, row.MIME, row.Bytes, row.Width, row.Height,
		row.Attempts, row.LastError,
	)
	if err != nil {
		return fmt.Errorf("%w: upsert: %v", ErrLedgerUnavailable, err)
	}
	return nil
}

// IngestedRows returns all status='ingested' rows for the credits.json rebuild
// (SPEC §2.7 — full rebuild, never diff-append). Ordered by slug for stable
// output.
func (l *Ledger) IngestedRows(ctx context.Context) ([]LedgerRow, error) {
	if l == nil || l.pool == nil {
		return nil, ErrLedgerUnavailable
	}
	const q = `
		SELECT slug, scientific_name,
		       COALESCE(license_short,''), COALESCE(license_url,''),
		       COALESCE(attribution_author,''), attribution_required,
		       COALESCE(source_file_page,'')
		FROM plant_image_ingest
		WHERE status = 'ingested'
		ORDER BY slug`
	rows, err := l.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: ingested rows: %v", ErrLedgerUnavailable, err)
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		r.Status = StatusIngested
		if err := rows.Scan(
			&r.Slug, &r.ScientificName,
			&r.LicenseShort, &r.LicenseURL,
			&r.AttributionAuthor, &r.AttributionRequired,
			&r.SourceFilePage,
		); err != nil {
			return nil, fmt.Errorf("%w: scan ingested: %v", ErrLedgerUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: ingested rows iter: %v", ErrLedgerUnavailable, err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// V2 two-table API (SPEC §6.1). EXPAND step: added alongside the v1 single-table
// API above. Slice 3 (contract) migrates ingestor/handlers/credits/main onto
// these, then deletes the v1 API + LedgerRow. These methods target the new
// plant_image_species + plant_image_files tables (migration 002); they do NOT
// touch plant_image_ingest. Unused until Slice 3 — intentional.
// ---------------------------------------------------------------------------

// SpeciesRow mirrors a plant_image_species row (SPEC §6.1) — species-level
// gallery aggregate + trigger dedup state.
type SpeciesRow struct {
	Slug                string
	ScientificName      string
	ImageCountRequested int
	ImageCountFilled    int
}

// FileRow mirrors a plant_image_files row (SPEC §6.1) — one per (slug,
// image_index). ScientificName is not a column on plant_image_files; it is
// populated only by IngestedFiles (JOIN plant_image_species) for credits.json.
type FileRow struct {
	Slug                string
	ImageIndex          int
	Status              IngestStatus
	R2Key               string
	Source              string
	SourceURL           string
	PendingURL          string // chosen BY/SA rendition stored while deferred_attribution
	LicenseCode         string
	LicenseShort        string
	LicenseURL          string
	AttributionAuthor   string
	AttributionRequired bool
	MIME                string
	Bytes               int64
	Width               int
	Height              int
	Attempts            int
	LastError           string
	ScientificName      string // JOIN-only (credits); empty on Lookup
}

// UpsertSpecies writes/refreshes the species-level row: INSERT ... ON CONFLICT
// (slug) DO UPDATE (SPEC §6.1, §2.1 flow step 3). last_triggered_at + updated_at
// bump to NOW() on every call. image_count_requested is set by the caller;
// image_count_filled is owned by RecomputeCount, so this never overwrites it.
func (l *Ledger) UpsertSpecies(ctx context.Context, row SpeciesRow) error {
	if l == nil || l.pool == nil {
		return ErrLedgerUnavailable
	}
	if row.Slug == "" {
		return errors.New("imageingest/ledger: upsert species empty slug")
	}
	requested := row.ImageCountRequested
	if requested <= 0 {
		requested = 4
	}
	const stmt = `
		INSERT INTO plant_image_species (
			slug, scientific_name, image_count_requested, last_triggered_at, updated_at
		) VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (slug) DO UPDATE SET
			scientific_name       = EXCLUDED.scientific_name,
			image_count_requested = EXCLUDED.image_count_requested,
			last_triggered_at     = NOW(),
			updated_at            = NOW()`
	if _, err := l.pool.Exec(ctx, stmt, row.Slug, row.ScientificName, requested); err != nil {
		return fmt.Errorf("%w: upsert species: %v", ErrLedgerUnavailable, err)
	}
	return nil
}

// UpsertFile writes the outcome of one (slug, image_index) attempt: INSERT ...
// ON CONFLICT (slug, image_index) DO UPDATE (SPEC §6.1). attempts is set
// absolutely by the caller. Empty optional columns map to NULL via NULLIF.
// Source defaults to wikimedia_commons (NOT NULL column) when unset.
func (l *Ledger) UpsertFile(ctx context.Context, row FileRow) error {
	if l == nil || l.pool == nil {
		return ErrLedgerUnavailable
	}
	if row.Slug == "" {
		return errors.New("imageingest/ledger: upsert file empty slug")
	}
	source := row.Source
	if source == "" {
		source = SourceWikimediaCommons
	}
	const stmt = `
		INSERT INTO plant_image_files (
			slug, image_index, status, r2_key, source, source_url, pending_url,
			license_code, license_short, license_url, attribution_author, attribution_required,
			mime, bytes, width, height, attempts, last_error, updated_at
		) VALUES (
			$1, $2, $3, NULLIF($4,''), $5, NULLIF($6,''), NULLIF($7,''),
			NULLIF($8,''), NULLIF($9,''), NULLIF($10,''), NULLIF($11,''), $12,
			NULLIF($13,''), NULLIF($14,0)::BIGINT, NULLIF($15,0)::INT, NULLIF($16,0)::INT, $17, NULLIF($18,''), NOW()
		)
		ON CONFLICT (slug, image_index) DO UPDATE SET
			status               = EXCLUDED.status,
			r2_key               = EXCLUDED.r2_key,
			source               = EXCLUDED.source,
			source_url           = EXCLUDED.source_url,
			pending_url          = EXCLUDED.pending_url,
			license_code         = EXCLUDED.license_code,
			license_short        = EXCLUDED.license_short,
			license_url          = EXCLUDED.license_url,
			attribution_author   = EXCLUDED.attribution_author,
			attribution_required = EXCLUDED.attribution_required,
			mime                 = EXCLUDED.mime,
			bytes                = EXCLUDED.bytes,
			width                = EXCLUDED.width,
			height               = EXCLUDED.height,
			attempts             = EXCLUDED.attempts,
			last_error           = EXCLUDED.last_error,
			updated_at           = NOW()`
	_, err := l.pool.Exec(ctx, stmt,
		row.Slug, row.ImageIndex, string(row.Status), row.R2Key, source, row.SourceURL, row.PendingURL,
		row.LicenseCode, row.LicenseShort, row.LicenseURL, row.AttributionAuthor, row.AttributionRequired,
		row.MIME, row.Bytes, row.Width, row.Height, row.Attempts, row.LastError,
	)
	if err != nil {
		return fmt.Errorf("%w: upsert file: %v", ErrLedgerUnavailable, err)
	}
	return nil
}

// LookupFile returns the per-image row for (slug, image_index), or (nil, nil)
// on miss. pgx.ErrNoRows collapses to a miss; real failures wrap
// ErrLedgerUnavailable.
func (l *Ledger) LookupFile(ctx context.Context, slug string, imageIndex int) (*FileRow, error) {
	if l == nil || l.pool == nil {
		return nil, ErrLedgerUnavailable
	}
	const q = `
		SELECT slug, image_index, status,
		       COALESCE(r2_key,''), COALESCE(source,''), COALESCE(source_url,''), COALESCE(pending_url,''),
		       COALESCE(license_code,''), COALESCE(license_short,''), COALESCE(license_url,''),
		       COALESCE(attribution_author,''), attribution_required,
		       COALESCE(mime,''), COALESCE(bytes,0), COALESCE(width,0), COALESCE(height,0),
		       attempts, COALESCE(last_error,'')
		FROM plant_image_files
		WHERE slug = $1 AND image_index = $2`
	var row FileRow
	err := l.pool.QueryRow(ctx, q, slug, imageIndex).Scan(
		&row.Slug, &row.ImageIndex, &row.Status,
		&row.R2Key, &row.Source, &row.SourceURL, &row.PendingURL,
		&row.LicenseCode, &row.LicenseShort, &row.LicenseURL,
		&row.AttributionAuthor, &row.AttributionRequired,
		&row.MIME, &row.Bytes, &row.Width, &row.Height,
		&row.Attempts, &row.LastError,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: lookup file: %v", ErrLedgerUnavailable, err)
	}
	return &row, nil
}

// RecomputeCount recomputes plant_image_species.image_count_filled from the
// authoritative plant_image_files rows (SPEC §2.1 step 5, §9 #17 — never trust a
// separately-incremented counter) and returns the new value.
func (l *Ledger) RecomputeCount(ctx context.Context, slug string) (int, error) {
	if l == nil || l.pool == nil {
		return 0, ErrLedgerUnavailable
	}
	const stmt = `
		UPDATE plant_image_species
		SET image_count_filled = (
		        SELECT COUNT(*) FROM plant_image_files
		        WHERE slug = $1 AND status = 'ingested'
		    ),
		    updated_at = NOW()
		WHERE slug = $1
		RETURNING image_count_filled`
	var filled int
	if err := l.pool.QueryRow(ctx, stmt, slug).Scan(&filled); err != nil {
		return 0, fmt.Errorf("%w: recompute count: %v", ErrLedgerUnavailable, err)
	}
	return filled, nil
}

// IngestedFiles returns all status='ingested' file rows joined to their species
// (for scientific_name) for the credits.json rebuild (SPEC §2.7 — full rebuild,
// one entry per (slug, image_index)). Ordered (slug, image_index) for stable
// output.
func (l *Ledger) IngestedFiles(ctx context.Context) ([]FileRow, error) {
	if l == nil || l.pool == nil {
		return nil, ErrLedgerUnavailable
	}
	const q = `
		SELECT f.slug, f.image_index, s.scientific_name,
		       COALESCE(f.source,''),
		       COALESCE(f.license_short,''), COALESCE(f.license_url,''),
		       COALESCE(f.attribution_author,''), f.attribution_required,
		       COALESCE(f.source_url,'')
		FROM plant_image_files f
		JOIN plant_image_species s ON s.slug = f.slug
		WHERE f.status = 'ingested'
		ORDER BY f.slug, f.image_index`
	rows, err := l.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: ingested files: %v", ErrLedgerUnavailable, err)
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		var r FileRow
		r.Status = StatusIngested
		if err := rows.Scan(
			&r.Slug, &r.ImageIndex, &r.ScientificName,
			&r.Source,
			&r.LicenseShort, &r.LicenseURL,
			&r.AttributionAuthor, &r.AttributionRequired,
			&r.SourceURL,
		); err != nil {
			return nil, fmt.Errorf("%w: scan ingested file: %v", ErrLedgerUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: ingested files iter: %v", ErrLedgerUnavailable, err)
	}
	return out, nil
}
