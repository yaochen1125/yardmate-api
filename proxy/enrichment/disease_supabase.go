package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/yaochen1125/yardmate-api/proxy"
)

// DiseaseInsertParams are the columns InsertDisease writes (besides the
// constant status='pending'). Mirrors InsertParams (the plant side).
type DiseaseInsertParams struct {
	Normalized      string // PK part 1 = proxy.NormalizeDiseaseName(name)
	Lang            string // PK part 2 = NormalizeLang(...) supported code (master or translated)
	CatalogID       string // empty → mint a fresh O id; non-empty → REUSE it (translated rows share the master's O id, §7)
	DiseaseName     string // original display name (e.g. "Drought Stress")
	Detail          *proxy.StructuredDiseaseDetail
	Source          string
	SourceVersion   string
	GenerationReqID string
}

// LookupDisease reads a cached out-of-catalog disease by (normalized name, lang).
// Returns (detail, catalogID, nil) on hit, (nil, "", nil) on miss, or
// (_, _, ErrDBUnavailable) on a real DB error. approved wins over pending
// (composite PK is unique per language, so this is defensive).
func (d *DB) LookupDisease(ctx context.Context, normalized, lang string) (*proxy.StructuredDiseaseDetail, string, error) {
	const q = `
		SELECT data, catalog_id
		FROM diseases_pending
		WHERE disease_name_normalized = $1
		  AND lang = $2
		  AND status IN ('pending', 'approved')
		ORDER BY (status = 'approved') DESC
		LIMIT 1`
	var raw []byte
	var catalogID string
	err := d.pool.QueryRow(ctx, q, normalized, lang).Scan(&raw, &catalogID)
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

// LookupDiseaseAny returns ANY stored master for a disease regardless of
// language, plus its catalog id and the language it found, or (nil, "", "", nil)
// on miss. Mirrors DB.LookupAny on the plant side: used to avoid generating a
// second independent master when one already exists in another language and the
// caller is racing its backfill (the §7 one-master invariant), AND so a
// translated row can REUSE the master's O id rather than mint a new one. Prefers
// approved rows, then English (the best translation source). Only status IN
// ('pending','approved') rows are considered.
//
// pgx.ErrNoRows collapses to (nil, "", "", nil). Real failures wrap ErrDBUnavailable.
func (d *DB) LookupDiseaseAny(ctx context.Context, normalized string) (*proxy.StructuredDiseaseDetail, string, string, error) {
	const q = `
		SELECT data, catalog_id, lang
		FROM diseases_pending
		WHERE disease_name_normalized = $1
		  AND status IN ('pending', 'approved')
		ORDER BY (status = 'approved') DESC, (lang = 'en') DESC
		LIMIT 1`
	var raw []byte
	var catalogID, lang string
	err := d.pool.QueryRow(ctx, q, normalized).Scan(&raw, &catalogID, &lang)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "", nil // miss ≠ error
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: disease lookup any: %v", ErrDBUnavailable, err)
	}
	var detail proxy.StructuredDiseaseDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, "", "", fmt.Errorf("%w: decode disease row: %v", ErrDBUnavailable, err)
	}
	return &detail, catalogID, lang, nil
}

// InsertDisease persists a generated disease row. When p.CatalogID is empty it
// mints a fresh O-series id from diseases_other_seq (the master); when it is
// non-empty it REUSES that id (a translated row sharing its master's O id, §7).
// Returns (catalogID, true, nil) on insert; ("", false, nil) when the (name,
// lang) row already exists (race loser — caller re-Lookups the winner);
// (_, _, ErrDBUnavailable) on a real DB error. Detail MUST already be back-filled
// (steps/remedies denormalized) — the stored row is self-contained.
func (d *DB) InsertDisease(ctx context.Context, p DiseaseInsertParams) (string, bool, error) {
	if p.Detail == nil {
		return "", false, fmt.Errorf("%w: nil disease detail", ErrDBUnavailable)
	}
	if p.Normalized == "" {
		return "", false, fmt.Errorf("%w: empty normalized name", ErrDBUnavailable)
	}
	if p.Lang == "" {
		return "", false, fmt.Errorf("%w: empty lang", ErrDBUnavailable)
	}
	raw, err := json.Marshal(p.Detail)
	if err != nil {
		return "", false, fmt.Errorf("%w: encode disease detail: %v", ErrDBUnavailable, err)
	}
	const stmt = `
		INSERT INTO diseases_pending (
			disease_name_normalized, lang, catalog_id, disease_name, data,
			status, source, source_version, generation_request_id
		) VALUES (
			$1, $2, COALESCE(NULLIF($3, ''), 'O' || nextval('diseases_other_seq')), $4, $5,
			'pending', $6, NULLIF($7, ''), NULLIF($8, '')
		)
		ON CONFLICT (disease_name_normalized, lang) DO NOTHING
		RETURNING catalog_id`
	var catalogID string
	err = d.pool.QueryRow(ctx, stmt, p.Normalized, p.Lang, p.CatalogID, p.DiseaseName, raw,
		p.Source, p.SourceVersion, p.GenerationReqID).Scan(&catalogID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil // ON CONFLICT DO NOTHING → row exists (race loser)
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: disease insert: %v", ErrDBUnavailable, err)
	}
	return catalogID, true, nil
}

// DiseaseSweepItem is one enriched disease missing at least one supported
// language. Mirrors PlantSweepItem on the plant side; CatalogID is the master's
// O id, which every translated row must reuse (§7 one-id invariant).
type DiseaseSweepItem struct {
	Normalized   string
	DiseaseName  string
	CatalogID    string
	SourceLang   string // the chosen master's language (the pivot for translation)
	Master       *proxy.StructuredDiseaseDetail
	MissingLangs []string
}

// IncompleteDiseaseMasters returns every enriched disease (status
// pending/approved) whose distinct-language count is below the full supported
// set, paired with the languages it still lacks. Read half of the periodic
// Sweeper's disease pass — mirrors IncompletePlantMasters. The master pivot
// prefers English; the carried catalog_id is reused by every translated row.
func (d *DB) IncompleteDiseaseMasters(ctx context.Context) ([]DiseaseSweepItem, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		WITH agg AS (
			SELECT disease_name_normalized, array_agg(DISTINCT lang) AS langs
			FROM diseases_pending
			WHERE status IN ('pending', 'approved')
			GROUP BY disease_name_normalized
			HAVING count(DISTINCT lang) < $1
		)
		SELECT DISTINCT ON (p.disease_name_normalized)
			p.disease_name_normalized, p.disease_name, p.catalog_id, p.lang, p.data, a.langs
		FROM diseases_pending p
		JOIN agg a USING (disease_name_normalized)
		WHERE p.status IN ('pending', 'approved')
		ORDER BY p.disease_name_normalized, (p.lang = 'en') DESC, p.lang`
	rows, err := d.pool.Query(ctx, q, len(SupportedLangs))
	if err != nil {
		return nil, fmt.Errorf("%w: list incomplete diseases: %v", ErrDBUnavailable, err)
	}
	defer rows.Close()
	var out []DiseaseSweepItem
	for rows.Next() {
		var it DiseaseSweepItem
		var raw []byte
		var have []string
		if err := rows.Scan(&it.Normalized, &it.DiseaseName, &it.CatalogID, &it.SourceLang, &raw, &have); err != nil {
			return nil, fmt.Errorf("%w: scan incomplete disease: %v", ErrDBUnavailable, err)
		}
		var detail proxy.StructuredDiseaseDetail
		if err := json.Unmarshal(raw, &detail); err != nil {
			return nil, fmt.Errorf("%w: decode incomplete disease: %v", ErrDBUnavailable, err)
		}
		it.Master = &detail
		it.MissingLangs = missingLangs(have)
		if len(it.MissingLangs) > 0 {
			out = append(out, it)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate incomplete diseases: %v", ErrDBUnavailable, err)
	}
	return out, nil
}
