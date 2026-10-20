#!/usr/bin/env bash
# 把 App Store Connect 导出的一次性 offer code CSV 灌进 code_pool（推荐好友 → 朋友的免费月）。
#
#   SUPABASE_URL=https://xxx.supabase.co SUPABASE_SERVICE_ROLE_KEY=eyJ... \
#     deploy/supabase/import-codes.sh codes.csv "Referral Friend Month" 2027-04-19
#
# 参数：CSV 路径、那批码在 App Store Connect 里的 reference name、过期日
# （YYYY-MM-DD，就是生成时填的那个，Apple 最长给 6 个月）。
#
# 先 staging（secrets-staging.env 里的 SUPABASE_URL / KEY）再 prod。
# 重复导入同一份 CSV 是安全的：主键是码本身，已在库里的会被忽略
# （Prefer: resolution=ignore-duplicates），已经发出去的那些不会被打回 free。
# 凭据只从环境变量读，不回显。
set -euo pipefail

csv="${1:?用法: import-codes.sh <csv> <offer-ref-name> <YYYY-MM-DD>}"
ref_name="${2:?那批码在 App Store Connect 里的 reference name}"
expires="${3:?过期日 YYYY-MM-DD}"
: "${SUPABASE_URL:?}" "${SUPABASE_SERVICE_ROLE_KEY:?}"
[[ "$expires" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "过期日要是 YYYY-MM-DD" >&2; exit 2; }

base="${SUPABASE_URL%/}"

# CSV 里可能有表头、BOM、引号、CRLF。码本身只有大写字母和数字，所以不猜列：
# 把每一行里长得像码的那一段挑出来，挑不出来的行直接丢掉。
codes=$(tr -d '\r"' < "$csv" | awk 'BEGIN { FS = "[,\t ]+" } { for (i = 1; i <= NF; i++) if ($i ~ /^[A-Z0-9]{6,20}$/) { print $i; break } }' | sort -u)
n=$(printf '%s\n' "$codes" | grep -c . || true)
[ "$n" -gt 0 ] || { echo "$csv 里一个码都没认出来" >&2; exit 1; }
echo "认出 $n 个码 → ref=\"$ref_name\" expires=$expires"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# 一次 500 条：PostgREST 一个请求装得下，失败了也好定位是哪一段。
printf '%s\n' "$codes" | grep . | split -l 500 - "$tmp/part."
for part in "$tmp"/part.*; do
  json=$(awk -v r="$ref_name" -v e="$expires" '
    BEGIN { printf "[" }
    { printf "%s{\"code\":\"%s\",\"offer_ref_name\":\"%s\",\"expires_at\":\"%sT00:00:00Z\"}", (NR>1?",":""), $0, r, e }
    END { printf "]" }' "$part")
  curl -fsS -X POST "$base/rest/v1/code_pool" \
    -H "apikey: $SUPABASE_SERVICE_ROLE_KEY" \
    -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY" \
    -H "Content-Type: application/json" \
    -H "Prefer: resolution=ignore-duplicates,return=minimal" \
    --data-binary "$json" > /dev/null
  echo "  灌了 $(wc -l < "$part" | tr -d ' ') 条"
done

# 现在还剩多少没发的（PostgREST 的 count 走 Content-Range 头）。
left=$(curl -fsSI "$base/rest/v1/code_pool?select=code&state=eq.free&expires_at=gt.$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -H "apikey: $SUPABASE_SERVICE_ROLE_KEY" -H "Authorization: Bearer $SUPABASE_SERVICE_ROLE_KEY" \
  -H "Prefer: count=exact" -H "Range: 0-0" \
  | awk -F/ '/[Cc]ontent-[Rr]ange/ { gsub(/\r/, "", $2); print $2 }')
echo "库存（未过期、未发出；没扣 issue_margin_days 余量，精确数看 deploy/referral-pool.sh）：${left:-?} 条"
