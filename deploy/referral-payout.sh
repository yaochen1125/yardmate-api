#!/usr/bin/env bash
# 推荐奖励：礼品卡申请的运营脚本（rewards/SPEC.md；表来自 015，缓冲 / 撤回来自 018）。
#
#   SUPABASE_URL=https://xxx.supabase.co SUPABASE_SERVICE_ROLE_KEY=eyJ... deploy/referral-payout.sh list
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-payout.sh mark-sent 17
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-payout.sh undo 17
#   SUPABASE_URL=... SUPABASE_SERVICE_ROLE_KEY=... deploy/referral-payout.sh cancel 17
#
# list [state]   不带参数：要处理的——requested（最早的在前）+ 已标记、还在 1 小时缓冲里的。
#                也可以是 requested / sent / cancelled，只列那一种。
# mark-sent <id> 买好 Amazon.com $100 礼品卡、把码发到那个邮箱之后标记已发。先列出这条申请，
#                再让你输一遍编号、再输 yes，两次都对才执行（必须在终端里手输，不能用管道喂）。
#                执行后是「已发、待生效」：1 小时内可以 undo；这 1 小时里对方 App 仍显示
#                「Gift card on its way」，过了 1 小时才显示「Sent {date}」（rewards.md ### 标记已发）。
# undo <id>      标记后 1 小时内撤回成 requested。只撤我们的记录——亚马逊那边已经发出的卡，
#                要去亚马逊那张礼品卡的订单里自己处理。过了 1 小时拒绝执行。
# cancel <id>    不发了（邮箱明显不对 / 收不到 W-9 等）：requested → cancelled，那 $100 自然回到
#                对方余额（可兑余额只扣 requested + sent）。缓冲中的先 undo 再 cancel。
#
# mark-sent / undo 走数据库函数 reward_payout_mark_sent / reward_payout_undo（018），判断 + 更新
# 一步完成、时间取数据库 now()。id 不存在、状态不对、或对方已删号（行随账号删了）→ 报错退出，
# 什么都不改。先 staging（secrets-staging.env 里的 SUPABASE_URL / KEY）再 prod。凭据只从环境变量读，不回显。
set -euo pipefail

usage() {
  echo "用法: referral-payout.sh list [requested|sent|cancelled] | mark-sent <id> | undo <id> | cancel <id>" >&2
  exit 2
}

cmd="${1:-}"
[ -n "$cmd" ] || usage
: "${SUPABASE_URL:?}" "${SUPABASE_SERVICE_ROLE_KEY:?}"

rest="${SUPABASE_URL%/}/rest/v1"
base="$rest/reward_payouts"
auth=(-H "apikey: $SUPABASE_SERVICE_ROLE_KEY" -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY")
cols="id,user_id,email,amount_cents,state,requested_at,sent_at,undo_until"

need_jq() {
  command -v jq > /dev/null || { echo "mark-sent / undo 需要 jq（brew install jq / apt install jq）" >&2; exit 1; }
}

now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# 一行一条；缓冲中的（state = sent 且 undo_until 还没到）标出来。没有 jq 就原样打 JSON。
show() {
  if command -v jq > /dev/null; then
    jq -r --arg now "$(now_utc)" '.[] |
      (if .state == "sent" and .undo_until != null and (.undo_until | sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z")) > $now
         then "sent（缓冲中，\(.undo_until | .[0:16] | sub("T"; " ")) UTC 前可 undo）"
         else .state end) as $st |
      "#\(.id)  \($st)  requested \(.requested_at)  sent \(.sent_at // "-")  $\(.amount_cents / 100)  \(.email)  user \(.user_id)"'
  else
    cat; echo
  fi
}

check_id() { [[ "$1" =~ ^[0-9]+$ ]] || usage; }

# 读一行申请（JSON 数组，可能为空）。
fetch() {
  curl -fsS "$base?select=$cols&id=eq.$1" "${auth[@]}"
}

# 调一个 018 的函数，返回改到的行（JSON 数组；没改到 = []）。
rpc() {
  local fn="$1" body="$2"
  curl -fsS -X POST "$rest/rpc/$fn" "${auth[@]}" \
    -H "Content-Type: application/json" \
    --data-binary "$body"
}

mark_sent() {
  local id="$1"
  check_id "$id"
  need_jq
  [ -t 0 ] || { echo "mark-sent 要在终端里手输两次确认，不能用管道 / 重定向喂" >&2; exit 2; }
  local row
  row=$(fetch "$id")
  if [ "$(printf '%s' "$row" | jq 'length')" = "0" ]; then
    echo "#$id 不存在（或对方已删号），没改任何东西" >&2
    exit 1
  fi
  local state email
  state=$(printf '%s' "$row" | jq -r '.[0].state')
  email=$(printf '%s' "$row" | jq -r '.[0].email')
  if [ "$state" != "requested" ]; then
    echo "#$id 是 $state，不是 requested（已标记 / 已取消），没改任何东西" >&2
    printf '%s' "$row" | show >&2
    exit 1
  fi

  echo "要标记为已发的申请："
  printf '%s' "$row" | jq -r '.[0] | "  编号：    #\(.id)\n  收卡邮箱：\(.email)\n  金额：    $\(.amount_cents / 100)\n  申请时间：\(.requested_at)\n  用户 ID： \(.user_id)"'
  echo

  local typed
  read -r -p "第一次确认：再输一遍编号：" typed
  if [ "$typed" != "$id" ]; then
    echo "编号不一致，退出，没改任何东西" >&2
    exit 1
  fi
  read -r -p "第二次确认：已经在亚马逊买好 \$$(printf '%s' "$row" | jq -r '.[0].amount_cents / 100') 礼品卡、发到上面这个邮箱了？输入 yes：" typed
  if [ "$typed" != "yes" ]; then
    echo "没有输入 yes，退出，没改任何东西" >&2
    exit 1
  fi

  local body out
  body=$(jq -cn --argjson id "$id" --arg email "$email" '{p_id: $id, p_email: $email}')
  out=$(rpc reward_payout_mark_sent "$body")
  if [ "$(printf '%s' "$out" | jq 'length')" = "0" ]; then
    echo "#$id 刚刚变过（已被标记 / 取消，或用户改了邮箱），没改任何东西。重新跑一次 mark-sent 看最新的。" >&2
    exit 1
  fi
  local until
  until=$(printf '%s' "$out" | jq -r '.[0].undo_until | .[0:16] | sub("T"; " ")')
  echo "#$id → 已发、待生效"
  printf '%s' "$out" | show
  echo "1 小时缓冲：$until UTC（本机时间约 $(date -v+1H +%H:%M 2>/dev/null || date -d '+1 hour' +%H:%M)）之前可以撤回："
  echo "  deploy/referral-payout.sh undo $id"
}

undo() {
  local id="$1"
  check_id "$id"
  need_jq
  local out
  out=$(rpc reward_payout_undo "{\"p_id\":$id}")
  if [ "$(printf '%s' "$out" | jq 'length')" = "0" ]; then
    local row
    row=$(fetch "$id")
    if [ "$(printf '%s' "$row" | jq 'length')" = "0" ]; then
      echo "#$id 不存在（或对方已删号），没改任何东西" >&2
    elif [ "$(printf '%s' "$row" | jq -r '.[0].state')" = "sent" ]; then
      echo "#$id 已过 1 小时缓冲，不能撤回（已正式算已发）。要改只能人工处理。" >&2
      printf '%s' "$row" | show >&2
    else
      echo "#$id 不是缓冲中的已发申请（现在是 $(printf '%s' "$row" | jq -r '.[0].state')），没改任何东西" >&2
    fi
    exit 1
  fi
  echo "#$id → requested（已撤回）"
  printf '%s' "$out" | show
  echo "注意：这只撤回了我们的记录。亚马逊那边已经发出的卡，去亚马逊那张礼品卡的订单里自己处理。"
}

cancel() {
  local id="$1"
  check_id "$id"
  local out
  out=$(curl -fsS -X PATCH "$base?id=eq.$id&state=eq.requested&select=$cols" "${auth[@]}" \
    -H "Content-Type: application/json" \
    -H "Prefer: return=representation" \
    --data-binary "{\"state\":\"cancelled\",\"cancelled_at\":\"$(now_utc)\"}")
  if [ "$out" = "[]" ]; then
    echo "#$id 不是 requested（不存在 / 已发 / 已取消 / 对方已删号；缓冲中的先 undo 再 cancel），没改任何东西" >&2
    exit 1
  fi
  echo "#$id → cancelled"
  printf '%s' "$out" | show
}

case "$cmd" in
  list)
    state="${2:-}"
    case "$state" in
      "")
        curl -fsS "$base?select=$cols&or=(state.eq.requested,and(state.eq.sent,undo_until.gt.$(now_utc)))&order=requested_at.asc" "${auth[@]}" | show
        ;;
      requested)
        curl -fsS "$base?select=$cols&state=eq.requested&order=requested_at.asc" "${auth[@]}" | show
        ;;
      sent|cancelled)
        curl -fsS "$base?select=$cols&state=eq.$state&order=requested_at.desc" "${auth[@]}" | show
        ;;
      *) usage ;;
    esac
    ;;
  mark-sent) mark_sent "${2:-}" ;;
  undo)      undo "${2:-}" ;;
  cancel)    cancel "${2:-}" ;;
  *) usage ;;
esac
