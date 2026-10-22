-- 021_referral_auto_restock.sql
-- 推荐好友码池自动补货（yardmate-swiftui docs: invite-friends.md ### 码池自动补货），默认关。
--
-- 码池（code_pool）是苹果一次性兑换码的库存，原来只能运营在 App Store Connect 手动生成、
-- 导出 CSV、用 deploy/supabase/import-codes.sh 导入。这里加一条自动补货：结算每轮看库存那一步
-- （referral/pool.go → referral/restock.go），开关开着时满足任一条就补一批：
--   * 可发（free 且到期日在 issue_margin_days 余量之外）< restock_below；
--   * 最早一批可发码 restock_lead_days 天内到期，且它到期后还能发的 < restock_below。
-- 补一批 = 调 App Store Connect API 给 offer「Referral Friend Month」生成 restock_batch_size 张
-- 一次性码（到期日 = 当天 UTC + 179 天），马上记下苹果给的批次 ID，再取码值灌进 code_pool
-- （offer_ref_name = 'Referral Friend Month'，库里已有的码忽略，同 import-codes.sh）。
--
-- 两样东西：
--   1. referral_settings 加六列：开关 + 触发线 + 批量 + 每天上限 + 提前天数 + 码环境。改库即生效。
--      restock_environment 迁移后为 null，各环境手动填一次（prod 填 'PRODUCTION'、staging 填
--      'SANDBOX'），必须和服务端 secrets 的 ASC_OFFER_CODE_ENVIRONMENT 一致才补。
--   2. code_restocks：每次补货一行。
--        creating  正在向苹果要码（或请求发出后结果不确定、正在对账）
--        created   苹果已给批次（asc_batch_id），码还没灌完
--        imported  已灌进码池（imported_count = 实际新进池的张数）
--        failed    失败（error 写原因）
--      creating / created 同时最多一条（partial unique index：两个进程同时发起只放行一个）；
--      asc_batch_id 唯一（对账时一批码不会被两次补货认领）。
--
-- 开关关着（默认）时后端不调 App Store Connect，码池和告警与改版前完全一样；手动补货的
-- import-codes.sh 不变。
--
-- 依赖 013（referral_settings、code_pool）、014（发码余量那几列）。
-- 用法：psql "$SUPABASE_DB_URL" -f 021_referral_auto_restock.sql，或 Supabase SQL Editor 整份跑。
-- 可重复执行。先 staging 后 prod，且先于二进制（新二进制每轮读这几列；没跑 021 时新二进制
-- 把自动补货当作关、只记一行日志，结算其它步照跑）。

-- ---------------------------------------------------------------- 设置

alter table referral_settings
    add column if not exists auto_restock_enabled boolean not null default false,  -- 总开关；false = 不调苹果
    add column if not exists restock_below        int     not null default 300
        check (restock_below >= 0),                    -- 可发 / 最早一批到期后可发 少于这个数 → 补
    add column if not exists restock_batch_size   int     not null default 1000
        check (restock_batch_size between 1 and 25000), -- 每批张数；生成 PRODUCTION 码时后端再夹到 500–25,000
    add column if not exists restock_daily_max    int     not null default 2
        check (restock_daily_max >= 0),                -- 每个 UTC 日最多发起几次
    add column if not exists restock_lead_days    int     not null default 45
        check (restock_lead_days >= 0),                -- 最早一批可发码在这么多天内到期就提前补
    add column if not exists restock_environment  text
        check (restock_environment in ('PRODUCTION', 'SANDBOX'));  -- null = 没填，拒绝补货

-- ---------------------------------------------------------------- 补货记录

create table if not exists code_restocks (
    id              bigint generated always as identity primary key,
    environment     text        not null check (environment in ('PRODUCTION', 'SANDBOX')),
    number_of_codes int         not null check (number_of_codes between 1 and 25000),
    expires_at      timestamptz not null,          -- 苹果批次的到期日，记成当天 00:00 UTC（同 import-codes.sh）
    asc_batch_id    text        unique,            -- 苹果给的批次 ID；creating 时为 null
    state           text        not null default 'creating'
                    check (state in ('creating', 'created', 'imported', 'failed')),
    imported_count  int,                           -- 实际新进 code_pool 的张数（库里已有的不算）
    error           text,                          -- failed 的原因
    started_at      timestamptz not null default now(),  -- 发起时间：1 小时间隔、每天上限、30 分钟 / 6 小时超时都按它算
    finished_at     timestamptz
);

-- 没收尾的补货（creating / created）同时最多一条。
create unique index if not exists code_restocks_one_open_uidx
    on code_restocks ((true)) where state in ('creating', 'created');
create index if not exists code_restocks_started_idx on code_restocks (started_at);

-- RLS 开、不加任何 policy（同 013）：anon / authenticated 一行也看不到，只有后端
-- （DB 密码 pgx 池 / service_role）能读写。
alter table code_restocks enable row level security;

notify pgrst, 'reload schema';
