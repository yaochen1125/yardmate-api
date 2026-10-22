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
#   3. 自动补货（migration 021；先查 information_schema，没跑 021 时这段只打一句提示，
#      其他错误照常报）：补货设置 + 最近 10 次补货
#      （code_restocks：环境、张数、到期日、苹果批次 ID、状态、新进池张数、发起 / 结束时间、失败原因）
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
             from c),
     -- 投影：最早那批到期那天还能发多少——届时也得留够余量，所以只算比它晚到期超过
     -- issue_margin_days 的批（紧挨着到期的那批几乎同时退场，不算顶替）。低于阈值才报
     -- expiring，后面有新批顶着就不报；阈值 0 = 两种告警都关（同 TakeStock）。
     p as (select count(*) filter (where c.expires_at > a.earliest + make_interval(days => s.issue_margin_days)) as after_earliest
             from c, a, s)
select s.issue_margin_days                                   as margin_days,
       s.low_stock_threshold                                 as low_stock_threshold,
       s.expiry_warn_days                                    as expiry_warn_days,
       a.usable,
       a.free_but_short,
       to_char(a.earliest at time zone 'UTC', 'YYYY-MM-DD')  as earliest_usable_expiry,
       p.after_earliest                                      as usable_after_earliest,
       coalesce(nullif(concat_ws('+',
           case when a.usable < s.low_stock_threshold then 'low_stock' end,
           case when a.earliest is not null and s.expiry_warn_days > 0
                 and a.earliest <= now() + make_interval(days => s.expiry_warn_days)
                 and p.after_earliest < s.low_stock_threshold then 'expiring' end), ''), 'ok')
                                                             as alert
  from s, a, p;
SQL

# 021 跑没跑：先看 information_schema 里有没有那一列 / 那张表（只看当前 search_path 里的 schema），
# 输出 "t|t" 之类（列|表）。不在才跳过那一段；其他错误（连不上、权限……）照常报出来、脚本失败。
read -r -d '' RESTOCK_PRESENT <<'SQL' || true
select exists (select 1 from information_schema.columns
                where table_schema = any (current_schemas(false))
                  and table_name = 'referral_settings' and column_name = 'auto_restock_enabled'),
       exists (select 1 from information_schema.tables
                where table_schema = any (current_schemas(false))
                  and table_name = 'code_restocks');
SQL

read -r -d '' RESTOCK_SETTINGS <<'SQL' || true
select auto_restock_enabled, restock_below, restock_batch_size, restock_daily_max,
       restock_lead_days, coalesce(restock_environment, '（空）') as restock_environment
  from referral_settings where id = 1;
SQL

read -r -d '' RESTOCKS <<'SQL' || true
select id, environment, number_of_codes                       as codes,
       to_char(expires_at at time zone 'UTC', 'YYYY-MM-DD')   as expires,
       coalesce(asc_batch_id, '')                             as batch,
       state, imported_count                                  as imported,
       to_char(started_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI')  as started_utc,
       to_char(finished_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI') as finished_utc,
       left(coalesce(error, ''), 120)                         as error
  from code_restocks
 order by started_at desc
 limit 10;
SQL

if [[ "${1:-}" == "--sql" ]]; then
    printf -- '-- 各批次\n%s\n\n-- 汇总\n%s\n\n-- 自动补货设置（021）\n%s\n\n-- 最近补货（021）\n%s\n' "$BATCHES" "$SUMMARY" "$RESTOCK_SETTINGS" "$RESTOCKS"
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
present=$("$psql_bin" "$SUPABASE_DB_URL" -X -q -t -A -v ON_ERROR_STOP=1 -c "$RESTOCK_PRESENT")
echo "== 自动补货设置（referral/restock.go；deploy/README.md §10）"
if [[ "${present%%|*}" == "t" ]]; then
    "$psql_bin" "$SUPABASE_DB_URL" -X -q -v ON_ERROR_STOP=1 -x -c "$RESTOCK_SETTINGS"
else
    echo "（referral_settings 没有 auto_restock_enabled 列：migration 021_referral_auto_restock.sql 还没跑）"
fi
echo "== 最近补货（code_restocks，新的在前）"
if [[ "${present##*|}" == "t" ]]; then
    "$psql_bin" "$SUPABASE_DB_URL" -X -q -v ON_ERROR_STOP=1 -c "$RESTOCKS"
else
    echo "（没有 code_restocks 表：migration 021_referral_auto_restock.sql 还没跑）"
fi
