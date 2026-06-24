-- 004_diseases_pending_lang.sql
--
-- Multi-language disease enrichment (proxy/enrichment/SPEC_disease.md, mirrors
-- 003_plants_pending_lang.sql for the plant side).
-- Adds `lang` and swaps the primary key from (disease_name_normalized)
-- to the composite (disease_name_normalized, lang). One out-of-catalog disease
-- now has up to one row per supported language; English is the master /
-- fallback pivot.
--
-- Existing rows are the English master, so DEFAULT 'en' backfills them
-- correctly. `lang` holds a supported iOS Localizable.xcstrings code:
--   en, de, es, fr, it, ja, ko, pt, vi, zh-Hans, zh-Hant
--
-- catalog_id is the O-series id that iOS uses as the disease's STABLE identity.
-- It must be SHARED across every language row of the same disease (the iOS
-- detail page keys on it). So we DROP the per-row UNIQUE constraint on
-- catalog_id: the master row mints a new 'O' || nextval(diseases_other_seq),
-- and every translated row of the same disease REUSES that same O id (the
-- server passes it through DiseaseInsertParams.CatalogID). It stays unique
-- PER DISEASE (one O id per normalized name) but repeats across that disease's
-- language rows.
--
-- Apply via Supabase Dashboard SQL Editor. Idempotent: safe to re-run.
--
-- Lookup contract (server-side):
--   SELECT data, catalog_id FROM diseases_pending
--    WHERE disease_name_normalized = $1 AND lang = $2
--      AND status IN ('pending','approved')
--    ORDER BY (status = 'approved') DESC
--    LIMIT 1
--
-- LookupAny contract (race window — find ANY language master of this disease):
--   SELECT data, catalog_id, lang FROM diseases_pending
--    WHERE disease_name_normalized = $1 AND status IN ('pending','approved')
--    ORDER BY (status = 'approved') DESC, (lang = 'en') DESC
--    LIMIT 1
--
-- Insert contract (server-side, master on miss + each translated row):
--   INSERT INTO diseases_pending
--     (disease_name_normalized, lang, catalog_id, disease_name, data,
--      status, source, source_version, generation_request_id)
--   VALUES ($1, $2,
--           COALESCE(NULLIF($3,''), 'O' || nextval('diseases_other_seq')),
--           $4, $5, 'pending', $6, NULLIF($7,''), NULLIF($8,''))
--   ON CONFLICT (disease_name_normalized, lang) DO NOTHING
--   RETURNING catalog_id;
--   (race loser: no RETURNING row → re-Lookup the winner)

ALTER TABLE diseases_pending
  ADD COLUMN IF NOT EXISTS lang TEXT NOT NULL DEFAULT 'en';

-- catalog_id is now shared across a disease's language rows, so it is no longer
-- globally unique. Drop the UNIQUE constraint if present (named by the original
-- inline UNIQUE in 002). Safe / idempotent.
ALTER TABLE diseases_pending DROP CONSTRAINT IF EXISTS diseases_pending_catalog_id_key;

-- Swap the primary key to the composite (disease_name_normalized, lang),
-- idempotently: only act if `lang` is not already part of the PK.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_index i
    JOIN pg_attribute a
      ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
    WHERE i.indrelid = 'diseases_pending'::regclass
      AND i.indisprimary
      AND a.attname = 'lang'
  ) THEN
    ALTER TABLE diseases_pending DROP CONSTRAINT IF EXISTS diseases_pending_pkey;
    ALTER TABLE diseases_pending
      ADD CONSTRAINT diseases_pending_pkey
      PRIMARY KEY (disease_name_normalized, lang);
  END IF;
END $$;

-- No extra index needed: LookupAny filters on disease_name_normalized, which the
-- composite PK (disease_name_normalized, lang) already serves via its leftmost
-- prefix (matches plant-side 003, which adds no secondary index).
