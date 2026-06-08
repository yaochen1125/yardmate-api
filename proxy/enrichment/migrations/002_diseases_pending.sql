-- 002_diseases_pending.sql
-- disease enrichment store (proxy/enrichment/SPEC_disease.md).
--
-- Out-of-catalog diagnose diseases: when /v1/diagnose produces a disease whose
-- name maps to no curated catalog entry (mapCatalogID returns nil), the server
-- generates catalog-quality structured detail via gpt-4o-mini (referencing the
-- shared step/remedy pools, back-filled), assigns an O-series catalog id, stores
-- it here keyed by normalized disease name, and reuses it for everyone who later
-- hits the same disease. The hero image stays the user's own photo (client-side).
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.
-- Mirrors 001_plants_pending.sql. No RLS in V1 (server uses the DB password DSN).

-- Mints the O-series catalog id for each new out-of-catalog disease.
-- Gaps are fine (a race loser consumes a value it never persists).
CREATE SEQUENCE IF NOT EXISTS diseases_other_seq;

CREATE TABLE IF NOT EXISTS diseases_pending (
  disease_name_normalized TEXT PRIMARY KEY,                 -- normalizeDiseaseName(name); plant-agnostic
  catalog_id              TEXT UNIQUE NOT NULL,             -- 'O' || nextval(diseases_other_seq), e.g. "O7"
  disease_name            TEXT NOT NULL,                    -- original (display) name, e.g. "Drought Stress"
  data                    JSONB NOT NULL,                   -- StructuredDiseaseDetail, ALREADY back-filled (frozen)
  status                  TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'approved', 'rejected')),
  source                  TEXT NOT NULL,                    -- e.g. openai-gpt-4o-mini-2024-07-18
  source_version          TEXT,                             -- disease PromptVersion
  generation_request_id   TEXT,
  created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  reviewed_at             TIMESTAMPTZ,
  reviewed_by             TEXT,
  notes                   TEXT
);

CREATE INDEX IF NOT EXISTS idx_diseases_pending_status     ON diseases_pending (status);
CREATE INDEX IF NOT EXISTS idx_diseases_pending_created_at ON diseases_pending (created_at DESC);

-- updated_at auto-bump (mirrors plants_pending).
CREATE OR REPLACE FUNCTION diseases_pending_set_updated_at() RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS diseases_pending_updated_at ON diseases_pending;
CREATE TRIGGER diseases_pending_updated_at
  BEFORE UPDATE ON diseases_pending
  FOR EACH ROW EXECUTE FUNCTION diseases_pending_set_updated_at();

-- Contract:
--   Lookup: SELECT data, catalog_id FROM diseases_pending
--           WHERE disease_name_normalized = $1 AND status IN ('pending','approved')
--           ORDER BY (status = 'approved') DESC LIMIT 1
--   Insert: INSERT INTO diseases_pending
--             (disease_name_normalized, catalog_id, disease_name, data, status,
--              source, source_version, generation_request_id)
--           VALUES ($1, 'O' || nextval('diseases_other_seq'), $2, $3, 'pending',
--                   $4, NULLIF($5,''), NULLIF($6,''))
--           ON CONFLICT (disease_name_normalized) DO NOTHING
--           RETURNING catalog_id
--   (race loser: RowsAffected 0 / no RETURNING row → re-Lookup the winner)
-- Review: pending → approved/rejected via Dashboard. Approved rows may later be
--         promoted into the curated diseases.json (V1.1).
