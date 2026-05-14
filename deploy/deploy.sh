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
for key in ATTEST_ALLOW_DEV OPENAI_API_KEY PLANT_ID_API_KEY; do
    val=$(grep -E "^${key}=" "$SECRETS" | head -1 | cut -d= -f2-)
    [[ -n "$val" ]] || die "missing or empty key '$key' in $SECRETS"
done

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

# Save previous binary for one-step rollback.
if [[ -f /usr/local/bin/yardmate-api ]]; then
    cp /usr/local/bin/yardmate-api /usr/local/bin/yardmate-api.prev
fi

install -o yardmate-api -g yardmate-api -m 0755 /tmp/yardmate-api.new /usr/local/bin/yardmate-api
install -o yardmate-api -g yardmate-api -m 0600 /tmp/secrets.env.new /etc/yardmate-api/secrets.env
install -o root -g root -m 0644 /tmp/yardmate-api.service.new /etc/systemd/system/yardmate-api.service

shred -u /tmp/yardmate-api.new /tmp/secrets.env.new /tmp/yardmate-api.service.new

systemctl daemon-reload
systemctl enable yardmate-api
systemctl restart yardmate-api
REMOTE

# --- 5. health check ---
yellow ">> waiting up to 10 s for /healthz"
for i in 1 2 3 4 5 6 7 8 9 10; do
    if ssh "$USER@$HOST" "curl -sf http://127.0.0.1:8080/healthz" 2>/dev/null | grep -q '"ok"'; then
        green "/healthz OK"
        break
    fi
    sleep 1
    if [[ $i -eq 10 ]]; then
        red "/healthz never became healthy. Logs:"
        ssh "$USER@$HOST" "journalctl -u yardmate-api -n 50 --no-pager" >&2 || true
        die "deploy aborted; previous binary at /usr/local/bin/yardmate-api.prev for rollback"
    fi
done

# --- 6. effective config eyeball check ---
banner "EFFECTIVE CONFIG ON $HOST"
ssh "$USER@$HOST" "grep -E '^(ATTEST_ALLOW_DEV)=' /etc/yardmate-api/secrets.env"
banner "DEPLOY COMPLETE"

case "$allow_dev" in
    false) green "ATTEST_ALLOW_DEV=false — production-safe."  ;;
    true)  yellow "ATTEST_ALLOW_DEV=true — dev/staging mode (App Store builds will be rejected). See README §3." ;;
    *)     red    "ATTEST_ALLOW_DEV=$allow_dev — unrecognised. Investigate." ;;
esac
