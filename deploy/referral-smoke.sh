#!/usr/bin/env bash
# referral-smoke.sh — 对 staging（默认）走一遍推荐好友三个接口。
#
#   TOKEN_A=<A 的 Supabase access token> TOKEN_B=<B 的 Supabase access token> \
#     deploy/referral-smoke.sh
#
# 可选：BASE（默认 https://api-staging.yardmate.ai）。
#
# 不带 token 时只跑不需要身份的那几条（healthz / 缺头 400 / 坏 token 401），
# 带两个 token 时走：A /me 造码 → B /me → B claim A 的码（拿到码池里一张码）
# → B 再 claim（拿回同一张）→ B /redeemed。token 不回显。
#
# 拿 token：staging Supabase 里的两个账号，
#   curl -sS "$SUPABASE_URL/auth/v1/token?grant_type=password" -H "apikey: <anon key>" \
#     -H "Content-Type: application/json" -d '{"email":"...","password":"..."}' | jq -r .access_token
# 或者 DEBUG 包切到 staging 登录后从 keychain / 日志里取。
set -euo pipefail

BASE="${BASE:-https://api-staging.yardmate.ai}"
DEV_A="${DEV_A:-11111111-1111-4111-8111-111111111111}"
DEV_B="${DEV_B:-22222222-2222-4222-8222-222222222222}"
APPV="2.2.0 (smoke)"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
die()   { red "FATAL: $*" >&2; exit 1; }

# req <method> <path> <device> <token|-> [json body] → 打印 "<status> <body>"
req() {
    local method="$1" p="$2" dev="$3" tok="$4" data="${5:-}"
    local args=(-sS -m 20 -X "$method" "$BASE$p" -H "X-Device-Install-Id: $dev" -H "X-App-Version: $APPV" -H "Content-Type: application/json" -w ' %{http_code}')
    [[ "$tok" != "-" ]] && args+=(-H "Authorization: Bearer $tok")
    [[ -n "$data" ]] && args+=(--data "$data")
    local out; out=$(curl "${args[@]}")
    echo "${out##* } ${out% *}"
}
expect() { # expect <label> <want-status> <got-line>
    local label="$1" want="$2" got="$3"
    if [[ "${got%% *}" == "$want" ]]; then green "  ok   $label -> $got"; else red "  FAIL $label -> $got (want $want)"; FAILED=1; fi
}
FAILED=0

echo "== $BASE"
curl -sf -m 10 "$BASE/healthz" | grep -q '"ok"' || die "healthz 不健康"
green "  ok   healthz"

echo "== 不需要身份的路径"
noDev=$(curl -sS -m 20 "$BASE/v1/referral/me" -H "X-App-Version: $APPV" -H "Authorization: Bearer x" -w ' %{http_code}')
expect "me 缺 X-Device-Install-Id"   400 "${noDev##* } ${noDev% *}"
expect "me 坏 token"                 401 "$(req GET  /v1/referral/me       "$DEV_A" "not-a-jwt")"
expect "claim 坏 token"              401 "$(req POST /v1/referral/claim    "$DEV_A" "not-a-jwt" '{"code":"YM-7KQ2"}')"
expect "redeemed 坏 token"           401 "$(req POST /v1/referral/redeemed "$DEV_A" "not-a-jwt" '{"offer_code":"X"}')"

if [[ -z "${TOKEN_A:-}" || -z "${TOKEN_B:-}" ]]; then
    echo
    echo "TOKEN_A / TOKEN_B 未设，跳过带身份的流程（见文件头怎么拿）。"
    [[ $FAILED -eq 0 ]] && green "SMOKE (unauthenticated part) OK" || die "有失败项"
    exit 0
fi

echo "== A /me（第一次调用造码）"
meA=$(req GET /v1/referral/me "$DEV_A" "$TOKEN_A"); expect "A me" 200 "$meA"
codeA=$(echo "${meA#* }" | sed -n 's/.*"code":"\([^"]*\)".*/\1/p')
[[ -n "$codeA" ]] || die "A 没拿到邀请码"
echo "  A code = $codeA"

echo "== B /me"
meB=$(req GET /v1/referral/me "$DEV_B" "$TOKEN_B"); expect "B me" 200 "$meB"
echo "${meB#* }" | grep -q '"can_claim":true' || red "  注意：B can_claim 不是 true（已填过码或 RevenueCat 有订阅记录），claim 会被拒"

echo "== B claim A 的码（小写带空格，服务端归一化）"
lower=$(echo "$codeA" | tr 'A-Z' 'a-z' | sed 's/-/ /')
c1=$(req POST /v1/referral/claim "$DEV_B" "$TOKEN_B" "{\"code\":\"$lower\",\"subscribed_before\":false}")
case "${c1%% *}" in
  200) green "  ok   B claim -> $c1" ;;
  503) red "  B claim -> $c1（码池空了？先灌码：deploy/supabase/import-codes.sh）"; FAILED=1 ;;
  *)   red "  FAIL B claim -> $c1"; FAILED=1 ;;
esac
offer=$(echo "${c1#* }" | sed -n 's/.*"offer_code":"\([^"]*\)".*/\1/p')

if [[ -n "$offer" ]]; then
    echo "== B 再 claim（应拿回同一张）"
    c2=$(req POST /v1/referral/claim "$DEV_B" "$TOKEN_B" "{\"code\":\"$codeA\",\"subscribed_before\":false}")
    if [[ "${c2#* }" == *"\"offer_code\":\"$offer\""* ]]; then green "  ok   same offer code"; else red "  FAIL second claim -> $c2"; FAILED=1; fi

    echo "== 错误码"
    expect "A claim 自己的码 own_code"       400 "$(req POST /v1/referral/claim "$DEV_A" "$TOKEN_A" "{\"code\":\"$codeA\"}")"
    expect "B claim 不存在的码 unknown_code" 400 "$(req POST /v1/referral/claim "$DEV_B" "$TOKEN_B" '{"code":"YM-2345"}')"
    expect "B subscribed_before not_new_user" 400 "$(req POST /v1/referral/claim "$DEV_B" "$TOKEN_B" "{\"code\":\"$codeA\",\"subscribed_before\":true}")"
    expect "bad_code"                        400 "$(req POST /v1/referral/claim "$DEV_B" "$TOKEN_B" '{"code":"YM-7KQ0"}')"

    echo "== B /redeemed（只是提醒服务端去查）"
    expect "B redeemed" 200 "$(req POST /v1/referral/redeemed "$DEV_B" "$TOKEN_B" "{\"offer_code\":\"$offer\"}")"
    expect "B redeemed again (idempotent)" 200 "$(req POST /v1/referral/redeemed "$DEV_B" "$TOKEN_B" "{\"offer_code\":\"$offer\"}")"

    echo "== A /me 应显示 invited=1"
    meA2=$(req GET /v1/referral/me "$DEV_A" "$TOKEN_A"); expect "A me" 200 "$meA2"
    echo "${meA2#* }" | grep -q '"invited":1' || { red "  FAIL A invited != 1"; FAILED=1; }
fi

echo
[[ $FAILED -eq 0 ]] && green "SMOKE OK" || die "有失败项"
