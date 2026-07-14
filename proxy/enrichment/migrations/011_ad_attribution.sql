-- 011_ad_attribution.sql
-- Apple Search Ads (AdServices) install attribution. One row per device install
-- id (first write wins — install attribution is immutable). The iOS app fetches
-- an AAAttribution token and posts it to POST /v1/attribution; the server
-- exchanges it at Apple's api-adservices endpoint (proxy/enrichment/attribution.go)
-- and stores the campaign / keyword breakdown here. Feeds ad-spend ROI analysis
-- (which Apple Search Ads campaigns / keywords actually drive installs).
--
-- attribution = false rows are confirmed-organic installs (valid token, no
-- campaign): the campaign columns stay NULL. No IDFA, no ATT prompt — AdServices
-- attribution is anonymous.
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS ad_attribution (
    device_id         text        NOT NULL PRIMARY KEY,
    user_id           text,
    attribution       boolean     NOT NULL,
    org_id            bigint,
    campaign_id       bigint,
    ad_group_id       bigint,
    ad_id             bigint,
    keyword_id        bigint,
    conversion_type   text,
    country_or_region text,
    click_date        text,
    created_at        timestamptz NOT NULL DEFAULT now()
);

-- Aggregation lookup: installs grouped by campaign / keyword for ROI reporting.
CREATE INDEX IF NOT EXISTS ad_attribution_campaign_idx
    ON ad_attribution (campaign_id, keyword_id);

-- Row-Level Security (default-deny; the server's service role bypasses RLS).
-- Same posture as plant_signals (006) / feedback (008): RLS on + NO policy so
-- the public anon / authenticated roles cannot read/write via PostgREST, while
-- the server's DB-password pool keeps writing unchanged.
ALTER TABLE ad_attribution ENABLE ROW LEVEL SECURITY;
