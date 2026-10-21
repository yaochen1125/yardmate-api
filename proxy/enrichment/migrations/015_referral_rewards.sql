-- 015_referral_rewards.sql
-- 推荐奖励（yardmate-swiftui docs: main-navigation/more/invite-friends/rewards/rewards.md）。
-- 朋友（referrals.referee）每真扣一笔费，推荐人记 30%（按 RevenueCat 给的美元价 ×
-- takehome_percentage），过了苹果 90 天退款期才算可兑余额；满 $100 申请一张
-- Amazon.com 礼品卡，运营手动发。代码在 yardmate-api 的 rewards/（webhook、账本、兑换）。
--
-- 三张表，没有函数：记账是一条 insert（rc_event_id、transaction_id 各自唯一 = 去重；
-- 跳过 reward_voided_txns 里已退款的），兑换是 Go 侧一个事务
-- （锁该用户的 referral_users 行 + 下面的 partial unique index 兜底）。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 015_referral_rewards.sql，或 Supabase SQL Editor 整份跑。
-- 依赖 013_referral.sql（referral_users / referrals）。可重复执行。先 staging 后 prod，且先于二进制。

-- ---------------------------------------------------------------- 账本

-- 一行 = 朋友的一笔真实扣费（RevenueCat INITIAL_PURCHASE / RENEWAL，period_type NORMAL / INTRO，price > 0）。
--   pending    还在 90 天退款窗口里（available_at 未到）
--   available  过了窗口，算进可兑余额（结算轮每 10 分钟转一次）
--   voided     窗口内苹果退款（CANCELLATION + cancel_reason CUSTOMER_SUPPORT）；已 available 的不追回
--
-- 删账号：推荐人删 → 这些行跟着 referral_users 删掉（余额作废）；
-- 朋友删 → referrals 那行没了，这里 referee / referral_id 置空，推荐人已记的钱不动。
create table if not exists reward_ledger (
    id                      bigint generated always as identity primary key,
    referral_id             bigint      references referrals(id) on delete set null,
    referrer                text        not null references referral_users(user_id) on delete cascade,
    referee                 text        references referral_users(user_id) on delete set null,
    rc_event_id             text        not null,       -- RevenueCat event.id；RevenueCat 会重发，靠它去重
    event_type              text        not null check (event_type in ('INITIAL_PURCHASE','RENEWAL')),
    transaction_id          text        not null,       -- 退款按它找 pending
    original_transaction_id text,
    product_id              text        not null default '',
    period_type             text        not null check (period_type in ('NORMAL','INTRO')),
    price_usd_cents         int         not null check (price_usd_cents > 0),
    takehome_percentage     numeric(5,4) not null check (takehome_percentage > 0 and takehome_percentage <= 1),
    credit_cents            int         not null check (credit_cents > 0),  -- round(price × takehome × 0.30 × 100)
    purchased_at            timestamptz not null,
    available_at            timestamptz not null,       -- purchased_at + 90 天
    state                   text        not null default 'pending'
                            check (state in ('pending','available','voided')),
    created_at              timestamptz not null default now(),
    matured_at              timestamptz,
    voided_at               timestamptz
);
create unique index if not exists reward_ledger_rc_event_uidx    on reward_ledger (rc_event_id);
create index        if not exists reward_ledger_referrer_state_idx on reward_ledger (referrer, state);
-- 一笔 App Store 交易只记一次：rc_event_id 只挡同一事件的重发，这条再挡「同一笔交易换了 event.id
-- 又来一次」。退款也按它找 pending。（本迁移早期草稿是同列的非唯一索引，有就删掉，唯一索引已覆盖查找。）
create unique index if not exists reward_ledger_transaction_uidx on reward_ledger (transaction_id);
drop index          if exists     reward_ledger_transaction_idx;
-- 结算轮只扫还没入账的。
create index        if not exists reward_ledger_due_idx          on reward_ledger (available_at) where state = 'pending';

-- ---------------------------------------------------------------- 退款先于扣费到达

-- 苹果退款（CANCELLATION + CUSTOMER_SUPPORT）作废时账本里没有这笔的 pending（扣费那次投递失败、
-- RevenueCat 稍后才重试成功，退款反而先到）→ 把 transaction_id 记在这里。之后扣费事件到了，
-- 记账 insert 带 where not exists 这张表，不入账、答 200 voided。没命中 pending 的退款都记
-- （已 available / 不是朋友的也记，无害）。只有交易号和时间，不关联用户，删账号不用管它。
create table if not exists reward_voided_txns (
    transaction_id text        primary key,
    voided_at      timestamptz not null default now()
);

-- ---------------------------------------------------------------- 兑换申请

-- 一行 = 一张 $100 Amazon.com 礼品卡的申请。
--   requested  已申请，等运营买卡发码（deploy/referral-payout.sh mark-sent <id>）
--   sent       已发
--   cancelled  运营取消（deploy/referral-payout.sh cancel <id>）；钱自然回到余额
-- 可兑余额 = sum(available 账本) - sum(requested + sent 申请)，所以 cancelled 不用回写任何东西。
create table if not exists reward_payouts (
    id           bigint generated always as identity primary key,
    user_id      text        not null references referral_users(user_id) on delete cascade,
    amount_cents int         not null default 10000 check (amount_cents > 0),
    email        text        not null,
    state        text        not null default 'requested'
                 check (state in ('requested','sent','cancelled')),
    requested_at timestamptz not null default now(),
    sent_at      timestamptz,
    cancelled_at timestamptz
);
create index        if not exists reward_payouts_user_state_idx      on reward_payouts (user_id, state);
-- 同一个人同时只能有一张在路上（并发两次 /payout 的兜底；Go 侧先锁行再查）。
create unique index if not exists reward_payouts_one_requested_uidx on reward_payouts (user_id) where state = 'requested';

-- RLS 开、不加任何 policy：anon / authenticated 一行也看不到。只有后端
-- （DB 密码 pgx 池 / service_role，deploy/referral-payout.sh 走 PostgREST + service role）能读写。
alter table reward_ledger      enable row level security;
alter table reward_payouts     enable row level security;
alter table reward_voided_txns enable row level security;

-- 让 PostgREST 重新读 schema（deploy/referral-payout.sh 经 PostgREST 读写 reward_payouts）。
notify pgrst, 'reload schema';
