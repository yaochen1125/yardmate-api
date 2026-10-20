#!/usr/bin/env bash
# 推荐奖励：礼品卡申请的运营脚本（rewards/SPEC.md，表来自 015_referral_rewards.sql）。
#
#   SUPABASE_URL=https://xxx.supabase.co SUPABASE_SERVICE_ROLE_KEY=eyJ... deploy/referral-payout.sh list
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-payout.sh mark-sent 17
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-payout.sh cancel 17
#
# list [state]   列出申请（默认 requested，最早的在前）：id / 申请时间 / 金额 / 邮箱 / user_id。
#                state 也可以是 sent / cancelled。
# mark-sent <id> 买好 Amazon.com $100 礼品卡、把码发到那个邮箱之后标 sent（sent_at = 现在）。
#                App 下次拉 /v1/referral/me 显示「Sent {date}」。
# cancel <id>    不发了（邮箱明显不对 / 收不到 W-9 等）：标 cancelled。不用改账本——
#                可兑余额只扣 requested + sent，取消的 $100 自然回到对方余额。
#
# mark-sent / cancel 只改 state = requested 的那一行；id 不存在、已经不是 requested、
# 或对方已删号（行随账号删了）→ 报错退出，什么都不改。
# 先 staging（secrets-staging.env 里的 SUPABASE_URL / KEY）再 prod。凭据只从环境变量读，不回显。
set -euo pipefail

usage() {
  echo "用法: referral-payout.sh list [requested|sent|cancelled] | mark-sent <id> | cancel <id>" >&2
  exit 2
}

cmd="${1:-}"
[ -n "$cmd" ] || usage
: "${SUPABASE_URL:?}" "${SUPABASE_SERVICE_ROLE_KEY:?}"

base="${SUPABASE_URL%/}/rest/v1/reward_payouts"
auth=(-H "apikey: $SUPABASE_SERVICE_ROLE_KEY" -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY")

# 有 jq 就排成一行一条，没有就原样打 JSON。
show() {
  if command -v jq > /dev/null; then
    jq -r '.[] | "#\(.id)  \(.state)  requested \(.requested_at)  sent \(.sent_at // "-")  $\(.amount_cents / 100)  \(.email)  user \(.user_id)"'
  else
    cat; echo
  fi
}

# PATCH 一行 requested → $1，带上时间列 $2。PostgREST 按 filter 改：只有 state=requested 的那行会动。
transition() {
  local to="$1" col="$2" id="$3"
  [[ "$id" =~ ^[0-9]+$ ]] || usage
  local now out
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  out=$(curl -fsS -X PATCH "$base?id=eq.$id&state=eq.requested" "${auth[@]}" \
    -H "Content-Type: application/json" \
    -H "Prefer: return=representation" \
    --data-binary "{\"state\":\"$to\",\"$col\":\"$now\"}")
  if [ "$out" = "[]" ]; then
    echo "#$id 不是 requested（不存在 / 已发 / 已取消 / 对方已删号），没改任何东西" >&2
    exit 1
  fi
  echo "#$id → $to"
  printf '%s' "$out" | show
}

case "$cmd" in
  list)
    state="${2:-requested}"
    case "$state" in requested|sent|cancelled) ;; *) usage ;; esac
    order="requested_at.asc"
    [ "$state" = "requested" ] || order="requested_at.desc"
    curl -fsS "$base?select=id,user_id,email,amount_cents,state,requested_at,sent_at&state=eq.$state&order=$order" "${auth[@]}" | show
    ;;
  mark-sent) transition sent sent_at "${2:-}" ;;
  cancel)    transition cancelled cancelled_at "${2:-}" ;;
  *) usage ;;
esac
