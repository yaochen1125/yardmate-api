-- 008_feedback.sql
-- In-app "Send feedback" messages (iOS: More → SUPPORT → Send feedback).
--
-- One row per submitted message. The server writes via POST /v1/feedback
-- (proxy/enrichment/feedback.go); a per-device daily cap is enforced in the
-- insert statement itself (CTE count over the last 24h), so no extra
-- constraint is needed here. Anonymous by design: the only identifier is the
-- device install id (rate-cap key), never a user id or email.
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS feedback (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    device_id    text        NOT NULL,
    app_version  text        NOT NULL,
    message      text        NOT NULL CHECK (char_length(message) BETWEEN 1 AND 1000),
    device       text        NOT NULL DEFAULT '',
    system       text        NOT NULL DEFAULT '',
    app_language text        NOT NULL DEFAULT '',
    region       text        NOT NULL DEFAULT ''
);

-- Legacy reconciliation: both Supabase projects predate this migration with an
-- EMPTY early-design table feedback(id, user_id, device_id, message,
-- created_at), which makes the CREATE above a no-op. Add the missing columns
-- instead of dropping it; the unused user_id column stays (harmless, and this
-- feature is anonymous by design so the server never writes it).
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS app_version  text NOT NULL DEFAULT '';
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS device       text NOT NULL DEFAULT '';
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS system       text NOT NULL DEFAULT '';
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS app_language text NOT NULL DEFAULT '';
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS region       text NOT NULL DEFAULT '';
ALTER TABLE feedback ALTER COLUMN device_id SET NOT NULL;

-- Message length cap for the legacy-table path (the inline CHECK above only
-- applies when this migration created the table fresh).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'feedback_message_len') THEN
        ALTER TABLE feedback ADD CONSTRAINT feedback_message_len
            CHECK (char_length(message) BETWEEN 1 AND 1000);
    END IF;
END $$;

-- The legacy table carried a pre-V1 "Users can insert own feedback" policy
-- (authenticated PostgREST inserts). That violates this table's default-deny
-- posture — all writes go through the server's DB-password pool, which
-- bypasses RLS — and would let clients skip the per-device daily cap. Same
-- lockdown treatment as the other legacy tables (RLS 遗留表锁定).
DROP POLICY IF EXISTS "Users can insert own feedback" ON feedback;

-- Rate-cap lookup: count per device over the last 24 hours.
CREATE INDEX IF NOT EXISTS feedback_device_created_idx
    ON feedback (device_id, created_at);

-- Row-Level Security (default-deny; the server's DB-password pool bypasses
-- RLS). Same posture as plant_signals / the pending tables: RLS on + NO
-- policy, so the public anon / authenticated roles cannot read or write via
-- PostgREST.
ALTER TABLE feedback ENABLE ROW LEVEL SECURITY;
