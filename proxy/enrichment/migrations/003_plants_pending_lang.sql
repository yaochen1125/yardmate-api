-- 003_plants_pending_lang.sql
--
-- Multi-language enrichment (proxy/enrichment/SPEC.md §6 / §7).
-- Adds `lang` and swaps the primary key from (scientific_name_normalized)
-- to the composite (scientific_name_normalized, lang). One plant now has up
-- to one row per supported language; English is the master / fallback pivot.
--
-- Existing rows are the English master, so DEFAULT 'en' backfills them
-- correctly. `lang` holds a supported iOS Localizable.xcstrings code:
--   en, de, es, fr, it, ja, ko, pt, vi, zh-Hans, zh-Hant
--
-- Apply via Supabase Dashboard SQL Editor. Idempotent: safe to re-run.
--
-- Lookup contract (server-side):
--   SELECT data FROM plants_pending
--    WHERE scientific_name_normalized = $1 AND lang = $2
--      AND status IN ('pending','approved')
--    LIMIT 1
--
-- Insert contract (server-side, master on path-3 miss + each translated row):
--   INSERT INTO plants_pending
--     (scientific_name_normalized, lang, scientific_name, common_name, data,
--      source, source_version, generation_request_id)
--   VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
--   ON CONFLICT (scientific_name_normalized, lang) DO NOTHING;

ALTER TABLE plants_pending
  ADD COLUMN IF NOT EXISTS lang TEXT NOT NULL DEFAULT 'en';

-- Swap the primary key to the composite (scientific_name_normalized, lang),
-- idempotently: only act if `lang` is not already part of the PK.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_index i
    JOIN pg_attribute a
      ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
    WHERE i.indrelid = 'plants_pending'::regclass
      AND i.indisprimary
      AND a.attname = 'lang'
  ) THEN
    ALTER TABLE plants_pending DROP CONSTRAINT IF EXISTS plants_pending_pkey;
    ALTER TABLE plants_pending
      ADD CONSTRAINT plants_pending_pkey
      PRIMARY KEY (scientific_name_normalized, lang);
  END IF;
END $$;
