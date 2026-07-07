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

-- Rate-cap lookup: count per device over the last 24 hours.
CREATE INDEX IF NOT EXISTS feedback_device_created_idx
    ON feedback (device_id, created_at);

-- Row-Level Security (default-deny; the server's DB-password pool bypasses
-- RLS). Same posture as plant_signals / the pending tables: RLS on + NO
-- policy, so the public anon / authenticated roles cannot read or write via
-- PostgREST.
ALTER TABLE feedback ENABLE ROW LEVEL SECURITY;
