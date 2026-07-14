package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yaochen1125/yardmate-api/proxy"
)

// ErrDBUnavailable is the typed error returned to the handler when a pgx
// operation fails (connection / query parse / row scan / JSONB decode). The
// HTTP layer maps it to 502 db_unavailable per SPEC §3.
var ErrDBUnavailable = errors.New("enrichment: db unavailable")

// DB wraps a pgx connection pool against Supabase Postgres plants_pending.
// Built once at startup; safe for concurrent use.
//
// Use the Supabase Session Pooler DSN (port 5432, host
// aws-0-<region>.pooler.supabase.com, user postgres.<project_ref>) — direct
// Postgres connections are IPv6-only and silently fail on networks without
// reliable IPv6 (SPEC §9 #15).
type DB struct {
	pool *pgxpool.Pool
}

// NewDB connects via pgx pool. The pool defaults to MaxConns=10 unless the
// DSN itself specifies pool_max_conns. Caller MUST Close() at shutdown to
// release sockets cleanly.
func NewDB(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("enrichment/db: parse dsn: %w", err)
	}
	if cfg.MaxConns < 1 {
		cfg.MaxConns = 10
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("enrichment/db: connect: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases the pool. Safe to call on nil.
func (d *DB) Close() {
	if d == nil || d.pool == nil {
		return
	}
	d.pool.Close()
}

// Ping issues a fast round-trip to verify the DSN works. Used at startup so
// a bad DSN fails fast rather than silently 502'ing on first request.
func (d *DB) Ping(ctx context.Context) error {
	if d == nil || d.pool == nil {
		return errors.New("enrichment/db: nil DB")
	}
	return d.pool.Ping(ctx)
}

// Lookup returns the stored PlantDetail for a (normalized scientific name,
// lang) pair, or (nil, nil) on miss. Only status IN ('pending','approved') rows
// are returned; 'rejected' rows are excluded. Approved rows are preferred when
// both could exist (defensive — composite PK uniqueness means at most one row
// per language in practice).
//
// pgx.ErrNoRows collapses to (nil, nil) — miss is not an error. Real failures
// (connection / scan / JSONB decode) wrap ErrDBUnavailable.
func (d *DB) Lookup(ctx context.Context, normalized, lang string) (*proxy.PlantDetail, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		SELECT data
		FROM plants_pending
		WHERE scientific_name_normalized = $1
		  AND lang = $2
		  AND status IN ('pending', 'approved')
		ORDER BY (status = 'approved') DESC
		LIMIT 1`
	var raw []byte
	err := d.pool.QueryRow(ctx, q, normalized, lang).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: lookup: %v", ErrDBUnavailable, err)
	}
	var pd proxy.PlantDetail
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, fmt.Errorf("%w: decode row data: %v", ErrDBUnavailable, err)
	}
	// Reconcile on read: rows persisted before period-derivation existed can
	// carry a *_period_short that disagrees with *_months_north. Deriving here
	// makes EVERY served row consistent regardless of when it was written, so
	// correctness no longer depends on running the backfill (cmd/backfill-periods
	// still cleans the stored bytes). Idempotent for already-consistent rows;
	// lang is the normalized requested code (bloom.go).
	reconcilePeriods(&pd, lang)
	return &pd, nil
}

// LookupAny returns ANY stored master for a plant regardless of language, plus
// the language it found, or (nil, "", nil) on miss. Used to avoid generating a
// second independent master when one already exists in another language and the
// caller is racing its backfill (SPEC §7 one-master invariant). Prefers approved
// rows, then English (the best translation source). Only status IN
// ('pending','approved') rows are considered.
//
// pgx.ErrNoRows collapses to (nil, "", nil). Real failures wrap ErrDBUnavailable.
func (d *DB) LookupAny(ctx context.Context, normalized string) (*proxy.PlantDetail, string, error) {
	if d == nil || d.pool == nil {
		return nil, "", ErrDBUnavailable
	}
	const q = `
		SELECT data, lang
		FROM plants_pending
		WHERE scientific_name_normalized = $1
		  AND status IN ('pending', 'approved')
		ORDER BY (status = 'approved') DESC, (lang = 'en') DESC
		LIMIT 1`
	var raw []byte
	var lang string
	err := d.pool.QueryRow(ctx, q, normalized).Scan(&raw, &lang)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: lookup any: %v", ErrDBUnavailable, err)
	}
	var pd proxy.PlantDetail
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, "", fmt.Errorf("%w: decode row data: %v", ErrDBUnavailable, err)
	}
	// Reconcile on read using the row's own language (see Lookup). Keeps the
	// master consistent before it is served directly or used as a translation
	// source. Idempotent for already-consistent rows (bloom.go).
	reconcilePeriods(&pd, lang)
	return &pd, lang, nil
}

// DeleteUserRows deletes all of a user's account data from the Supabase
// Postgres tables that are keyed on the auth user id (auth.uid() == user_id):
// diary_entries then garden_records. Used by POST /v1/account/delete (account
// deletion). Both DELETEs run on the shared pgx pool; the user id is the
// Supabase JWT `sub` (a UUID string) so it is passed as a bind param, never
// interpolated.
//
// Returns the first DELETE error wrapped in ErrDBUnavailable — the handler
// maps that to 500 (the auth user is NOT removed unless the rows are gone, so
// the caller can safely retry without orphaning storage/auth). Tables are
// deleted child-first (diary_entries before garden_records) defensively in case
// a FK is ever added; today they are independent.
func (d *DB) DeleteUserRows(ctx context.Context, userID string) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	if userID == "" {
		return errors.New("enrichment/db: delete user rows: empty user id")
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM diary_entries WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("%w: delete diary_entries: %v", ErrDBUnavailable, err)
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM garden_records WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("%w: delete garden_records: %v", ErrDBUnavailable, err)
	}
	// ad_attribution stores the Supabase auth user id when a signed-in install is
	// attributed (attribution.go). Included in the hard-delete set so account
	// deletion stays end-to-end: only the attributed row keyed to THIS user is
	// removed (device-only / organic rows have user_id NULL and are untouched).
	if _, err := d.pool.Exec(ctx, `DELETE FROM ad_attribution WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("%w: delete ad_attribution: %v", ErrDBUnavailable, err)
	}
	return nil
}

// NativeRegionRow identifies one legacy non-English row whose native_region is
// still canonical English and needs localizing (SPEC §7 v5 backfill).
type NativeRegionRow struct {
	Normalized   string   // PK part 1
	Lang         string   // PK part 2 (always != "en")
	NativeRegion []string // current (English) native_region from data JSONB
}

// ListNativeRegionBackfillRows returns every non-English row written before v5
// (source_version IS NULL OR < 'v5') whose native_region is a non-empty array —
// the legacy rows carrying English place names that v5 localizes. English rows
// are excluded (their native_region stays English by design). Rows with an empty
// native_region are excluded (nothing to translate). status IN
// ('pending','approved') only. Read-only; safe to call repeatedly.
//
// The comparison is lexicographic `< 'v5'` (the codebase's established
// stale-row-targeting convention — see prompt.go PromptVersion history), NOT
// `<> 'v5'`: not-equal would re-select rows already advanced to a FUTURE version
// (v6+) on a re-run and needlessly re-translate them. `< 'v5'` is forward-safe —
// only genuinely older rows (v1..v4, NULL) match.
func (d *DB) ListNativeRegionBackfillRows(ctx context.Context) ([]NativeRegionRow, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		SELECT scientific_name_normalized, lang, data->'native_region'
		FROM plants_pending
		WHERE lang <> 'en'
		  AND status IN ('pending', 'approved')
		  AND (source_version IS NULL OR source_version < 'v5')
		  AND jsonb_typeof(data->'native_region') = 'array'
		  AND jsonb_array_length(data->'native_region') > 0
		ORDER BY scientific_name_normalized, lang`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: list native_region backfill: %v", ErrDBUnavailable, err)
	}
	defer rows.Close()
	var out []NativeRegionRow
	for rows.Next() {
		var r NativeRegionRow
		var raw []byte
		if err := rows.Scan(&r.Normalized, &r.Lang, &raw); err != nil {
			return nil, fmt.Errorf("%w: scan native_region row: %v", ErrDBUnavailable, err)
		}
		if err := json.Unmarshal(raw, &r.NativeRegion); err != nil {
			return nil, fmt.Errorf("%w: decode native_region: %v", ErrDBUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate native_region rows: %v", ErrDBUnavailable, err)
	}
	return out, nil
}

// UpdateNativeRegion patches just data->'native_region' on one (normalized, lang)
// row via jsonb_set and stamps source_version to mark the row v5-conformant. It
// touches NOTHING else — the row's already-localized prose is preserved. The
// source_version bump makes the backfill idempotent (a re-run's WHERE clause
// excludes rows already at v5). Returns the number of rows updated (0 if the row
// vanished between list and update — benign).
func (d *DB) UpdateNativeRegion(ctx context.Context, normalized, lang string, regions []string, version string) (int64, error) {
	if d == nil || d.pool == nil {
		return 0, ErrDBUnavailable
	}
	raw, err := json.Marshal(regions)
	if err != nil {
		return 0, fmt.Errorf("enrichment/db: marshal native_region: %w", err)
	}
	const stmt = `
		UPDATE plants_pending
		SET data = jsonb_set(data, '{native_region}', $3::jsonb, true),
		    source_version = NULLIF($4, '')
		WHERE scientific_name_normalized = $1 AND lang = $2`
	tag, err := d.pool.Exec(ctx, stmt, normalized, lang, raw, version)
	if err != nil {
		return 0, fmt.Errorf("%w: update native_region: %v", ErrDBUnavailable, err)
	}
	return tag.RowsAffected(), nil
}

// InsertParams bundles the columns for a new plants_pending row.
type InsertParams struct {
	Normalized      string             // PK part 1, == NormalizeScientificName(ScientificName)
	Lang            string             // PK part 2, normalized supported code (master or translated)
	ScientificName  string             // original un-normalized form, preserved for audit
	CommonName      string             // optional user hint; empty -> NULL
	Data            *proxy.PlantDetail // full payload, stored as JSONB
	Source          string             // e.g. "openai-gpt-4o-mini-2024-07-18" (master) / "...-translated"
	SourceVersion   string             // prompt revision tag (e.g. "v4"); empty -> NULL
	GenerationReqID string             // OpenAI chatcmpl id; empty -> NULL
}

// Insert performs INSERT ... ON CONFLICT (scientific_name_normalized, lang) DO
// NOTHING. Returns inserted=true when a new row was created, false when the
// composite PK already existed (the concurrent first-caller race per SPEC §2.1
// step 5 + pitfall §9 #2, AND the idempotent backfill re-trigger per §7).
// Callers handle the false case by re-Lookup'ing.
func (d *DB) Insert(ctx context.Context, p InsertParams) (bool, error) {
	if d == nil || d.pool == nil {
		return false, ErrDBUnavailable
	}
	if p.Data == nil {
		return false, errors.New("enrichment/db: insert: nil data")
	}
	if p.Normalized == "" {
		return false, errors.New("enrichment/db: insert: empty normalized name")
	}
	if p.Lang == "" {
		return false, errors.New("enrichment/db: insert: empty lang")
	}
	raw, err := json.Marshal(p.Data)
	if err != nil {
		return false, fmt.Errorf("enrichment/db: marshal data: %w", err)
	}
	const stmt = `
		INSERT INTO plants_pending (
			scientific_name_normalized,
			lang,
			scientific_name,
			common_name,
			data,
			status,
			source,
			source_version,
			generation_request_id
		) VALUES ($1, $2, $3, NULLIF($4, ''), $5, 'pending', $6, NULLIF($7, ''), NULLIF($8, ''))
		ON CONFLICT (scientific_name_normalized, lang) DO NOTHING`
	tag, err := d.pool.Exec(ctx, stmt,
		p.Normalized,
		p.Lang,
		p.ScientificName,
		p.CommonName,
		raw,
		p.Source,
		p.SourceVersion,
		p.GenerationReqID,
	)
	if err != nil {
		return false, fmt.Errorf("%w: insert: %v", ErrDBUnavailable, err)
	}
	return tag.RowsAffected() == 1, nil
}

// PlantSweepItem is one enriched plant that is missing at least one supported
// language. Master is the preferred pivot row (English when present, else the
// lexically-first language) and MissingLangs is exactly the set still absent —
// the Sweeper translates only those, off this master.
type PlantSweepItem struct {
	Normalized     string
	ScientificName string
	CommonHint     string
	SourceLang     string // the chosen master's language (the pivot for translation)
	Master         *proxy.PlantDetail
	MissingLangs   []string
}

// IncompletePlantMasters returns every enriched plant whose language coverage is
// below the full supported set, paired with the languages it still lacks. It is
// the read half of the periodic Sweeper: the in-request backfill is
// fire-and-forget with no retry, so legacy rows (from before multilingual
// support) and jobs lost to queue saturation / restarts / transient LLM errors
// never self-heal without this. Read-only.
//
// Language presence (the agg CTE) is counted across ALL statuses, INCLUDING
// 'rejected'. A rejected translation keeps its (normalized, lang) primary key, so
// Insert's ON CONFLICT DO NOTHING would silently drop any re-translation —
// counting a rejected language as "missing" would make the sweep re-pay an LLM
// call for it on every tick forever with no progress. A reviewer's rejection is
// deliberate; leave it. The master PIVOT, by contrast, is still chosen only from
// pending/approved rows (a rejected row must never be a translation source), so a
// plant whose only rows are rejected yields no pivot and is correctly skipped.
//
// The pivot prefers English (the best translation source); for the rare plant
// with no English row yet, the lexically-first available language is used. $1 is
// the full supported-language count (len(SupportedLangs)); a plant with that many
// distinct language rows (in any status) is complete and excluded.
func (d *DB) IncompletePlantMasters(ctx context.Context) ([]PlantSweepItem, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		WITH agg AS (
			SELECT scientific_name_normalized, array_agg(DISTINCT lang) AS langs
			FROM plants_pending
			GROUP BY scientific_name_normalized
			HAVING count(DISTINCT lang) < $1
		)
		SELECT DISTINCT ON (p.scientific_name_normalized)
			p.scientific_name_normalized, p.scientific_name,
			COALESCE(p.common_name, ''), p.lang, p.data, a.langs
		FROM plants_pending p
		JOIN agg a USING (scientific_name_normalized)
		WHERE p.status IN ('pending', 'approved')
		ORDER BY p.scientific_name_normalized, (p.lang = 'en') DESC, p.lang`
	rows, err := d.pool.Query(ctx, q, len(SupportedLangs))
	if err != nil {
		return nil, fmt.Errorf("%w: list incomplete plants: %v", ErrDBUnavailable, err)
	}
	defer rows.Close()
	var out []PlantSweepItem
	for rows.Next() {
		var it PlantSweepItem
		var raw []byte
		var have []string
		if err := rows.Scan(&it.Normalized, &it.ScientificName, &it.CommonHint, &it.SourceLang, &raw, &have); err != nil {
			return nil, fmt.Errorf("%w: scan incomplete plant: %v", ErrDBUnavailable, err)
		}
		var pd proxy.PlantDetail
		if err := json.Unmarshal(raw, &pd); err != nil {
			return nil, fmt.Errorf("%w: decode incomplete plant: %v", ErrDBUnavailable, err)
		}
		it.Master = &pd
		it.MissingLangs = missingLangs(have)
		if len(it.MissingLangs) > 0 {
			out = append(out, it)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate incomplete plants: %v", ErrDBUnavailable, err)
	}
	return out, nil
}

// missingLangs returns the supported languages absent from have, preserving
// SupportedLangs order (English first). Shared by the plant + disease sweeps.
func missingLangs(have []string) []string {
	present := make(map[string]bool, len(have))
	for _, l := range have {
		present[l] = true
	}
	var missing []string
	for _, l := range SupportedLangs {
		if !present[l] {
			missing = append(missing, l)
		}
	}
	return missing
}
