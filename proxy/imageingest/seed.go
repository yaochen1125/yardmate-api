package imageingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSeedUnavailable wraps a pgx failure reading the seed list.
var ErrSeedUnavailable = errors.New("imageingest/seed: db unavailable")

// SeedReader reads out-of-catalog scientific names from the enrichment-owned
// plants_pending table (READ ONLY — SPEC §1.2 / §6.2). It NEVER writes that
// table and NEVER imports the enrichment Go package; the coupling is the table
// + column name only (pitfall §9 #4). Own small pgx pool (MaxConns=2).
type SeedReader struct {
	pool *pgxpool.Pool
}

// NewSeedReader connects via a small pgx pool against the same Supabase DSN
// enrichment uses. Caller closes at shutdown.
func NewSeedReader(ctx context.Context, dsn string) (*SeedReader, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("imageingest/seed: parse dsn: %w", err)
	}
	if cfg.MaxConns < 1 || cfg.MaxConns > 2 {
		cfg.MaxConns = 2
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("imageingest/seed: connect: %w", err)
	}
	return &SeedReader{pool: pool}, nil
}

// Close releases the pool. Safe on nil.
func (s *SeedReader) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

// Ping verifies the DSN at startup.
func (s *SeedReader) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("imageingest/seed: nil reader")
	}
	return s.pool.Ping(ctx)
}

// Seeds returns the scientific names of out-of-catalog plants the server has
// recorded (status IN ('pending','approved')). READ ONLY (SPEC §6.2). The
// caller filters already-done slugs in Go (Slug is a Go function, not SQL —
// SQL-side slug filtering is a §8 optimization not done in V1).
func (s *SeedReader) Seeds(ctx context.Context) ([]string, error) {
	if s == nil || s.pool == nil {
		return nil, ErrSeedUnavailable
	}
	const q = `
		SELECT scientific_name
		FROM plants_pending
		WHERE status IN ('pending', 'approved')`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: query: %v", ErrSeedUnavailable, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("%w: scan: %v", ErrSeedUnavailable, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iter: %v", ErrSeedUnavailable, err)
	}
	return out, nil
}
