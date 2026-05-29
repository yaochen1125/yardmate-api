-- 003_drop_plant_image_ingest.sql
--
-- CONTRACT step (Slice 3) of the expand/contract migration begun in
-- 002_plant_image_v2.sql. 002 CREATEd the two v2 tables (plant_image_species +
-- plant_image_files) WITHOUT dropping the v1 table, because the v1 ledger Go
-- code still read it until Slice 3 switched all callers (ingestor / handlers /
-- credits / main) onto the two-table API. This migration drops the now-orphan
-- v1 table once nothing reads it.
--
-- Apply via Supabase Dashboard SQL Editor AFTER the Slice 3 server build is
-- deployed (the running binary must no longer query plant_image_ingest).
-- Idempotent: safe to re-run.
--
-- The single manual v1 row (Monstera adansonii smoke, PR #23/#24) carries no
-- production data; its loss is benign (PIVOT memory v1_image_self_hosting).

DROP TABLE IF EXISTS plant_image_ingest;
