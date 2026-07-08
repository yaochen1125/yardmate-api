-- 010_catalog_signals.sql
-- In-catalog usage counters, feeding the catalog admin tool's per-plant
-- 📷 identify / 🔍 search sort columns.
--
-- Separate from plant_signals (006/007) on purpose: plant_signals is a
-- promotion-priority signal for plants NOT yet in the catalog (iOS only reports
-- there when plantId == nil). This table records signals for plants that ARE in
-- the catalog, keyed directly by the AAA catalog id — no scientific-name mapping
-- needed, and zero impact on the promotion badges.
--
-- One row per (plant_id, kind, device install id); the composite primary key
-- dedups by device, so COUNT(*) per (plant_id, kind) is a distinct-device
-- (≈ distinct-person) tally. The server writes via POST /v1/plants/signal with
-- a plantId set (proxy/enrichment/signal.go, INSERT ... ON CONFLICT DO NOTHING).
-- 'garden' is intentionally excluded — in-catalog garden adds are already
-- tracked by garden_records.
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS catalog_signals (
    plant_id   text        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('identify', 'search')),
    device_id  text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plant_id, kind, device_id)
);

-- Aggregation lookup: COUNT(*) grouped by (plant_id, kind) for the sort columns.
CREATE INDEX IF NOT EXISTS catalog_signals_plant_kind_idx
    ON catalog_signals (plant_id, kind);

-- Row-Level Security (default-deny; the server's service role bypasses RLS).
-- Same posture as plant_signals (006) and the pending tables (005): RLS on + NO
-- policy so the public anon / authenticated roles cannot read/write via
-- PostgREST, while the server's DB-password pool keeps writing unchanged.
ALTER TABLE catalog_signals ENABLE ROW LEVEL SECURITY;
