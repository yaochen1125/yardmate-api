-- 001_plant_image_ingest.sql
--
-- Supabase ledger for the V1 out-of-catalog hero-image ingest pipeline.
-- See proxy/imageingest/SPEC.md §6.1 for column rationale + the contract.
--
-- Apply via Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.
--
-- This table serves three jobs at once:
--   - idempotency       (skip slugs already ingested)
--   - negative cache    (don't re-search species with no free image every pass)
--   - attribution ledger (feed the iOS Credits page via credits.json)
--
-- PK = slug (== Slug(scientific_name) == iOS R2 key segment). Distinct names
-- that slug identically intentionally share one hero.
--
-- Re-ingest a species = manually DELETE its row (and the R2 object) → next
-- pass re-attempts (mirrors enrichment's delete-to-regenerate Dashboard flow).
--
-- This package only READS plants_pending (seed names); it NEVER writes that
-- table. plant_image_ingest is owned solely by proxy/imageingest.

CREATE TABLE IF NOT EXISTS plant_image_ingest (
  slug                TEXT PRIMARY KEY,            -- == Slug(scientific_name); == iOS R2 key segment
  scientific_name     TEXT NOT NULL,               -- the searched name (audit; not re-slugged on read)
  status              TEXT NOT NULL                -- see SPEC §3 outcome matrix
                        CHECK (status IN ('ingested','no_acceptable_image','failed','deferred_attribution')),
  r2_key              TEXT,                         -- 'plant_images/{slug}/hero.png' when ingested
  pending_thumburl    TEXT,                         -- chosen BY/SA rendition URL stored while deferred_attribution → upload on flag-flip without re-search (§2.4)
  source              TEXT NOT NULL DEFAULT 'wikimedia_commons',
  source_file_page    TEXT,                         -- Commons File: page URL (re-derive attribution anytime)
  license_code        TEXT,                         -- machine, e.g. 'cc-by-sa-4.0' / 'cc0' / 'pd'
  license_short       TEXT,                         -- display, e.g. 'CC BY-SA 4.0'
  license_url         TEXT,
  attribution_author  TEXT,                         -- HTML-stripped Artist (NULL for CC0/PD)
  attribution_required BOOLEAN NOT NULL DEFAULT FALSE,
  mime                TEXT,
  bytes               BIGINT,
  width               INT,
  height              INT,
  attempts            INT NOT NULL DEFAULT 0,        -- failed-attempt counter for backoff/cap
  last_error          TEXT,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_plant_image_ingest_status ON plant_image_ingest (status);
