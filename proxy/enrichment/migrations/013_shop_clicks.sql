-- 013_shop_clicks.sql
-- Shop (Amazon affiliate storefront tab) product-card click log, feeding the
-- 7788 admin tool's「商店数据」page: top clicked products, by category /
-- collection, by screen, by locale, daily series.
--
-- One row per click (NOT deduped per device — the admin page derives both
-- click count and distinct-device count from the rows). The server writes via
-- POST /v1/shop/click (proxy/enrichment/shop_click.go) and enforces a
-- per-device daily cap (300) in the insert statement, so no constraint is
-- needed here. Anonymous by design: the only identifier is the device install
-- id; never a user id. iOS skips the report entirely in EEA/UK (PrivacyRegion).
--
-- Apply via the Supabase Dashboard SQL Editor (V1). Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS shop_clicks (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at  timestamptz NOT NULL DEFAULT now(),
    device_id   text        NOT NULL,
    app_version text        NOT NULL DEFAULT '',
    item_id     text        NOT NULL CHECK (char_length(item_id) BETWEEN 1 AND 64),
    screen      text        NOT NULL CHECK (screen IN ('shop', 'shop_category', 'shop_collection')),
    context_id  text        NOT NULL DEFAULT '' CHECK (char_length(context_id) <= 64),
    locale      text        NOT NULL DEFAULT '' CHECK (char_length(locale) <= 16)
);

-- Admin page: window scans (created_at >= now() - N days), then aggregate.
CREATE INDEX IF NOT EXISTS shop_clicks_created_idx
    ON shop_clicks (created_at);

-- Rate-cap lookup: count per device over the last 24 hours.
CREATE INDEX IF NOT EXISTS shop_clicks_device_created_idx
    ON shop_clicks (device_id, created_at);

-- Row-Level Security (default-deny; the server's DB-password pool bypasses
-- RLS). Same posture as plant_signals / catalog_signals / feedback: RLS on +
-- NO policy, so the public anon / authenticated roles cannot read or write via
-- PostgREST. The 7788 admin tool reads with the service-role key (bypasses RLS).
ALTER TABLE shop_clicks ENABLE ROW LEVEL SECURITY;
