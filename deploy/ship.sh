#!/usr/bin/env bash
# ship.sh — 后端全自动发布管线：staging → 冒烟校验 → prod → 冒烟校验。
#
# 任一步失败立即中止，prod 不动。自动化的门 = 技术校验（go test 在两个 deploy
# 脚本里各跑一次 + healthz + 关键路由 4xx 可达性），不替代对业务行为的人工验证；
# 想中途在 App 里人工看一眼，用原来的两步流程（deploy-staging.sh → deploy.sh）。
#
# Usage:
#   ./deploy/ship.sh                 全自动 staging→prod
#   ./deploy/ship.sh --staging-only  只发 staging + 冒烟（想先人工验证时用）
#
# YARDMATE_SECRETS 默认 ~/.config/yardmate-api/secrets.env.prod（prod 段用）。

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROD_SECRETS="${YARDMATE_SECRETS:-$HOME/.config/yardmate-api/secrets.env.prod}"
STAGING_ONLY=0
[[ "${1:-}" == "--staging-only" ]] && STAGING_ONLY=1

green()  { printf '\033[32m%s\033[0m\n' "$*"; }
red()    { printf '\033[31m%s\033[0m\n' "$*"; }

# 冒烟：healthz 必须 ok；关键路由未认证应答 4xx（路由/nginx/TLS 活着），5xx 或不通 = 拦截。
smoke() {
    local base="$1"
    curl -sf --max-time 10 "$base/healthz" | grep -q '"ok"' \
        || { red "FATAL: $base/healthz 不健康"; exit 1; }
    echo "    healthz ok"
    # 变量名避开 path：zsh 里 path 绑定 PATH，误 source 时会炸掉后续命令查找。
    local p code
    for p in /v1/identify /v1/plants/enrichment /v1/account/delete; do
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$base$p")
        [[ "$code" =~ ^4 ]] || { red "FATAL: $base$p 返回 $code（期望 4xx）"; exit 1; }
        echo "    $p -> $code ok"
    done
}

echo "=== [1/4] deploy staging ==="
# env -u：本进程的 YARDMATE_SECRETS 指向 prod secrets，deploy-staging.sh 会把任何
# 非空 YARDMATE_SECRETS 当 staging secrets 装到 secrets-staging.env——staging 是
# 独立 Supabase 项目，混入 prod 凭证会让 staging 直连 prod 数据。staging 段一律
# 只发二进制，保留服务器现有 secrets-staging.env。
env -u YARDMATE_SECRETS "$DIR/deploy-staging.sh"

echo "=== [2/4] staging 冒烟（公网全链路：DNS/TLS/nginx/app）==="
smoke https://api-staging.yardmate.ai

if [[ $STAGING_ONLY -eq 1 ]]; then
    green "STAGING ONLY DONE — 验证后跑 YARDMATE_SECRETS=$PROD_SECRETS $DIR/deploy.sh 发 prod"
    exit 0
fi

echo "=== [3/4] deploy prod ==="
[[ -f "$PROD_SECRETS" ]] || { red "FATAL: prod secrets 不存在: $PROD_SECRETS"; exit 1; }
YARDMATE_SECRETS="$PROD_SECRETS" "$DIR/deploy.sh"

echo "=== [4/4] prod 冒烟 ==="
smoke https://api.yardmate.ai

green "=== SHIP COMPLETE: staging + prod 均已发布并通过冒烟 ==="
