package enrichment

import (
	"context"
	"fmt"
)

// RecordIdentifyScan increments the day-bucketed successful-identify counter
// for an in-catalog species (dex_identify_daily, migration 012). One call per
// successful /v1/identify whose top suggestion resolved to a curated AAA id —
// every scan counts (repeats included), which is the Plantdex rarity metric
// ("1 in {n} scans"), unlike the device-deduped catalog_signals engagement
// rows. The day bucket is computed in UTC so the counter is client-timezone
// independent. Nil-safe (ErrDBUnavailable when the shared pool is absent,
// matching the enrichment DB contract).
func (d *DB) RecordIdentifyScan(ctx context.Context, plantID string) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	const stmt = `
		INSERT INTO dex_identify_daily (plant_id, day, count)
		VALUES ($1, (now() AT TIME ZONE 'utc')::date, 1)
		ON CONFLICT (plant_id, day) DO UPDATE
		SET count = dex_identify_daily.count + 1`
	if _, err := d.pool.Exec(ctx, stmt, plantID); err != nil {
		return fmt.Errorf("%w: identify scan upsert: %v", ErrDBUnavailable, err)
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
