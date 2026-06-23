-- 005_pending_tables_rls.sql
-- Security hardening for the two V1 pending tables (plants_pending, diseases_pending).
--
-- Records two fixes applied manually in the Supabase Dashboard on 2026-06-23 after
-- Supabase Security Advisor flagged them, so a database rebuilt purely from these
-- migration files matches production.
--
-- 1) Enable Row-Level Security on both pending tables.
--    001/002 shipped with the note "No RLS in V1 (server uses the DB password DSN)".
--    That reasoning was incomplete: the server's service-role / DB-password pool
--    does bypass RLS, but Supabase still exposes every public-schema table through
--    PostgREST to the anon + authenticated roles by default. With RLS disabled,
--    anyone holding the app-embedded (public) anon key could read / write / delete
--    these tables via /rest/v1/<table>. (Advisor: rls_disabled_in_public.)
--    Enabling RLS with NO policy = default-deny for anon + authenticated, while the
--    server's connection keeps bypassing RLS unchanged. Zero application impact.
--    diseases_pending was the table actually flagged; plants_pending had been
--    enabled manually earlier (live) but 001 still said "No RLS", so it is pinned
--    here too -- otherwise a rebuild-from-migrations would leave it exposed.
--
-- 2) Pin the updated_at trigger functions' search_path (Advisor:
--    function_search_path_mutable). A mutable search_path is a privilege-escalation
--    surface; pinning it to a fixed value removes the warning and the risk.
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

-- 1) Row-Level Security (default-deny; the server's service role still bypasses RLS).
ALTER TABLE plants_pending   ENABLE ROW LEVEL SECURITY;
ALTER TABLE diseases_pending ENABLE ROW LEVEL SECURITY;

-- 2) Pin trigger-function search_path.
ALTER FUNCTION plants_pending_set_updated_at()   SET search_path = pg_catalog, public;
ALTER FUNCTION diseases_pending_set_updated_at() SET search_path = pg_catalog, public;

-- When V1.x adds an iOS admin tab (anon key + per-user JWT), replace the implicit
-- default-deny with explicit per-role policies here.
