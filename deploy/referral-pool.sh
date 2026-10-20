#!/usr/bin/env bash
# referral-pool.sh — 推荐好友码池现状（只读）。
#
#   SUPABASE_DB_URL=postgres://... deploy/referral-pool.sh
#
# 打印两张表：
#   1. 各批次（offer_ref_name + 到期日）的 free / issued / redeemed / expired，
#      以及这批现在还发不发（usable = 到期日在 issue_margin_days 余量之外）
#   2. 汇总：三个阈值（referral_settings）、可发数、余量不足的 free 数、最早到期日、
#      告警判定（与 referral/pool.go 的口径一致：low_stock / expiring）
#
# 只跑 select，不改任何东西。需要 psql（brew 的 libpq 也带）；没有的话
# `deploy/referral-pool.sh --sql` 把 SQL 打出来，贴到 Supabase SQL Editor 跑。
# 先 staging（secrets-staging.env 的 SUPABASE_DB_URL）再 prod。凭据只从环境变量读，不回显。
set -euo pipefail

read -r -d '' BATCHES <<'SQL' || true
select offer_ref_name                                      as batch,
       to_char(expires_at at time zone 'UTC', 'YYYY-MM-DD') as expires,
       count(*) filter (where state = 'free')              as free,
       count(*) filter (where state = 'issued')            as issued,
       count(*) filter (where state = 'redeemed')          as redeemed,
       count(*) filter (where state = 'expired')           as expired,
       case when expires_at > now() + make_interval(days => (select issue_margin_days from referral_settings where id = 1))
            then 'yes' else 'no' end                       as usable
  from code_pool
 group by offer_ref_name, expires_at
 order by expires_at, offer_ref_name;
SQL

read -r -d '' SUMMARY <<'SQL' || true
with s as (select issue_margin_days, low_stock_threshold, expiry_warn_days from referral_settings where id = 1),
     c as (select expires_at,
                  expires_at > now() + make_interval(days => s.issue_margin_days) as usable
             from code_pool, s
            where state = 'free'),
     a as (select count(*) filter (where usable)                          as usable,
                  count(*) filter (where not usable)                      as free_but_short,
                  min(expires_at) filter (where usable)                   as earliest
             from c)
select s.issue_margin_days                                   as margin_days,
       s.low_stock_threshold                                 as low_stock_threshold,
       s.expiry_warn_days                                    as expiry_warn_days,
       a.usable,
       a.free_but_short,
       to_char(a.earliest at time zone 'UTC', 'YYYY-MM-DD')  as earliest_usable_expiry,
       coalesce(nullif(concat_ws('+',
           case when a.usable < s.low_stock_threshold then 'low_stock' end,
           case when a.earliest is not null and s.expiry_warn_days > 0
                 and a.earliest <= now() + make_interval(days => s.expiry_warn_days) then 'expiring' end), ''), 'ok')
                                                             as alert
  from s, a;
SQL

if [[ "${1:-}" == "--sql" ]]; then
    printf -- '-- 各批次\n%s\n\n-- 汇总\n%s\n' "$BATCHES" "$SUMMARY"
    exit 0
fi

: "${SUPABASE_DB_URL:?需要 SUPABASE_DB_URL（或用 --sql 拿 SQL 去 SQL Editor 跑）}"

psql_bin=$(command -v psql || true)
[[ -n "$psql_bin" || ! -x /opt/homebrew/opt/libpq/bin/psql ]] || psql_bin=/opt/homebrew/opt/libpq/bin/psql
[[ -n "$psql_bin" ]] || { echo "找不到 psql（brew install libpq），或者用 --sql 自己去 SQL Editor 跑" >&2; exit 2; }

echo "== 各批次"
"$psql_bin" "$SUPABASE_DB_URL" -X -q -v ON_ERROR_STOP=1 -c "$BATCHES"
echo "== 汇总（alert 口径同 referral/pool.go；补货：deploy/supabase/import-codes.sh codes.csv \"Referral Friend Month\" YYYY-MM-DD）"
"$psql_bin" "$SUPABASE_DB_URL" -X -q -v ON_ERROR_STOP=1 -x -c "$SUMMARY"
