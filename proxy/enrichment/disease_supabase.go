package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/yaochen1125/yardmate-api/proxy"
)

// DiseaseInsertParams are the columns InsertDisease writes (besides the minted
// catalog_id and the constant status='pending'). Mirrors InsertParams.
type DiseaseInsertParams struct {
	Normalized      string // PK = proxy.NormalizeDiseaseName(name)
	DiseaseName     string // original display name (e.g. "Drought Stress")
	Detail          *proxy.StructuredDiseaseDetail
	Source          string
	SourceVersion   string
	GenerationReqID string
}

// LookupDisease reads a cached out-of-catalog disease by normalized name.
// Returns (detail, catalogID, nil) on hit, (nil, "", nil) on miss, or
// (_, _, ErrDBUnavailable) on a real DB error. approved wins over pending
// (PK is unique, so this is defensive).
func (d *DB) LookupDisease(ctx context.Context, normalized string) (*proxy.StructuredDiseaseDetail, string, error) {
	const q = `
		SELECT data, catalog_id
		FROM diseases_pending
		WHERE disease_name_normalized = $1
		  AND status IN ('pending', 'approved')
		ORDER BY (status = 'approved') DESC
		LIMIT 1`
	var raw []byte
	var catalogID string
	err := d.pool.QueryRow(ctx, q, normalized).Scan(&raw, &catalogID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil // miss ≠ error
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: disease lookup: %v", ErrDBUnavailable, err)
	}
	var detail proxy.StructuredDiseaseDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, "", fmt.Errorf("%w: decode disease row: %v", ErrDBUnavailable, err)
	}
	return &detail, catalogID, nil
}

// InsertDisease persists a generated disease, minting an O-series catalog id from
// diseases_other_seq. Returns (catalogID, true, nil) on insert; ("", false, nil)
// when the row already exists (race loser — caller re-Lookups the winner);
// (_, _, ErrDBUnavailable) on a real DB error. Detail MUST already be back-filled
// (steps/remedies denormalized) — the stored row is self-contained.
func (d *DB) InsertDisease(ctx context.Context, p DiseaseInsertParams) (string, bool, error) {
	if p.Detail == nil {
		return "", false, fmt.Errorf("%w: nil disease detail", ErrDBUnavailable)
	}
	if p.Normalized == "" {
		return "", false, fmt.Errorf("%w: empty normalized name", ErrDBUnavailable)
	}
	raw, err := json.Marshal(p.Detail)
	if err != nil {
		return "", false, fmt.Errorf("%w: encode disease detail: %v", ErrDBUnavailable, err)
	}
	const stmt = `
		INSERT INTO diseases_pending (
			disease_name_normalized, catalog_id, disease_name, data,
			status, source, source_version, generation_request_id
		) VALUES (
			$1, 'O' || nextval('diseases_other_seq'), $2, $3,
			'pending', $4, NULLIF($5, ''), NULLIF($6, '')
		)
		ON CONFLICT (disease_name_normalized) DO NOTHING
		RETURNING catalog_id`
	var catalogID string
	err = d.pool.QueryRow(ctx, stmt, p.Normalized, p.DiseaseName, raw,
		p.Source, p.SourceVersion, p.GenerationReqID).Scan(&catalogID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil // ON CONFLICT DO NOTHING → row exists (race loser)
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: disease insert: %v", ErrDBUnavailable, err)
	}
	return catalogID, true, nil
}
