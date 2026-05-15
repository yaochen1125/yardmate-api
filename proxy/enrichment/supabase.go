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

// Lookup returns the stored PlantDetail for a normalized scientific name, or
// (nil, nil) on miss. Only status IN ('pending','approved') rows are returned;
// 'rejected' rows are excluded. Approved rows are preferred when both could
// exist (defensive — PK uniqueness means at most one row in practice).
//
// pgx.ErrNoRows collapses to (nil, nil) — miss is not an error. Real failures
// (connection / scan / JSONB decode) wrap ErrDBUnavailable.
func (d *DB) Lookup(ctx context.Context, normalized string) (*proxy.PlantDetail, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		SELECT data
		FROM plants_pending
		WHERE scientific_name_normalized = $1
		  AND status IN ('pending', 'approved')
		ORDER BY (status = 'approved') DESC
		LIMIT 1`
	var raw []byte
	err := d.pool.QueryRow(ctx, q, normalized).Scan(&raw)
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
	return &pd, nil
}

// InsertParams bundles the columns for a new plants_pending row.
type InsertParams struct {
	Normalized      string             // PK, == NormalizeScientificName(ScientificName)
	ScientificName  string             // original un-normalized form, preserved for audit
	CommonName      string             // optional user hint; empty -> NULL
	Data            *proxy.PlantDetail // full payload, stored as JSONB
	Source          string             // e.g. "openai-gpt-4o-mini-2024-07-18"
	SourceVersion   string             // prompt revision tag (e.g. "v1"); empty -> NULL
	GenerationReqID string             // OpenAI chatcmpl id; empty -> NULL
}

// Insert performs INSERT ... ON CONFLICT (scientific_name_normalized) DO NOTHING.
// Returns inserted=true when a new row was created, false when the PK already
// existed (the standard concurrent first-caller race per SPEC §2.1 step 5 +
// pitfall §9 #2). Callers handle the false case by re-Lookup'ing.
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
	raw, err := json.Marshal(p.Data)
	if err != nil {
		return false, fmt.Errorf("enrichment/db: marshal data: %w", err)
	}
	const stmt = `
		INSERT INTO plants_pending (
			scientific_name_normalized,
			scientific_name,
			common_name,
			data,
			status,
			source,
			source_version,
			generation_request_id
		) VALUES ($1, $2, NULLIF($3, ''), $4, 'pending', $5, NULLIF($6, ''), NULLIF($7, ''))
		ON CONFLICT (scientific_name_normalized) DO NOTHING`
	tag, err := d.pool.Exec(ctx, stmt,
		p.Normalized,
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
