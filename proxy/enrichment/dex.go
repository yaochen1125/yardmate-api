package enrichment

import (
	"context"
	"fmt"
)

// AddIdentifyScans applies one batch of Plantdex identify-counter increments
// to dex_identify_daily (migration 012): one multi-row upsert per flush of
// proxy.IdentifyScanCounter, NOT one round-trip per identify — the shared pool
// (MaxConns 10) must never be fanned out on by the request path (SPEC §9 #18).
// Every scan counts (repeats included) — the Plantdex rarity metric
// ("1 in {n} scans") — unlike the device-deduped catalog_signals rows. The day
// bucket is the flush day in UTC (≤ one flush interval of skew around
// midnight, irrelevant to the all-time aggregation). Nil-safe
// (ErrDBUnavailable when the shared pool is absent).
func (d *DB) AddIdentifyScans(ctx context.Context, counts map[string]int64) error {
	ids := make([]string, 0, len(counts))
	ns := make([]int64, 0, len(counts))
	for id, n := range counts {
		if id == "" || n <= 0 {
			continue
		}
		ids = append(ids, id)
		ns = append(ns, n)
	}
	if len(ids) == 0 {
		return nil // nothing to write — a no-op regardless of pool state
	}
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	const stmt = `
		INSERT INTO dex_identify_daily (plant_id, day, count)
		SELECT t.plant_id, (now() AT TIME ZONE 'utc')::date, t.cnt
		FROM unnest($1::text[], $2::bigint[]) AS t(plant_id, cnt)
		ON CONFLICT (plant_id, day) DO UPDATE
		SET count = dex_identify_daily.count + EXCLUDED.count`
	if _, err := d.pool.Exec(ctx, stmt, ids, ns); err != nil {
		return fmt.Errorf("%w: identify scan batch upsert: %v", ErrDBUnavailable, err)
	}
	return nil
}

// IdentifyCountTotals returns the all-time successful-identify scan total per
// species (SUM over the dex_identify_daily day buckets). Consumed by the
// proxy/rarity aggregation job to derive quantile tiers and oneIn ratios.
// Nil-safe (ErrDBUnavailable when the shared pool is absent).
func (d *DB) IdentifyCountTotals(ctx context.Context) (map[string]int64, error) {
	if d == nil || d.pool == nil {
		return nil, ErrDBUnavailable
	}
	rows, err := d.pool.Query(ctx, `
		SELECT plant_id, SUM(count)::bigint
		FROM dex_identify_daily
		GROUP BY plant_id`)
	if err != nil {
		return nil, fmt.Errorf("%w: identify totals query: %v", ErrDBUnavailable, err)
	}
	defer rows.Close()
	totals := make(map[string]int64)
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("%w: identify totals scan: %v", ErrDBUnavailable, err)
		}
		totals[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: identify totals rows: %v", ErrDBUnavailable, err)
	}
	return totals, nil
}
