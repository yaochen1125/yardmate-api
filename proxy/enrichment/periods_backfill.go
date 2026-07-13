package enrichment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/yaochen1125/yardmate-api/proxy"
	"github.com/yaochen1125/yardmate-api/proxy/lang"
)

// periods_backfill.go — one-shot repair of EXISTING plants_pending rows so their
// stored *_period_short labels agree with *_months_north (the same invariant
// reconcilePeriods enforces on every new Generate/Translate). Rows written
// before that change can still carry the drift this fix prevents going forward
// (chart says one thing, header another); this backfill brings them in line.
//
// Run via cmd/backfill-periods (dry-run by default). It is a standalone one-shot,
// NOT part of the resident service.

// BackfillStats summarizes a BackfillPeriods run.
type BackfillStats struct {
	Scanned int // rows read
	Changed int // rows whose payload reconcilePeriods altered
	Failed  int // rows that could not be decoded / marshaled / written
}

// pendingRow is one plants_pending row's identity + raw JSONB payload.
type pendingRow struct {
	normalized string
	lang       string
	raw        []byte
}

// BackfillPeriods scans every served (pending/approved) plants_pending row,
// re-derives its *_period_short labels from the month arrays, and — when apply
// is true — writes back the rows that changed. With apply=false it only reports
// what WOULD change (dry-run). logf receives progress lines (pass log.Printf;
// nil is a no-op). The returned error is non-nil only on a fatal DB read; a
// per-row failure increments Failed and the run continues.
func BackfillPeriods(ctx context.Context, db *DB, apply bool, logf func(format string, args ...any)) (BackfillStats, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var stats BackfillStats

	rows, err := db.scanPendingRows(ctx)
	if err != nil {
		return stats, err
	}
	logf("scanned %d served rows", len(rows))

	for _, r := range rows {
		stats.Scanned++

		pd, oldBloom, changed, err := reconcileRow(r.raw, r.lang)
		if err != nil {
			stats.Failed++
			logf("FAIL %s/%s: %v", r.normalized, r.lang, err)
			continue
		}
		if !changed {
			continue // already consistent
		}
		stats.Changed++

		verb := "WOULD UPDATE"
		if apply {
			verb = "UPDATE"
		}
		logf("%s %s/%s: bloom_period_short %q -> %q", verb, r.normalized, r.lang, oldBloom, pd.BloomPeriodShort)

		if !apply {
			continue
		}
		if err := db.updateRowData(ctx, r.normalized, r.lang, pd); err != nil {
			stats.Failed++
			logf("FAIL %s/%s: write: %v", r.normalized, r.lang, err)
			continue
		}
	}

	return stats, nil
}

// reconcileRow decodes one row's JSONB payload, re-derives its period labels for
// the row's language, and reports whether the payload changed. Pure (no DB) so
// the decode/reconcile/diff decision is unit-testable. Returns the reconciled
// detail, the pre-reconcile bloom label (for logging), and changed=false when
// the row was already consistent.
//
// before/after are both struct→JSON encodings, so they share field order and
// whitespace — only a real field change (a relabeled period or a sanitized month
// array) makes them differ, not JSONB key-order quirks.
func reconcileRow(raw []byte, langCode string) (detail *proxy.PlantDetail, oldBloom string, changed bool, err error) {
	var pd proxy.PlantDetail
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, "", false, fmt.Errorf("decode: %w", err)
	}
	before, err := json.Marshal(&pd)
	if err != nil {
		return nil, "", false, fmt.Errorf("marshal before: %w", err)
	}
	oldBloom = pd.BloomPeriodShort

	reconcilePeriods(&pd, lang.Normalize(langCode))

	after, err := json.Marshal(&pd)
	if err != nil {
		return nil, "", false, fmt.Errorf("marshal after: %w", err)
	}
	return &pd, oldBloom, !bytes.Equal(before, after), nil
}

// scanPendingRows loads every served (pending/approved) row's identity + raw
// payload into memory. The library-external table is small (hundreds of rows,
// a few KB each), so reading it fully and closing the cursor before issuing
// UPDATEs avoids holding a read cursor open across writes.
func (d *DB) scanPendingRows(ctx context.Context) ([]pendingRow, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	const q = `
		SELECT scientific_name_normalized, lang, data
		FROM plants_pending
		WHERE status IN ('pending', 'approved')`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: scan rows: %v", ErrDBUnavailable, err)
	}
	defer rows.Close()

	var out []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.normalized, &r.lang, &r.raw); err != nil {
			return nil, fmt.Errorf("%w: scan row: %v", ErrDBUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows iter: %v", ErrDBUnavailable, err)
	}
	return out, nil
}

// updateRowData overwrites a single row's data JSONB, keyed on the composite PK.
// Only the data column is touched (any updated_at trigger fires on its own).
func (d *DB) updateRowData(ctx context.Context, normalized, lang string, data *proxy.PlantDetail) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("enrichment/db: marshal data: %w", err)
	}
	// Match scanPendingRows' status filter: a row flipped to 'rejected' between the
	// list scan and this write must NOT be overwritten (parity with
	// disease_supabase.go UpdateDiseaseSeverity).
	const stmt = `
		UPDATE plants_pending
		SET data = $3
		WHERE scientific_name_normalized = $1 AND lang = $2
		  AND status IN ('pending', 'approved')`
	if _, err := d.pool.Exec(ctx, stmt, normalized, lang, raw); err != nil {
		return fmt.Errorf("%w: update data: %v", ErrDBUnavailable, err)
	}
	return nil
}
