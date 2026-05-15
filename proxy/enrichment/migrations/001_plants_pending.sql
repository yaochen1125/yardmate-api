-- 001_plants_pending.sql
--
-- Supabase table for V1 plant-detail enrichment cache + review queue.
-- See proxy/enrichment/SPEC.md §6 for column rationale + lookup contract.
--
-- Apply via Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.
--
-- Lookup contract (server-side):
--   SELECT data FROM plants_pending
--    WHERE scientific_name_normalized = $1
--      AND status IN ('pending','approved')
--    LIMIT 1
--
-- Insert contract (server-side, on path-3 miss):
--   INSERT INTO plants_pending
--     (scientific_name_normalized, scientific_name, common_name, data,
--      source, source_version, generation_request_id)
--   VALUES ($1, $2, $3, $4, $5, $6, $7)
--   ON CONFLICT (scientific_name_normalized) DO NOTHING;
--
-- Status transitions (manual via Supabase Dashboard in V1):
--   pending  → approved   (Yao reviewed + accepted)
--   pending  → rejected   (Yao reviewed + rejected; row kept for audit, excluded from lookups)
--   approved → pending    (Yao wants to re-review)

CREATE TABLE IF NOT EXISTS plants_pending (
  scientific_name_normalized TEXT PRIMARY KEY,
  scientific_name            TEXT NOT NULL,
  common_name                TEXT,
  data                       JSONB NOT NULL,
  status                     TEXT NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'approved', 'rejected')),
  source                     TEXT NOT NULL,
  source_version             TEXT,
  generation_request_id      TEXT,
  created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  reviewed_at                TIMESTAMPTZ,
  reviewed_by                TEXT,
  notes                      TEXT
);

CREATE INDEX IF NOT EXISTS idx_plants_pending_status
  ON plants_pending (status);

CREATE INDEX IF NOT EXISTS idx_plants_pending_created_at
  ON plants_pending (created_at DESC);

-- updated_at auto-bump trigger
CREATE OR REPLACE FUNCTION plants_pending_set_updated_at() RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS plants_pending_updated_at ON plants_pending;
CREATE TRIGGER plants_pending_updated_at
  BEFORE UPDATE ON plants_pending
  FOR EACH ROW EXECUTE FUNCTION plants_pending_set_updated_at();

-- No RLS in V1: the server uses the Supabase service role key (bypasses RLS).
-- When V1.x adds an iOS admin tab, the client will switch to the anon key + per-user JWT
-- and RLS policies must be added here.
