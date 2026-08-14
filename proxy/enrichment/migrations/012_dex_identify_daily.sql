-- 012_dex_identify_daily.sql
-- Plantdex rarity: per-species daily counters of successful in-catalog
-- identifications, feeding the dex/rarity.json aggregation job (proxy/rarity —
-- quantile tiers + oneIn, published to the content CDN).
--
-- Separate from catalog_signals (010) on purpose: catalog_signals is a
-- device-deduped engagement signal (one row per plant/kind/device,
-- client-reported via POST /v1/plants/signal, gated by the user's analytics
-- consent) feeding the catalog admin tool's sort columns. Rarity needs the
-- number of successful identify *scans* (contract: oneIn = "1 in {n} scans"),
-- counted server-side at the moment /v1/identify succeeds — every scan counts,
-- repeats included, no client opt-in bias.
--
-- Anonymous by design: day-bucketed counters only, NO device_id / user id.
-- Nothing personal to consent-gate, and nothing for account deletion to erase
-- (DeleteUserRows intentionally does not touch this table).
--
-- Writer: proxy/enrichment/dex.go RecordIdentifyScan — best-effort detached
-- goroutine on the identify success path (INSERT ... ON CONFLICT DO UPDATE
-- count+1; day computed in UTC). The AAA0000 "unknown" sentinel is excluded by
-- the writer. Reader: IdentifyCountTotals (SUM(count) per plant_id).
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS dex_identify_daily (
    plant_id text   NOT NULL,
    day      date   NOT NULL,
    count    bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (plant_id, day)
);

-- Aggregation reads SUM(count) GROUP BY plant_id — the (plant_id, day) primary
-- key index already serves that scan; no extra index needed at this size
-- (catalog species × days, a few hundred new rows/day at most).

-- Row-Level Security (default-deny; the server's DB-password pool bypasses
-- RLS). Same posture as plant_signals (006) and catalog_signals (010): RLS on
-- + NO policy so the public anon / authenticated roles cannot read or write
-- via PostgREST.
ALTER TABLE dex_identify_daily ENABLE ROW LEVEL SECURITY;
