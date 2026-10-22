# `deploy/` — yardmate-api production deployment

Self-host runbook for the App-Attest-gated secret-vending service. Server is a
single 4-vCPU / 8 GB / 160 GB box at `5.78.183.252`. systemd-managed,
Linux/amd64 binary built from this repo and shipped via scp.

> **This file covers the Go service itself.** The nginx reverse proxy in front
> of it and the healthz watchdog live in [`OPS.md`](OPS.md) + `ops-sync.sh`.
> If the app reports network errors while images still load, check
> `systemctl is-active nginx` **before** looking at yardmate-api — the service
> only listens on `127.0.0.1:8080`, so it reads `active` even when nobody can
> reach it. That exact confusion cost 22 hours on 2026-07-28.

## 0. One-time server bootstrap (admin only)

Run once when provisioning a fresh box. Subsequent deploys re-use this layout.

```bash
ssh root@5.78.183.252

# Dedicated user (no login shell, no home dir).
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin yardmate-api

# Paths — now auto-created, kept for reference: /etc/yardmate-api is ensured
# by deploy.sh / deploy-staging.sh (install -d), /var/lib/yardmate-api[-staging]
# by StateDirectory= in the systemd units at every start.
install -d -o yardmate-api -g yardmate-api -m 0750 /etc/yardmate-api
install -d -o yardmate-api -g yardmate-api -m 0750 /var/lib/yardmate-api

# systemd unit (the file from this repo is installed by deploy.sh on first run).
# Just verify the path is what we'll use:
ls -la /etc/systemd/system/yardmate-api.service 2>/dev/null || \
    echo "Will be installed by deploy.sh"
```

### Sign in with Apple `.p8` key (one-time, for `POST /v1/account/delete`)

`deploy.sh` ships the binary + `secrets.env` but **does NOT ship the Apple `.p8`
private key**. If you use `APPLE_PRIVATE_KEY_PATH` (recommended over the inline
`APPLE_PRIVATE_KEY` for a multi-line PEM), copy the `.p8` to the host once and
lock it down:

```bash
# From your Mac (the .p8 is the file you downloaded from the Apple Developer
# portal: Certificates, Identifiers & Profiles → Keys → your Sign in with Apple key).
scp AuthKey_XXXXXXXXXX.p8 root@5.78.183.252:/tmp/apple.p8.new
ssh root@5.78.183.252 'install -o yardmate-api -g yardmate-api -m 0600 \
    /tmp/apple.p8.new /etc/yardmate-api/AuthKey_XXXXXXXXXX.p8 && shred -u /tmp/apple.p8.new'
```

Then point `APPLE_PRIVATE_KEY_PATH=/etc/yardmate-api/AuthKey_XXXXXXXXXX.p8` in
your local `secrets.env.prod` and deploy. (Skip this if you instead inline the
key as a single-line `APPLE_PRIVATE_KEY` with `\n` escapes.)

## 1. `secrets.env` — format + lifecycle

Lives at `/etc/yardmate-api/secrets.env` on the server, `chmod 600`, owned by
the `yardmate-api` user. **Never committed to git.** Maintained locally by
the operator (in `~/.config/yardmate-api/secrets.env.prod` or similar — keep
it outside this repo) and pushed via `deploy.sh`.

Format (see `secrets/SPEC.md §2` + `secrets.env.example`):

```env
# Server config
ATTEST_ALLOW_DEV=false

# Vended to authenticated clients via /v1/app-secrets
OPENAI_API_KEY=sk-...
PLANT_ID_API_KEY=...
```

### Required keys (deploy.sh refuses to ship without all of these set)

- `ATTEST_ALLOW_DEV` — `true` or `false`, no implicit default. See §3.
- `OPENAI_API_KEY` — OpenAI GPT-4o-mini key for AI features.
- `PLANT_ID_API_KEY` — Plant.id API key for plant identification.
- `SUPABASE_DB_URL` — Supabase Session Pooler DSN (enrichment + account-delete row deletes).

Account-deletion endpoint (`POST /v1/account/delete` — Supabase account+data
deletion + Sign in with Apple revoke). All SERVER-ONLY (never vended/logged):

- `SUPABASE_URL` — project base URL, `https://<ref>.supabase.co` (no trailing slash).
- _(No JWT secret needed — the client access-token is verified against the project's public JWKS, ES256, at `{SUPABASE_URL}/auth/v1/.well-known/jwks.json`.)_
- `SUPABASE_SERVICE_ROLE_KEY` — service_role key (admin auth-user + Storage delete).
- `APPLE_TEAM_ID` — Apple Developer Team ID (10 chars).
- `APPLE_KEY_ID` — the Sign in with Apple .p8 key's Key ID.
- `APPLE_BUNDLE_ID` — app bundle id (== OAuth client_id).
- `APPLE_PRIVATE_KEY` **or** `APPLE_PRIVATE_KEY_PATH` — the .p8 EC private key.
  deploy.sh requires exactly one. Because `secrets.env` is line-based, an inline
  `APPLE_PRIVATE_KEY` must be ONE line with literal `\n` escapes between PEM
  lines (the server un-escapes them); for a real multi-line PEM use
  `APPLE_PRIVATE_KEY_PATH` and deploy the .p8 to the host manually (see §0).

Referral endpoints (`/v1/referral/{me,claim,redeemed}` — invite a friend,
`referral/SPEC.md`). SERVER-ONLY; when any is missing the three routes answer
`503 not_configured` (never 404) and the settlement loop does not start:

- `SUPABASE_URL` + `SUPABASE_DB_URL` — same two as above (JWT verification +
  the tables from `proxy/enrichment/migrations/013_referral.sql` and the pool
  settings / issue margin from `014_referral_pool_margin.sql`, applied
  staging → prod BEFORE the binary that uses them).
- `REVENUECAT_SECRET_KEY` — RevenueCat **v1 secret** key (`sk_…`): reads
  customers, grants promotional entitlements.
- `REVENUECAT_ENTITLEMENT_ID` — the entitlement **identifier** string exactly
  as the RevenueCat dashboard shows it. For this project that is the display
  name itself, `YardMate: AI Plant ID & Care Pro` (spaces + colon, no quotes;
  verified against a prod customer record 2026-10-19).
- `REFERRAL_SETTLE_INTERVAL` — optional, Go duration, default `10m`.

Restocking the offer-code pool: `deploy/supabase/import-codes.sh` (CSV from
App Store Connect → `code_pool`, idempotent). Smoke: `deploy/referral-smoke.sh`.
Stock check (read-only): `SUPABASE_DB_URL=… deploy/referral-pool.sh` (per-batch
counts, usable stock, earliest expiry, alert verdict). The settlement loop
watches the same numbers every pass and emails the `FEEDBACK_EMAIL_TO` inbox
(same `FEEDBACK_SMTP_*` account as `/v1/feedback`, built once in main) at most
once a day per condition set when usable stock drops below
`referral_settings.low_stock_threshold` (100) or the earliest usable batch
expires within `expiry_warn_days` (30) AND too little would be left behind it;
without the mail settings it only logs `WARN referral pool`. The same row also
holds `issue_margin_days` (14, new codes must outlive it) and `swap_grace_days`
(3, a re-claim swaps the code in hand only below it). Apple batches hold
500–25,000 codes and live at most six months (1,000,000 codes / quarter / app,
shared by all subscriptions — checked 2026-10-22): import a new batch about
every five months, or turn on auto-restock (§10).

Adding a new key:

1. Add it to your local `secrets.env.prod`.
2. Add the env name (UPPER_SNAKE) to `vendedKeys` in `handlers.go` if it should reach the iOS client.
3. Run `./deploy/deploy.sh`. Restart picks up the new value.

## 2. Deploy

From a clean checkout of this repo on macOS, with your local secrets file at
`~/.config/yardmate-api/secrets.env.prod`:

```bash
YARDMATE_SECRETS=~/.config/yardmate-api/secrets.env.prod ./deploy/deploy.sh
```

What `deploy.sh` does, in order:

1. **Pre-flight checks (local):**
   - `go test ./...` — refuses to ship a failing build.
   - Validate `$YARDMATE_SECRETS` exists, is `0600`, contains the three required keys + `ATTEST_ALLOW_DEV` explicitly.
2. **Build:** `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/yardmate-api-linux-amd64 .`
3. **Ship binary:** `scp bin/yardmate-api-linux-amd64 root@host:/tmp/yardmate-api.new`
4. **Ship secrets:** `scp "$YARDMATE_SECRETS" root@host:/tmp/secrets.env.new`
5. **Ship unit file:** `scp deploy/yardmate-api.service root@host:/etc/systemd/system/yardmate-api.service`
6. **Install + restart (over ssh):**
   ```bash
   install -o yardmate-api -g yardmate-api -m 0755 /tmp/yardmate-api.new /usr/local/bin/yardmate-api
   install -o yardmate-api -g yardmate-api -m 0600 /tmp/secrets.env.new /etc/yardmate-api/secrets.env
   shred -u /tmp/yardmate-api.new /tmp/secrets.env.new
   systemctl daemon-reload
   systemctl enable --now yardmate-api
   systemctl restart yardmate-api
   ```
7. **Verify:** `curl -sf http://127.0.0.1:8080/healthz` over ssh → must return `{"status":"ok"}` within 5 s.
8. **Print effective config loudly:** reads `grep ATTEST_ALLOW_DEV /etc/yardmate-api/secrets.env` on the host and prints it in big letters before declaring success. The operator's eyeball check is the final gate.

## 3. `ATTEST_ALLOW_DEV` — stage-aware ship SOP

Recap of `attest/SPEC §待确认`:

| Stage | `ATTEST_ALLOW_DEV` | When |
|---|---|---|
| Local dev (xcodebuild on a paired iPhone) | `true` | Active D-Client development |
| Internal TestFlight with dev-signed builds | `true` | Pre-App-Store testing |
| App Store production | **`false`** | Once a binary is in App Store Connect, forever |

**The flip from `true` → `false` happens BEFORE the App Store binary is pushed to App Store Connect**, in this exact order:

1. Edit local `secrets.env.prod`: `ATTEST_ALLOW_DEV=false`.
2. `./deploy/deploy.sh` (this also runs the §2 step-8 eyeball check — read it).
3. On a test iPhone with a dev-signed YardMate build, hit `/v1/app-secrets`. **Expect 401** (`{"error":"aaguid_mismatch"}`). If you get 200, **STOP** — flag is still effectively `true` somewhere; debug then re-run from step 1.
4. Only after the dev-build smoke test returns 401: push the production binary to App Store Connect.

Why this matters: production attestation is Apple's cryptographic guarantee
that the App-Attest token came from an App-Store-signed YardMate binary on
real Apple hardware. Leaving `ATTEST_ALLOW_DEV=true` after launch lets anyone
holding *any* Apple-issued dev signing cert tied to this Bundle ID build a
"fake YardMate", produce a development-environment attestation, and drain
the OpenAI / Plant.id quota.

`deploy.sh` enforces defense-in-depth:

- Refuses to deploy if `ATTEST_ALLOW_DEV` is absent from the env file (no implicit defaults at deploy time).
- Refuses to deploy if `ATTEST_ALLOW_DEV=true` and `$YARDMATE_DEPLOY_STAGE=prod` (the operator must `YARDMATE_DEPLOY_STAGE=dev` explicitly to ship a dev-allowing binary).
- Prints the effective value loudly at the end of every deploy.

## 4. Rotation

Rotating an API key (`OPENAI_API_KEY`, `PLANT_ID_API_KEY`, etc.):

1. Generate the new key at the provider's dashboard.
2. Edit local `secrets.env.prod` — replace the old value with the new.
3. `./deploy/deploy.sh` — restart picks up the new value (the in-memory Vault
   is built from the file content at process start; `systemctl restart` is
   how rotation takes effect).
4. Wait ~30 s; check `journalctl -u yardmate-api -n 50` for healthy startup.
5. Revoke the old key at the provider (last step — only after the new key is confirmed working).

iOS clients re-fetch via `/v1/app-secrets` opportunistically (cached in memory). During the brief window where some clients have the old key and the server has only the new one, those clients see 401/403 from OpenAI/Plant.id and re-fetch. Within minutes the whole fleet is on the new key.

## 5. Rollback

If a deploy regresses behavior, roll the binary back:

```bash
ssh root@5.78.183.252
# The previous binary is saved by deploy.sh as /usr/local/bin/yardmate-api.prev
mv /usr/local/bin/yardmate-api.prev /usr/local/bin/yardmate-api
systemctl restart yardmate-api
journalctl -u yardmate-api -n 50
```

For a code change that needs a longer fix: revert the offending PR on `main`,
re-deploy. Don't hot-patch the server binary.

## 6. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `journalctl` shows `load secrets: open /etc/yardmate-api/secrets.env: permission denied` | File owner ≠ `yardmate-api` OR mode ≠ 0600 | Re-run deploy (it reinstalls with correct perms) |
| `open store: open /var/lib/yardmate-api/credentials.db: permission denied` | `/var/lib/yardmate-api` owner ≠ `yardmate-api` | `chown -R yardmate-api:yardmate-api /var/lib/yardmate-api` |
| `aaguid_mismatch` from a known-good production iOS build | `ATTEST_ALLOW_DEV` flipped wrong-way OR Team/Bundle ID drift | Check secrets.env on host + `YARDMATE_API_APP_ID` env in unit file |
| Client gets 429 immediately | per-IP limit too tight for shared NAT egress | Raise `YARDMATE_API_RL_IP_LIMIT` (env in systemd unit) |
| `WARN plantnet quota LOW` in journal / daily "PlantNet quota low" email | Pl@ntNet's daily identify quota is nearly exhausted (falls back to Plant.id credits then GPT-4o vision at 0) | Upgrade the Pl@ntNet plan; tune the alert point via `YARDMATE_API_PLANTNET_QUOTA_WARN` (default 50, env in systemd unit) |
| BoltDB grows past expected size | challenge sweeper not running | Restart service; bbolt compacts on open (rare path) |
| `POST /v1/plants/catalog-images` returns `400 unknown_catalog_id` for a real curated plant | embedded `proxy/data/plants_index.json` is stale (a newly-added species is absent from the binary's copy) | Re-sync the copy from `yardmate-swiftui` and rebuild/redeploy (§7) |

## 7. Embedded catalog data (`proxy/data/`) — cross-repo sync

The binary embeds the plant/disease catalog at **build time** (`proxy/content.go`,
`//go:embed data/*.json`). The source of truth lives in the **sibling repo**
`yardmate-swiftui`; the files here are copies that must be re-synced when the
catalog changes:

| Embedded copy (this repo) | Source of truth (`yardmate-swiftui`) |
|---|---|
| `proxy/data/plants_index.json` | `scripts/plants_index.json` |
| `proxy/data/plants_detail.json` | (same `scripts/` pipeline) |
| `proxy/data/diseases.json` | (same `scripts/` pipeline) |

**When the catalog changes (species added / renamed / removed), re-sync before deploy:**

```bash
cp ../yardmate-swiftui/scripts/plants_index.json proxy/data/plants_index.json
# (+ plants_detail.json / diseases.json if they changed too)
go build ./... && go test ./...   # embed is compile-time — only a rebuild picks it up
./deploy/deploy.sh
```

Why this matters beyond identify/diagnose: `proxy/data/plants_index.json` is also
the **authoritative `catalog_id → scientific_name` map** for the
`POST /v1/plants/catalog-images` supplementary-image endpoint (imageingest
SPEC §2.8, Codex P1 fix). The handler derives the cascade search name from
`catalog_id` via this embed and **rejects ids not present** with
`400 unknown_catalog_id`. So a stale copy means a newly-added curated plant's
supplementary gallery 400s until the copy is re-synced and the binary rebuilt.
There is exactly **one** in-repo copy (the fix deliberately reuses this embed
rather than adding a second) — keep it in lockstep with the swiftui source.

## 8. Referral rewards — RevenueCat webhook + gift-card payouts

`rewards/SPEC.md`. A friend's real charge (RevenueCat `INITIAL_PURCHASE` /
`RENEWAL`, `NORMAL` / `INTRO`, price > 0, `PRODUCTION`) credits the referrer
30% of Apple's payout; it clears after 90 days; $100 of cleared balance buys
one Amazon.com gift card, **sent by hand**. Everything below is staging first,
then prod.

**Turning it on**

1. Apply `proxy/enrichment/migrations/015_referral_rewards.sql` (after 013 and 014)
   **before** the binary that uses it. Re-runnable.
2. Generate a secret (`openssl rand -hex 32`), add
   `REVENUECAT_WEBHOOK_SECRET=<it>` to the secrets file, deploy. Unset → the
   webhook answers `503` and nothing is credited (the rest of the app is
   unaffected).
3. RevenueCat dashboard → Project → Integrations → Webhooks → add one:
   URL `https://<api host>/v1/revenuecat/webhook`, *Authorization header
   value* = the same secret (with or without `Bearer ` — either side may carry
   it), all events. Sandbox events are accepted and ignored (`200
   ignored_environment`), so one webhook for both environments is fine.
4. "Send test event" in that screen → `200 {"status":"ignored_environment"}`
   (or `ignored_type`). `401` = secret mismatch; `503` = secret or database not
   configured (see `journalctl -u yardmate-api | grep "revenuecat webhook"`).
   A `500` is ours (database) and RevenueCat retries it with backoff.
5. nginx: `location /v1/` already forwards it; nothing to change.

**A gift-card request arrives** (mail `[YardMate] Referral gift card request
#17` to `FEEDBACK_EMAIL_TO`, same account as `/v1/feedback`):

1. If the mail says `W-9 FIRST`, the user is at $500+ in cards this calendar
   year: collect a W-9 before sending (1099-NEC from $600).
2. Within 3 business days buy a $100 Amazon.com eGift card and deliver it to
   the address in the mail.
3. `SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… deploy/referral-payout.sh mark-sent 17`
   — the app shows `Sent {date}` on its next `/me`.
4. Not sending after all (bad address, no W-9): `… deploy/referral-payout.sh cancel 17`
   — the $100 is back in their balance by definition (balance = cleared
   credits − requested − sent), no ledger edit.
5. Missed a mail / mail not configured: `… deploy/referral-payout.sh list`
   shows every open request (`list sent` / `list cancelled` for history).

## 9. Referral anti-abuse (防刷) — DeviceCheck, devices, one Apple ID once, velocity

`referral/SPEC.md` §2.3–§2.4; product rules in yardmate-swiftui
`invite-friends.md ### 防刷`. Gate 1 is Apple DeviceCheck (one device is a
referred friend once in its life); gate 2 refuses a friend claiming on a device
the referrer has used (`own_code`); gate 3 voids a second referral whose friend
subscription is the same Apple ID chain (`duplicate_apple_id`); gate 4 parks
the 6th+ success of one referrer within 24 h as `held` and mails the operator
(counted on theoretical success time — redeemed + `hold_days`, migration 017 —
so a backlog judged in one pass after a settlement outage is not held
wholesale). An approved held referral has the friend's refund checked again
before it pays.

**Rollout order** (nothing here touches prod until step 6):

1. **Migrations 016 then 017 on staging**: `psql "$STAGING_SUPABASE_DB_URL" -f
   proxy/enrichment/migrations/016_referral_antiabuse.sql`, then the same with
   `017_referral_review_fixes.sql` (after 013 → 014 → 015; both re-runnable —
   but re-running 016 after 017 puts 016's `referral_claim` / `referral_qualify`
   back, so always follow a 016 re-run with 017). Both must land **before** the
   binary: the new binary calls 016's functions (`referral_claim(text,text,text)`,
   `referral_me(text,text)`, `referral_mark_redeemed`, `referral_qualify`, …)
   and reads / writes 017's `referrals.last_checked_at` (without 017,
   settlement ① / ④ fail every pass; `/me` and `/claim` still work). Older
   binaries keep working on a 017 database (017 only replaces two functions
   with the same signatures; the two-argument ones are untouched), so a binary
   rollback needs no database rollback.
2. **Staging secrets** (`secrets-staging.env`): add `DEVICECHECK_KEY_ID`
   (`W3YN83SB24`), `DEVICECHECK_PRIVATE_KEY` (the .p8 on one line with literal
   `\n`, exactly like `APPLE_PRIVATE_KEY`; or `DEVICECHECK_PRIVATE_KEY_PATH`),
   **`DEVICECHECK_MODE=log`** and **`DEVICECHECK_ALLOW_DEVELOPMENT=true`**
   (staging only: lets enforce take a DEBUG build's `development` token when
   you switch to enforce in step 4). `APPLE_TEAM_ID` is already there. Unset
   mode means `enforce`; unset allow-development means `false`.
3. **Deploy staging** (`deploy/deploy-staging.sh`). The log must show
   `referral devicecheck ready: mode=log`; a `WARN: referral devicecheck not
   configured (missing [...])` line names what is missing (never the key).
4. **Watch staging in log mode**: claims are never refused, but every verdict
   is logged — `journalctl -u yardmate-api-staging | grep devicecheck`
   (`would be device_used`, `would be device_unverified`, `apple query failed`,
   `set bit0 failed`). `deploy/referral-smoke.sh` passes only in log mode (it
   has no real-device token). To test gate 1 itself: set
   `DEVICECHECK_MODE=enforce` on staging, redeploy, run the real-device
   acceptance list (invite-friends.md ## 验收 · 防刷). With
   `DEVICECHECK_ALLOW_DEVELOPMENT=true` a DEBUG build from Xcode works
   (`device_check_env = development` → Apple's development host, whose bits
   are separate from production's) as well as a TestFlight build; without it
   enforce only accepts `production`. The simulator or a request without the
   field gets `400 device_unverified`. The startup log has a `WARN:
   DEVICECHECK_ALLOW_DEVELOPMENT=true …` line while it is on. Then back to
   `log` (which asks whichever environment the request names).
   Gates 2–4 work the same in both modes; tune the velocity gate in the
   database (`update referral_settings set velocity_limit = …,
   velocity_window_hours = … where id = 1` — `0` turns it off).
5. **Migrations 016 then 017 on prod**, same commands against the prod
   database.
6. **Prod secrets** (`secrets.env`): the same two DeviceCheck values (already in
   the local `~/.config/yardmate-api/secrets.env.prod`) and
   **`DEVICECHECK_MODE=enforce`** — and **no** `DEVICECHECK_ALLOW_DEVELOPMENT`
   (on prod it would give every device a second life) — then
   `deploy/deploy.sh`. Check for `referral devicecheck ready: mode=enforce` and
   that there is no `DEVICECHECK_ALLOW_DEVELOPMENT` WARN line. In enforce, a missing or broken
   key makes every **new** claim `503 device_check_unavailable` (re-claims,
   `/me`, `/redeemed` keep working), so look at that line before turning the
   `referralEnabled` flag on. Only Release builds (TestFlight / App Store,
   `device_check_env = production`) can claim in enforce.

**A held mail arrives** (`[YardMate] Referral review: N held for one
referrer`, at most one per referrer per 24 h):

1. Read it: how many friends, how many distinct claim devices (`有重复` =
   several friends on one phone), claim times bunched together.
2. `SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… deploy/referral-review.sh list`
   shows every held referral.
3. Fine: `… deploy/referral-review.sh approve <referral_id>` — the next
   settlement pass (≤ 10 min) first re-checks the friend's subscription (a
   refund while it was held voids it as `refunded`), then grants or saves the
   month as usual, and the app's arrival notification fires on its next `/me`.
4. Abuse: `… deploy/referral-review.sh reject <referral_id>` — void
   (`rejected`), no month, and its gift-card money (pending + available) is
   voided with it. Also the tool for a suspicious gift-card request (§8):
   cancel the request, then reject the referrals behind it.

The referrer never sees "under review": a held referral counts in
`months_pending` ("on the way").

## 10. Referral code pool auto-restock (自动补货, default off)

`referral/SPEC.md` §3.1; product rules in yardmate-swiftui `invite-friends.md
### 码池自动补货`. When `referral_settings.auto_restock_enabled` is on, every
settlement pass (10 min) generates a batch of one-time offer codes through the
App Store Connect API and imports it into `code_pool` when usable stock is
below `restock_below` (300), or the earliest usable batch expires within
`restock_lead_days` (45) with too little behind it. At most one restock in
flight, an hour apart, `restock_daily_max` (2) per UTC day; every failure and
every success mails the `FEEDBACK_EMAIL_TO` inbox, subject tagged with the code
environment (`[SANDBOX]` / `[PRODUCTION]`). With the switch off nothing calls
Apple and `import-codes.sh` works exactly as before (it also works with the
switch on — the two kinds of codes are the same in the pool).

**Turning it on** (staging first, SANDBOX codes; then prod, PRODUCTION codes):

1. **Migration 021**: `psql "$STAGING_SUPABASE_DB_URL" -f
   proxy/enrichment/migrations/021_referral_auto_restock.sql` (after 013 / 014;
   re-runnable), **before** the binary. Without it the new binary treats
   auto-restock as off and logs `referral restock: off (migration
   021_referral_auto_restock.sql not applied)` once.
2. **Secrets** (`secrets-staging.env`, later `secrets.env`) — five keys, values
   never in the repo or a chat:
   - `ASC_KEY_ID` — the App Store Connect API key's Key ID (the App Manager key
     already used for uploads, decided 2026-10-22).
   - `ASC_ISSUER_ID` — App Store Connect → Users and Access → Integrations →
     Issuer ID.
   - `ASC_PRIVATE_KEY` — that key's `.p8` on ONE line with literal `\n` between
     the PEM lines (like `APPLE_PRIVATE_KEY`).
   - `ASC_REFERRAL_OFFER_ID` — the `Referral Friend Month` offer's id
     (`bbce4503-29ee-4629-9a90-f55d8c2a6327` as of 2026-10-22).
   - `ASC_OFFER_CODE_ENVIRONMENT` — `SANDBOX` on staging, `PRODUCTION` on prod.
   Deploy. The log shows `referral restock: App Store Connect configured, code
   environment SANDBOX …`, or `… not configured (missing [...])` naming what
   is missing (never a value).
3. **The database's own code environment** (each database once; must equal
   `ASC_OFFER_CODE_ENVIRONMENT`, or every pass refuses and mails once a day):
   `update referral_settings set restock_environment = 'SANDBOX' where id = 1;`
   (`'PRODUCTION'` on prod).
4. **Staging numbers** (approved 2026-10-22: one 10-code SANDBOX batch):
   `update referral_settings set restock_batch_size = 10, restock_below = 5,
   low_stock_threshold = 4 where id = 1;` (`low_stock_threshold` under
   `restock_below`, or the low-stock alert fires before the restock does; the
   settings check logs a WARN for that). Prod keeps the defaults (1,000 / 300 /
   100); a PRODUCTION batch is clamped to Apple's 500–25,000.
5. **Switch on**: `update referral_settings set auto_restock_enabled = true
   where id = 1;` — takes effect on the next pass, no deploy.

**Pausing**: `restock_daily_max = 0` pauses auto-restock — nothing new is
started and there is no "daily max" mail (a restock already under way is still
finished; the low-stock alert works as usual). Switching
`auto_restock_enabled` off stops everything, including an unfinished restock:
it is left as it is (no Apple calls, no mails) and the log says so once a day
(`WARN referral restock: auto_restock_enabled is off but restock N is still
created …`) — switch back on to let it finish, or import its batch by hand.

Once `ASC_OFFER_CODE_ENVIRONMENT` is set, the old low-stock / expiring alert
also carries it in the subject (`[YardMate] [SANDBOX] Referral code pool: …`),
so staging and prod mailing the same inbox can be told apart.

**Checking it on staging** (this generates real sandbox codes on Apple's side —
only with the batch size above):

- Usable sandbox stock must be under `restock_below` for it to fire:
  `SUPABASE_DB_URL=… deploy/referral-pool.sh` shows usable, the restock
  settings and the last restocks. If staging still has ≥ 5 usable codes, either
  wait, or raise the trigger for one pass (`update referral_settings set
  restock_below = <usable + 1> where id = 1;`, back to 5 afterwards — with
  `restock_batch_size = 10` that is still one 10-code batch).
- Nudge a pass instead of waiting up to 10 minutes: restart the service
  (`sudo systemctl restart yardmate-api-staging`; the first pass runs a minute
  after start); an authenticated `POST /v1/referral/redeemed` also nudges one.
- Watch: `journalctl -u yardmate-api-staging -f | grep 'referral restock'` —
  `restock N started`, `restock N created: App Store Connect batch …`,
  `restock N imported: batch …, 10 values, 10 new in the pool …`; the inbox
  gets `[YardMate] [SANDBOX] Referral code restock: added 10 codes, expires …`.
  `deploy/referral-pool.sh` lists the new `Referral Friend Month` batch and the
  `code_restocks` row as `imported`.
- Then set the switch back off on staging unless you want it running there
  (`auto_restock_enabled = false`).

**When a mail arrives**: `not configured` / `code environment mismatch` — fix
the secrets or the row; `Apple refused the batch (http_401|http_403)` — the key
is revoked or lacks the role; `(http_4xx)` — Apple refused (e.g. the quarter's
quota); `outcome unknown, no matching batch` — check the offer's one-time code
batches in App Store Connect and, if a batch with that count / expiry exists,
download its CSV and import it with `deploy/supabase/import-codes.sh` (safe to
repeat); `batch <id> not imported in 6h` — same, for that batch id (the 6-hour
pass makes one last try first); `batch <id> code values not recognised` — the
CSV has enough lines but too few look like codes (Apple changed the format?):
download that batch's CSV, check it, import by hand if it reads; the backend
keeps trying until the 6 hours are up; `daily max reached` — restock by hand or
raise `restock_daily_max`. A refused or failed
restock is retried after an hour within the daily max.

## 11. References

- `attest/SPEC.md` — App Attest protocol implementation
- `secrets/SPEC.md` — env file format + load semantics
- `ratelimit/SPEC.md` — bucket policy + post-verify rationale
- `deploy/yardmate-api.service` — the unit file shipped to the host
- `deploy/secrets.env.example` — template for the local secrets file
- `deploy/deploy.sh` — the script
- `rewards/SPEC.md` + `deploy/referral-payout.sh` — referral gift-card rewards (§8)
- `referral/SPEC.md` + `deploy/referral-review.sh` — referral anti-abuse, held review (§9)
- `referral/SPEC.md` §3.1 + `deploy/referral-pool.sh` — referral code pool auto-restock (§10)
