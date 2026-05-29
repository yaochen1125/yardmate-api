-- 002_plant_image_v2.sql
--
-- V2 on-demand cascade schema for the out-of-catalog plant image gallery.
-- See proxy/imageingest/SPEC.md §6.1 for column rationale + the contract.
--
-- Apply via Supabase Dashboard SQL Editor. Idempotent: safe to re-run.
--
-- EXPAND step (Slice 1) of an expand/contract migration. This migration ONLY
-- CREATEs the two new tables; it does NOT drop the v1 `plant_image_ingest`
-- table, because the v1 ledger Go code (ledger.go old API, ingestor.go) still
-- queries it until Slice 3 switches the callers over. The DROP lives in a later
-- migration (003_drop_plant_image_ingest.sql) shipped with the contract step,
-- so the old table is removed only after no code reads it. This intentionally
-- diverges from SPEC §6.1's "002 drops + creates" — split for build-green
-- incremental delivery; SPEC to be corrected in the Slice 3 PR.
--
-- Two-table model (replaces v1's single `plant_image_ingest`):
--   - plant_image_species : one row per slug (gallery aggregate + trigger dedup)
--   - plant_image_files   : one row per (slug, image_index) (per-image
--                           idempotency + attribution ledger)
--
-- slug == Slug(scientific_name) == iOS R2 key segment (PlantImageURL.slug,
-- trinomial, never folded). Distinct names that slug identically share a gallery.

CREATE TABLE IF NOT EXISTS plant_image_species (
  slug                    TEXT PRIMARY KEY,            -- == Slug(scientific_name); == iOS R2 key segment
  scientific_name         TEXT NOT NULL,               -- the searched name (audit; not re-slugged on read)
  image_count_requested   INT  NOT NULL DEFAULT 4,     -- N slots requested (clamp 1–6 in Go)
  image_count_filled      INT  NOT NULL DEFAULT 0,     -- COUNT(plant_image_files.status='ingested'); recomputed each pass
  last_triggered_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_plant_image_species_last_triggered ON plant_image_species (last_triggered_at);

CREATE TABLE IF NOT EXISTS plant_image_files (
  slug                    TEXT NOT NULL REFERENCES plant_image_species(slug) ON DELETE CASCADE,
  image_index             INT  NOT NULL,               -- 1 = primary/hero; 2..N = gallery slots
  status                  TEXT NOT NULL                -- see SPEC §3 outcome matrix
                            CHECK (status IN ('ingested','no_acceptable_image','failed','deferred_attribution')),
  r2_key                  TEXT,                         -- 'plant_images/{slug}/{i}.png' when ingested
  source                  TEXT NOT NULL DEFAULT 'wikimedia_commons',  -- 'inaturalist' | 'wikimedia_commons' | future 'gbif' | 'usda'
  source_url              TEXT,                         -- upstream URL (iNat photo page / Wikimedia File: page)
  pending_url             TEXT,                         -- chosen BY/SA candidate URL stored while deferred_attribution (write-only; §8 fast-path)
  license_code            TEXT,                         -- machine, e.g. 'cc-by-sa-4.0' / 'cc0' / 'pd'
  license_short           TEXT,                         -- display, e.g. 'CC BY-SA 4.0'
  license_url             TEXT,
  attribution_author      TEXT,                         -- HTML-stripped Artist (Wikimedia) / iNat user (NULL for CC0/PD)
  attribution_required    BOOLEAN NOT NULL DEFAULT FALSE,
  mime                    TEXT,
  bytes                   BIGINT,
  width                   INT,
  height                  INT,
  attempts                INT NOT NULL DEFAULT 0,        -- failed-attempt counter (observability)
  last_error              TEXT,
  created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (slug, image_index)
);
CREATE INDEX IF NOT EXISTS idx_plant_image_files_status ON plant_image_files (status);
CREATE INDEX IF NOT EXISTS idx_plant_image_files_source ON plant_image_files (source);
