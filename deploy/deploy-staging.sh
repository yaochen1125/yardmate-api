#!/usr/bin/env bash
# deploy-staging.sh — build the linux/amd64 binary and ship it to the STAGING
# instance (yardmate-api-staging.service, 127.0.0.1:8081, api-staging.yardmate.ai).
#
# 与 deploy.sh（prod）的差异：
#   - 目标 = yardmate-api-staging（独立 binary / unit / secrets / DB / 端口）
#   - ATTEST_ALLOW_DEV=true 是预期（staging 供模拟器 / 开发机 App Attest 降级路径）
#   - 不带 YARDMATE_SECRETS 时**只发二进制**，保留服务器上现有
#     /etc/yardmate-api/secrets-staging.env（日常改代码验证的最短路径）
#
# 流程：staging 验证通过后，再用 ./deploy/deploy.sh 发 prod。
#
# Usage:
#   ./deploy/deploy-staging.sh          # 只发二进制 + unit，保留服务器 secrets
#   YARDMATE_SECRETS=~/.config/yardmate-api/secrets.env.staging ./deploy/deploy-staging.sh
#
# Optional env:
#   YARDMATE_DEPLOY_HOST   default 5.78.183.252
#   YARDMATE_DEPLOY_USER   default root

set -euo pipefail

HOST="${YARDMATE_DEPLOY_HOST:-5.78.183.252}"
USER="${YARDMATE_DEPLOY_USER:-root}"
SECRETS="${YARDMATE_SECRETS:-}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_OUT="${REPO_ROOT}/bin/yardmate-api-staging-linux-amd64"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
die() { red "FATAL: $*" >&2; exit 1; }

# --- 1. pre-flight: secrets file（可选；不带 = 保留服务器现有 secrets-staging.env）---
SHIP_SECRETS=0
if [[ -n "$SECRETS" ]]; then
    [[ -f "$SECRETS" ]] || die "secrets file not found: $SECRETS"
    chmod 600 "$SECRETS" 2>/dev/null || true
    SHIP_SECRETS=1
    yellow ">> will ship secrets -> /etc/yardmate-api/secrets-staging.env"
else
    yellow ">> YARDMATE_SECRETS unset — keeping server's secrets-staging.env"
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
scp "$BIN_OUT" "$USER@$HOST:/tmp/yardmate-api-staging.new"
scp "$REPO_ROOT/deploy/yardmate-api-staging.service" "$USER@$HOST:/tmp/yardmate-api-staging.service.new"
if [[ $SHIP_SECRETS -eq 1 ]]; then
    scp "$SECRETS" "$USER@$HOST:/tmp/secrets-staging.env.new"
fi

yellow ">> installing on $HOST"
ssh "$USER@$HOST" bash -se -- "$SHIP_SECRETS" <<'REMOTE'
set -euo pipefail
SHIP_SECRETS="$1"

if [[ -f /usr/local/bin/yardmate-api-staging ]]; then
    cp /usr/local/bin/yardmate-api-staging /usr/local/bin/yardmate-api-staging.prev
fi

# BoltDB 状态目录（unit 指向 /var/lib/yardmate-api-staging/credentials.db）：
# 未手动引导过的主机上不存在 → attest.OpenStore 直接 bbolt.Open 报
# no such file or directory，服务起不来（Codex api#87 P2）。install -d 幂等。
install -d -o yardmate-api -g yardmate-api -m 0750 /var/lib/yardmate-api-staging

install -o yardmate-api -g yardmate-api -m 0755 /tmp/yardmate-api-staging.new /usr/local/bin/yardmate-api-staging
install -o root -g root -m 0644 /tmp/yardmate-api-staging.service.new /etc/systemd/system/yardmate-api-staging.service
rm -f /tmp/yardmate-api-staging.new /tmp/yardmate-api-staging.service.new
if [[ "$SHIP_SECRETS" == "1" ]]; then
    install -o yardmate-api -g yardmate-api -m 0600 /tmp/secrets-staging.env.new /etc/yardmate-api/secrets-staging.env
    shred -u /tmp/secrets-staging.env.new
fi

systemctl daemon-reload
systemctl enable yardmate-api-staging
systemctl restart yardmate-api-staging
REMOTE

# --- 5. health check ---
yellow ">> waiting up to 10 s for /healthz (staging :8081)"
for i in 1 2 3 4 5 6 7 8 9 10; do
    if ssh "$USER@$HOST" "curl -sf http://127.0.0.1:8081/healthz" 2>/dev/null | grep -q '"ok"'; then
        green "staging /healthz OK"
        break
    fi
    sleep 1
    if [[ $i -eq 10 ]]; then
        red "/healthz never became healthy. Logs:"
        ssh "$USER@$HOST" "journalctl -u yardmate-api-staging -n 50 --no-pager" >&2 || true
        die "staging deploy aborted; previous binary at /usr/local/bin/yardmate-api-staging.prev"
    fi
done

green "STAGING DEPLOY COMPLETE (prod untouched — ship prod via ./deploy/deploy.sh)"
