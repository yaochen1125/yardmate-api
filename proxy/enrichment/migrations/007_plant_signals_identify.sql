-- 007_plant_signals_identify.sql
-- Widen plant_signals.kind to allow 'identify' (photo-recognition interest signal)
-- alongside the original 'search' and 'garden' (006). Feeds the catalog promotion
-- tool's 📷 badge = "how many people photo-identified this out-of-catalog plant".
--
-- 006 created the table with an inline CHECK (kind IN ('search','garden')), which
-- Postgres auto-names plant_signals_kind_check. This migration drops that and adds
-- an equivalent covering all three kinds. Apply via the Supabase Dashboard SQL
-- Editor. Idempotent: safe to re-run.

ALTER TABLE plant_signals DROP CONSTRAINT IF EXISTS plant_signals_kind_check;
ALTER TABLE plant_signals ADD CONSTRAINT plant_signals_kind_check
    CHECK (kind IN ('search', 'garden', 'identify'));
