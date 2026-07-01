-- 006_plant_signals.sql
-- Interest-signal counters for library-outside plants, feeding the catalog
-- promotion tool's "how many people searched / added to garden" badges.
--
-- One row per (normalized scientific name, kind, device install id). The
-- composite primary key dedups by device, so COUNT(*) per (name, kind) is a
-- distinct-device (≈ distinct-person) tally. The server writes via
-- POST /v1/plants/signal (proxy/enrichment/signal.go, INSERT ... ON CONFLICT
-- DO NOTHING); iOS only reports for non-catalog plants, gated by the user's
-- analytics consent.
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS plant_signals (
    scientific_name_normalized text        NOT NULL,
    kind                       text        NOT NULL CHECK (kind IN ('search', 'garden')),
    device_id                  text        NOT NULL,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scientific_name_normalized, kind, device_id)
);

-- Aggregation lookup: COUNT(*) grouped by (name, kind) for the digest badges.
CREATE INDEX IF NOT EXISTS plant_signals_name_kind_idx
    ON plant_signals (scientific_name_normalized, kind);

-- Row-Level Security (default-deny; the server's service role bypasses RLS).
-- Same posture as the pending tables (005): RLS on + NO policy so the public
-- anon / authenticated roles cannot read/write via PostgREST, while the
-- server's DB-password pool keeps writing unchanged.
ALTER TABLE plant_signals ENABLE ROW LEVEL SECURITY;
