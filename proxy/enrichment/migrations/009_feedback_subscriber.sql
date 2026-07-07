-- 009_feedback_subscriber.sql
-- Add is_subscriber to feedback: whether the submitter is a paid subscriber
-- (coarse AppState.hasActiveSubscription flag, no identity), so triage can tell
-- paying vs free feedback apart. Anonymous design unchanged — still no user id.
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

ALTER TABLE feedback ADD COLUMN IF NOT EXISTS is_subscriber boolean NOT NULL DEFAULT false;
