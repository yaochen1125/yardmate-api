#!/usr/bin/env bash
# deploy.sh — build the linux/amd64 binary, ship binary + secrets + systemd
# unit to the production host, restart the service, and verify health.
#
# Usage:
#   YARDMATE_SECRETS=~/.config/yardmate-api/secrets.env.prod ./deploy/deploy.sh
#
# Optional env:
#   YARDMATE_DEPLOY_HOST   default 5.78.183.252
#   YARDMATE_DEPLOY_USER   default root
#   YARDMATE_DEPLOY_STAGE  default prod ; set to "dev" to allow ATTEST_ALLOW_DEV=true
#
# See deploy/README.md for the full runbook.

set -euo pipefail

# --- config ---
HOST="${YARDMATE_DEPLOY_HOST:-5.78.183.252}"
USER="${YARDMATE_DEPLOY_USER:-root}"
STAGE="${YARDMATE_DEPLOY_STAGE:-prod}"
SECRETS="${YARDMATE_SECRETS:-}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_OUT="${REPO_ROOT}/bin/yardmate-api-linux-amd64"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
banner() { printf '\033[1;7m %s \033[0m\n' "$*"; }

die() { red "FATAL: $*" >&2; exit 1; }

# --- 1. pre-flight: secrets file ---
[[ -n "$SECRETS" ]] || die "YARDMATE_SECRETS env not set. See deploy/README.md §2."
[[ -f "$SECRETS" ]] || die "secrets file not found: $SECRETS"

# macOS stat vs Linux stat differ — try both.
mode=$(stat -f '%Lp' "$SECRETS" 2>/dev/null || stat -c '%a' "$SECRETS" 2>/dev/null || echo "")
if [[ "$mode" != "600" ]]; then
    red "WARNING: $SECRETS mode is $mode (want 600). Running 'chmod 600' on it locally."
    chmod 600 "$SECRETS"
fi

# Required keys must all be present and non-empty.
#
# SUPABASE_URL / SUPABASE_SERVICE_ROLE_KEY + APPLE_TEAM_ID / APPLE_KEY_ID /
# APPLE_BUNDLE_ID back POST /v1/account/delete (Supabase account+data deletion +
# Sign in with Apple token revoke). The access-token is verified against the
# project's public JWKS (ES256), so no SUPABASE_JWT_SECRET is needed. The Apple
# private key is checked separately below (PEM OR path).
for key in ATTEST_ALLOW_DEV OPENAI_API_KEY PLANT_ID_API_KEY SUPABASE_DB_URL \
           SUPABASE_URL SUPABASE_SERVICE_ROLE_KEY \
           APPLE_TEAM_ID APPLE_KEY_ID APPLE_BUNDLE_ID; do
    # `|| true` so a MISSING key (grep exit 1 under set -e + pipefail) flows to
    # the die below with an accurate message, instead of the whole script
    # aborting silently on the assignment.
    val=$(grep -E "^${key}=" "$SECRETS" | head -1 | cut -d= -f2- || true)
    [[ -n "$val" ]] || die "missing or empty key '$key' in $SECRETS"
done

# PLANTNET_API_KEY is optional — identify degrades to Plant.id-only when absent
# (main.go WARN-logs the downgrade). Not a hard requirement, but a missing key on
# a fresh secrets file silently changes identify quality + Plant.id billing, so
# warn loudly here rather than leaving it to be discovered in journalctl.
plantnet_key=$(grep -E '^PLANTNET_API_KEY=' "$SECRETS" | head -1 | cut -d= -f2- || true)
[[ -n "$plantnet_key" ]] || yellow "WARNING: PLANTNET_API_KEY absent — identify will run Plant.id-only (primary engine disabled). Intentional? See main.go engine selection."

# Apple private key: exactly one of APPLE_PRIVATE_KEY (the .p8 PEM inline) or
# APPLE_PRIVATE_KEY_PATH (path to the .p8 on the server) must be set. Both empty
# means the Sign in with Apple revoke step can't run (App Store deletion
# requirement), so refuse to ship.
apple_pem=$(grep -E '^APPLE_PRIVATE_KEY=' "$SECRETS" | head -1 | cut -d= -f2- || true)
apple_pem_path=$(grep -E '^APPLE_PRIVATE_KEY_PATH=' "$SECRETS" | head -1 | cut -d= -f2- || true)
if [[ -z "$apple_pem" && -z "$apple_pem_path" ]]; then
    die "set APPLE_PRIVATE_KEY (.p8 PEM contents) OR APPLE_PRIVATE_KEY_PATH (path to .p8) in $SECRETS — Sign in with Apple revoke (POST /v1/account/delete) needs the signing key."
fi

allow_dev=$(grep -E '^ATTEST_ALLOW_DEV=' "$SECRETS" | head -1 | cut -d= -f2-)
if [[ "$allow_dev" == "true" && "$STAGE" == "prod" ]]; then
    die "ATTEST_ALLOW_DEV=true is forbidden when YARDMATE_DEPLOY_STAGE=prod.
        Either fix the env file (set to false) or set YARDMATE_DEPLOY_STAGE=dev
        if you really mean it. See attest/SPEC §待确认 and deploy/README.md §3."
fi

# --- 2. pre-flight: tests pass ---
yellow ">> running go test ./..."
(cd "$REPO_ROOT" && go test ./... -count=1) || die "tests fail; refusing to deploy"

# --- 3. build linux binary ---
yellow ">> building linux/amd64 binary -> $BIN_OUT"
mkdir -p "$(dirname "$BIN_OUT")"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN_OUT" .)
[[ -f "$BIN_OUT" ]] || die "build produced no output"

# --- 4. ship + install ---
yellow ">> uploading to $USER@$HOST"
scp "$BIN_OUT" "$USER@$HOST:/tmp/yardmate-api.new"
scp "$SECRETS" "$USER@$HOST:/tmp/secrets.env.new"
scp "$REPO_ROOT/deploy/yardmate-api.service" "$USER@$HOST:/tmp/yardmate-api.service.new"

yellow ">> installing on $HOST"
ssh "$USER@$HOST" bash -se <<'REMOTE'
set -euo pipefail

# Clean up staged /tmp files even if an install step fails mid-script, so plain
# secrets never linger on the shared /tmp past this ssh session.
trap 'shred -u /tmp/secrets.env.new 2>/dev/null || true; rm -f /tmp/yardmate-api.new /tmp/yardmate-api.service.new' EXIT

# Save previous binary + secrets + unit for one-step rollback: if the failure
# root cause is the new secrets or unit (not the binary), a binary-only .prev
# can't recover.
if [[ -f /usr/local/bin/yardmate-api ]]; then
    cp /usr/local/bin/yardmate-api /usr/local/bin/yardmate-api.prev
fi
if [[ -f /etc/yardmate-api/secrets.env ]]; then
    cp -p /etc/yardmate-api/secrets.env /etc/yardmate-api/secrets.env.prev
fi
if [[ -f /etc/systemd/system/yardmate-api.service ]]; then
    cp /etc/systemd/system/yardmate-api.service /etc/systemd/system/yardmate-api.service.prev
fi

install -o yardmate-api -g yardmate-api -m 0755 /tmp/yardmate-api.new /usr/local/bin/yardmate-api
install -d -o yardmate-api -g yardmate-api -m 0750 /etc/yardmate-api
install -o yardmate-api -g yardmate-api -m 0600 /tmp/secrets.env.new /etc/yardmate-api/secrets.env
install -o root -g root -m 0644 /tmp/yardmate-api.service.new /etc/systemd/system/yardmate-api.service

systemctl daemon-reload
systemctl enable yardmate-api
systemctl restart yardmate-api
REMOTE

# --- 5. health check (auto-rollback on failure) ---
# 30 s window: startup does content-index load + Supabase ping; 10 s was tight
# enough to false-negative a slow-but-healthy boot and leave the new binary in
# service while printing "aborted".
yellow ">> waiting up to 30 s for /healthz"
healthy=0
for _ in $(seq 1 30); do
    if ssh "$USER@$HOST" "curl -sf http://127.0.0.1:8080/healthz" 2>/dev/null | grep -q '"ok"'; then
        green "/healthz OK"
        healthy=1
        break
    fi
    sleep 1
done

if [[ "$healthy" -ne 1 ]]; then
    red "/healthz never became healthy. Logs:"
    ssh "$USER@$HOST" "journalctl -u yardmate-api -n 50 --no-pager" >&2 || true
    yellow ">> auto-rolling back to previous binary + unit + secrets"
    ssh "$USER@$HOST" bash -se <<'ROLLBACK' || true
set -uo pipefail
[[ -f /usr/local/bin/yardmate-api.prev ]] && cp /usr/local/bin/yardmate-api.prev /usr/local/bin/yardmate-api
[[ -f /etc/systemd/system/yardmate-api.service.prev ]] && cp /etc/systemd/system/yardmate-api.service.prev /etc/systemd/system/yardmate-api.service
if [[ -f /etc/yardmate-api/secrets.env.prev ]]; then
    cp -p /etc/yardmate-api/secrets.env.prev /etc/yardmate-api/secrets.env
fi
systemctl daemon-reload
systemctl restart yardmate-api
ROLLBACK
    sleep 2
    if ssh "$USER@$HOST" "curl -sf http://127.0.0.1:8080/healthz" 2>/dev/null | grep -q '"ok"'; then
        die "deploy FAILED — auto-rolled back to previous version; service healthy on old binary."
    fi
    die "deploy FAILED and rollback health-check also failed — MANUAL INTERVENTION NEEDED. Check journalctl on $HOST."
fi

# --- 6. effective config eyeball check ---
banner "EFFECTIVE CONFIG ON $HOST"
ssh "$USER@$HOST" "grep -E '^(ATTEST_ALLOW_DEV)=' /etc/yardmate-api/secrets.env"
banner "DEPLOY COMPLETE"

case "$allow_dev" in
    false) green "ATTEST_ALLOW_DEV=false — production-safe."  ;;
    true)  yellow "ATTEST_ALLOW_DEV=true — dev/staging mode (App Store builds will be rejected). See README §3." ;;
    *)     red    "ATTEST_ALLOW_DEV=$allow_dev — unrecognised. Investigate." ;;
esac
