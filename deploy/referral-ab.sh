#!/usr/bin/env bash
# 推荐好友：朋友免费月分组实验（月付组 / 年付组）的运营脚本。
# 规则见 yardmate-swiftui docs/releases/v1/main-navigation/more/invite-friends/invite-friends.md
# ### 兑换码分组实验（月付组 / 年付组）、#### 后台：比例 / 统计 / 换码；库函数在迁移 023。
#
#   SUPABASE_URL=https://xxx.supabase.co SUPABASE_SERVICE_ROLE_KEY=eyJ... deploy/referral-ab.sh show
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-ab.sh set 50
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-ab.sh switch-unredeemed year
#
# show                 年付组占比、实验开始时间、两组可发库存、没兑人数、不认识的码名，以及两组统计
#                      （每个领码的实收是主指标；365 天前不下「哪组收入更好」的结论）。
# set <0-100>          改年付组占比。要再输一遍新数字确认。第一次从 0 改成大于 0 时库里记下实验开始时间。
# switch-unredeemed <month|year>
#                      把实验开始后领的、领了码还没兑、不在目标组的推荐换成目标组的码（老码作废，不再发）。
#                      先打印会动多少条、目标组的码够不够，输入 yes 才执行；码不够一条都不动。
#
# 先 staging（secrets-staging.env 里的 SUPABASE_URL / KEY）再 prod。凭据只从环境变量读，不回显。
set -euo pipefail

usage() {
  echo "用法: referral-ab.sh show | set <0-100> | switch-unredeemed <month|year>" >&2
  exit 2
}

cmd="${1:-}"
[ -n "$cmd" ] || usage
: "${SUPABASE_URL:?}" "${SUPABASE_SERVICE_ROLE_KEY:?}"
command -v jq > /dev/null || { echo "需要 jq（brew install jq / apt install jq）" >&2; exit 1; }

rest="${SUPABASE_URL%/}/rest/v1"
auth=(-H "apikey: $SUPABASE_SERVICE_ROLE_KEY" -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY")

# 调一个库函数（023），body 是 JSON 参数。出错时把数据库的报错打出来（不含凭据）。
rpc() {
  local fn="$1" body="${2:-{\}}" out code
  out=$(curl -sS -w '\n%{http_code}' -X POST "$rest/rpc/$fn" "${auth[@]}" \
          -H "Content-Type: application/json" -d "$body")
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [ "$code" -ge 300 ]; then
    echo "调用 $fn 失败（HTTP $code）：$(echo "$out" | jq -r '.message // .' 2>/dev/null || echo "$out")" >&2
    return 1
  fi
  echo "$out"
}

overview() {
  rpc referral_ab_overview | jq -r '
    "年付组占比：\(.offer_year_pct)%",
    "实验开始：\(.offer_ab_started_at // "还没开始（占比一直是 0）")",
    "可发库存：月付组 \(.usable_month) 张，年付组 \(.usable_year) 张（年付码名「\(.offer_ref_year)」）",
    "领了码还没兑：月付组 \(.unredeemed_month) 人，年付组 \(.unredeemed_year) 人",
    (if (.unknown_names | length) > 0
       then "⚠️ 不认识的码名（会被当月付码发出去，核对导入时的名字）：\(.unknown_names | join("、"))"
       else empty end)'
}

pct() { # a / b → 百分比，b = 0 时打 -
  awk -v a="$1" -v b="$2" 'BEGIN { if (b == 0) print "-"; else printf "%.1f%%", 100 * a / b }'
}

dollars() { # 分 / 人 → 每人美元，人 = 0 时打 -
  awk -v c="$1" -v n="$2" 'BEGIN { if (n == 0) print "-"; else printf "$%.2f", c / 100 / n }'
}

stats() {
  local rows
  rows=$(rpc referral_ab_stats)
  if [ "$(echo "$rows" | jq 'map(select(.claimed > 0)) | length')" = "0" ]; then
    echo; echo "统计：还没有实验开始后领码的推荐。"
    return
  fi
  for v in month year; do
    local name; [ "$v" = month ] && name="月付组" || name="年付组"
    local r; r=$(echo "$rows" | jq -c --arg v "$v" 'map(select(.variant == $v)) | .[0]')
    local claimed redeemed paid renewed refunded switched
    claimed=$(echo "$r" | jq .claimed); redeemed=$(echo "$r" | jq .redeemed); paid=$(echo "$r" | jq .paid)
    renewed=$(echo "$r" | jq .renewed); refunded=$(echo "$r" | jq .refunded); switched=$(echo "$r" | jq .switched)
    echo
    echo "== $name：领码 $claimed，兑换率 $(pct "$redeemed" "$claimed")，转付费率 $(pct "$paid" "$redeemed")，续费率 $(pct "$renewed" "$paid")，退款率 $(pct "$refunded" "$paid")（被换过组的另有 $switched 人，不计入）"
    echo "   窗口   每个领码的实收（人数）   每个兑换的实收（人数）"
    echo "$rows" | jq -r --arg v "$v" 'map(select(.variant == $v)) | .[] |
      "\(.window_days) \(.claim_proceeds_cents) \(.claim_cohort) \(.redeem_proceeds_cents) \(.redeem_cohort)"' |
    while read -r d cp cn rp rn; do
      printf "   %3s 天  %-10s (%s)            %-10s (%s)\n" "$d" "$(dollars "$cp" "$cn")" "$cn" "$(dollars "$rp" "$rn")" "$rn"
    done
  done
  echo
  echo "主指标是「每个领码的实收」。年付组收入集中在免费月刚结束，月付组要到第 6 个月左右才追平、续费要第 13 个月才看得到：365 天之前不下「哪组收入更好」的结论。"
}

confirm_tty() {
  [ -t 0 ] || { echo "需要在终端里交互确认（stdin 不是终端）" >&2; exit 1; }
}

case "$cmd" in
  show)
    overview
    stats
    ;;
  set)
    new="${2:-}"
    [[ "$new" =~ ^[0-9]+$ ]] && [ "$new" -le 100 ] || usage
    confirm_tty
    overview | head -2
    echo
    read -r -p "把年付组占比改成 $new%？再输一遍新数字确认：" again
    [ "$again" = "$new" ] || { echo "两次不一致，没改。" >&2; exit 1; }
    rpc referral_set_offer_year_pct "{\"p_pct\": $new}" | jq -r '"已改：\(.before)% → \(.after)%。实验开始：\(.started_at // "还没开始")"'
    ;;
  switch-unredeemed)
    to="${2:-}"
    [ "$to" = month ] || [ "$to" = year ] || usage
    confirm_tty
    dry=$(rpc referral_switch_unredeemed "{\"p_to\": \"$to\", \"p_execute\": false}")
    n=$(echo "$dry" | jq .candidates); a=$(echo "$dry" | jq .available)
    echo "会把 $n 个「实验开始后领的、领了码还没兑、不在 $to 组」的推荐换成 $to 组；$to 组可发的码 $a 张。"
    [ "$n" -gt 0 ] || { echo "没有要换的。"; exit 0; }
    [ "$a" -ge "$n" ] || { echo "码不够，一条都不动。先导入 $to 组的码。" >&2; exit 1; }
    read -r -p "输入 yes 执行：" ok
    [ "$ok" = yes ] || { echo "没执行。"; exit 1; }
    rpc referral_switch_unredeemed "{\"p_to\": \"$to\", \"p_execute\": true}" | jq -r '"已换 \(.moved) 条。朋友下次进「就差一步」那一页时拿到新码（同一账号 24 小时最多重领一次）。"'
    ;;
  *)
    usage
    ;;
esac
