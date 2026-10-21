#!/usr/bin/env bash
# 推荐好友防刷第 4 道（速度闸）的运营审核脚本（referral/SPEC.md §2.4，函数来自 016_referral_antiabuse.sql）。
#
#   SUPABASE_URL=https://xxx.supabase.co SUPABASE_SERVICE_ROLE_KEY=eyJ... deploy/referral-review.sh list
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-review.sh approve 123
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-review.sh reject 123
#
# list           列出待审核（held）的推荐，最早进闸的在前：推荐 id / 推荐人 / 朋友 / 填码时间 /
#                兑换时间（+ hold_days = 速度闸数的理论成功时间，017）/ 认定时间 / 填码设备 /
#                是否已发过运营邮件。同一推荐人设备重复、填码 / 兑换时间扎堆 = 可疑。
# approve <id>   放行：held → qualified（记 reviewed_at）。下一轮结算（≤ 10 分钟）先再查一次朋友的
#                退款 / 订阅（审核期间退了款 → void refunded，不发），过了照常给推荐人发月份或攒着；
#                推荐人那边 months_earned 变了，App 照常发到账通知。只动 held 的那条。
# reject <id>    作废：任何没作废的状态 → void（void_reason = rejected，记 reviewed_at），不发月份，
#                这条推荐名下 pending / available 的礼品卡钱一起标 voided（礼品卡审核看着是刷的
#                时也用它）。已经 rewarded 的：RevenueCat 那边赠出去的月份不收回，脚本会提示。
#
# 都走 PostgREST + service role（rpc referral_approve / referral_reject 只授权给 service_role）。
# 先 staging（secrets-staging.env 里的 SUPABASE_URL / KEY）再 prod。凭据只从环境变量读，不回显。
set -euo pipefail

usage() {
  echo "用法: referral-review.sh list | approve <referral_id> | reject <referral_id>" >&2
  exit 2
}

cmd="${1:-}"
[ -n "$cmd" ] || usage
: "${SUPABASE_URL:?}" "${SUPABASE_SERVICE_ROLE_KEY:?}"

rest="${SUPABASE_URL%/}/rest/v1"
auth=(-H "apikey: $SUPABASE_SERVICE_ROLE_KEY" -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY")

# 有 jq 就排成一行一条，没有就原样打 JSON。
show() {
  if command -v jq > /dev/null; then
    jq -r '.[] | "#\(.id)  referrer \(.referrer)  friend \(.referee)  claimed \(.claimed_at)  qualified \(.qualified_at)  device \(.claim_install_id // "-")  mailed \(.held_notified_at // "no")"'
  else
    cat; echo
  fi
}

rpc() {
  local fn="$1" id="$2"
  [[ "$id" =~ ^[0-9]+$ ]] || usage
  curl -fsS -X POST "$rest/rpc/$fn" "${auth[@]}" \
    -H "Content-Type: application/json" \
    --data-binary "{\"p_id\":$id}"
}

case "$cmd" in
  list)
    curl -fsS "$rest/referrals?select=id,referrer,referee,claimed_at,redeemed_at,qualified_at,claim_install_id,held_notified_at&state=eq.held&order=qualified_at.asc" "${auth[@]}" | show
    ;;
  approve)
    out=$(rpc referral_approve "${2:-}")
    if [[ "$out" == *'"approved" : true'* || "$out" == *'"approved":true'* ]]; then
      echo "#$2 → qualified（下一轮结算先再查朋友退款，过了照常发 / 攒）"
    else
      echo "#$2 不是 held（不存在 / 已审 / 已作废），没改任何东西：$out" >&2
      exit 1
    fi
    ;;
  reject)
    out=$(rpc referral_reject "${2:-}")
    if [[ "$out" == *'"rejected" : true'* || "$out" == *'"rejected":true'* ]]; then
      echo "#$2 → void (rejected)，名下的钱已一起作废：$out"
      if [[ "$out" == *'rewarded'* ]]; then
        echo "注意：这条之前是 rewarded，RevenueCat 那边已赠的月份不会收回。" >&2
      fi
    else
      echo "#$2 没作废（不存在 / 本来就是 void），没改任何东西：$out" >&2
      exit 1
    fi
    ;;
  *) usage ;;
esac
