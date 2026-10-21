-- 016_referral_antiabuse.sql
-- 推荐好友防刷（yardmate-swiftui docs: main-navigation/more/invite-friends/invite-friends.md
-- ### 防刷）。「送一个月」那份原来只有「填码每设备每 24 小时 5 次」，小号零成本可刷。这里加库里那几道：
--
--   第 1 道 DeviceCheck（一台设备一辈子只当一次朋友）—— bit 存在苹果那边，库里只多
--          referrals.claim_install_id（填码时的 X-Device-Install-Id，礼品卡审核邮件里数设备用）。
--   第 2 道 推荐人用过的设备 —— referral_devices：推荐人每次 /me 记一笔；朋友在这些设备上填
--          这个推荐人的码 → own_code（referral_claim_check / 三参数 referral_claim）。
--   第 3 道 同一个 Apple ID 只算一次 —— referrals.subscription_key + referral_used_subscriptions：
--          结算 ① 认定 redeemed 时记下朋友那条订阅的身份；没作废的推荐里已有同一个 → 后认定的
--          void（duplicate_apple_id），名下的钱一起作废（referral_mark_redeemed）。
--          ⚠️ 文档写的是苹果 original_transaction_id，但 RevenueCat REST v1 / v2 的订阅记录都不带它
--          （只有 webhook 带）。所以这里存的是 RevenueCat 订阅记录里能拿到的、同一条订阅链换账号 /
--          转移都不变的代替：`<store>:<original_purchase_date>`（referral/revenuecat.go SubscriptionKey）。
--   第 4 道 速度闸 —— referral_settings.velocity_limit / velocity_window_hours；结算 ② 判成功时
--          这个推荐人 velocity_window_hours 小时内已成功（含 held）≥ velocity_limit 条 → 这一条 held
--          （referral_qualify）。运营 deploy/referral-review.sh approve / reject（referral_approve /
--          referral_reject）。held 算进 /me 的 months_pending（referral_me 重写）。
--   第 5 道 礼品卡人工审核 —— reward_ledger.void_reason（区分「退款作废」和「推荐作废连带」），
--          申请邮件里数退款笔数用。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 016_referral_antiabuse.sql，或 Supabase SQL Editor 整份跑。
-- 依赖 013 / 014 / 015（会改 015 的 reward_ledger）。可重复执行。先 staging 后 prod，且先于二进制
-- （新二进制调三参数 referral_claim / 两参数 referral_me 等；旧二进制仍调 013/014 的两参数
-- referral_claim / 一参数 referral_me，都还在，回滚二进制不用回滚库）。

-- ---------------------------------------------------------------- 设置

alter table referral_settings
    add column if not exists velocity_limit        int not null default 5
        check (velocity_limit >= 0),         -- 一个推荐人 velocity_window_hours 内成功超过这个数，后面的进 held；0 = 关
    add column if not exists velocity_window_hours int not null default 24
        check (velocity_window_hours > 0);   -- 速度闸的窗口（按 qualified_at 数）

-- ---------------------------------------------------------------- referrals

-- 状态多一个 held（待审核，第 4 道）：
--   claimed → redeemed → qualified → (held →) (rewarded | saved → rewarded)，任一步可 void。
-- 013 的 check 是列上内联写的，名字是 Postgres 默认的 referrals_state_check。
alter table referrals drop constraint if exists referrals_state_check;
alter table referrals add constraint referrals_state_check
    check (state in ('claimed','redeemed','qualified','held','saved','rewarded','void'));

-- void_reason（013 就有，无 check）：refunded / no_subscription / duplicate_apple_id / rejected。
alter table referrals
    add column if not exists subscription_key  text,         -- 第 3 道：朋友那条订阅的身份（见文件头），结算 ① 记
    add column if not exists claim_install_id  text,         -- 填码时的 X-Device-Install-Id（小写），新填码才记
    add column if not exists reviewed_at       timestamptz,  -- 运营 approve / reject 的时间
    add column if not exists held_notified_at  timestamptz;  -- 这条 held 已经发过运营邮件

-- 没作废的推荐里同一个订阅身份只能有一条（referral_mark_redeemed 先查再写，这条兜底）。
create unique index if not exists referrals_subscription_key_uidx
    on referrals (subscription_key)
 where subscription_key is not null and state <> 'void';
-- 速度闸按推荐人 + qualified_at 数。
create index if not exists referrals_referrer_qualified_idx on referrals (referrer, qualified_at);

-- ---------------------------------------------------------------- 第 2 道：推荐人用过的设备

-- /v1/referral/me 每次成功记一笔（install_id 小写）。只用来判「朋友在推荐人自己的设备上填他的码」。
-- 删账号随 referral_users 级联删。
create table if not exists referral_devices (
    user_id       text        not null references referral_users(user_id) on delete cascade,
    install_id    text        not null,
    first_seen_at timestamptz not null default now(),
    last_seen_at  timestamptz not null default now(),
    primary key (user_id, install_id)
);
-- referral_claim 按 (码主人, 设备) 查，主键已覆盖。

-- ---------------------------------------------------------------- 第 3 道：删账号后留下的订阅身份

-- 删账号时（referral/pgstore.go DeleteUser）把这个人两侧没作废推荐的 subscription_key 存一份：
-- 只有身份串和时间，不带用户 ID，免得删号重来同一个 Apple ID 再算一次。
create table if not exists referral_used_subscriptions (
    subscription_key text        primary key,
    first_used_at    timestamptz not null default now()
);

-- ---------------------------------------------------------------- 第 5 道：账本作废原因

-- refunded（苹果退款，rewards webhook）/ duplicate_apple_id / rejected（推荐作废连带，referral_void）。
-- 016 之前作废的只可能是退款。
alter table reward_ledger add column if not exists void_reason text;
update reward_ledger set void_reason = 'refunded' where state = 'voided' and void_reason is null;

alter table referral_devices            enable row level security;
alter table referral_used_subscriptions enable row level security;

-- ---------------------------------------------------------------- GET /v1/referral/me

-- 与 013 同名同签名整体替换：months_pending 含 held（推荐人看不出审核，文档拍板）。旧二进制也受益。
create or replace function referral_me(p_user text) returns json
language plpgsql as $$
declare
    v_code    text;
    v_invited int;
    v_earned  int;
    v_pending int;
    v_saved   int;
    v_claimed boolean;
begin
    select code into v_code from referral_users where user_id = p_user;
    if v_code is null then
        insert into referral_users (user_id, code) values (p_user, referral_new_code())
            on conflict (user_id) do nothing;
        select code into v_code from referral_users where user_id = p_user;
    end if;

    select count(*) filter (where state <> 'void'),
           count(*) filter (where state in ('rewarded','saved')),
           count(*) filter (where state in ('redeemed','qualified','held')),
           count(*) filter (where state = 'saved')
      into v_invited, v_earned, v_pending, v_saved
      from referrals where referrer = p_user;

    select exists (select 1 from referrals where referee = p_user) into v_claimed;

    return json_build_object(
        'code',           v_code,
        'invited',        coalesce(v_invited, 0),
        'months_earned',  coalesce(v_earned, 0),
        'months_pending', coalesce(v_pending, 0),
        'months_saved',   coalesce(v_saved, 0),
        'can_claim',      not v_claimed
    );
end;
$$;

-- 新二进制调这个：同上，再记下这次用的设备（第 2 道）。
create or replace function referral_me(p_user text, p_install text) returns json
language plpgsql as $$
declare
    v_out     json;
    v_install text := lower(nullif(btrim(coalesce(p_install, '')), ''));
begin
    v_out := referral_me(p_user);   -- 第一次调用在这里造码（也造出 referral_users 那一行，下面的 FK 要它）
    if v_install is not null then
        insert into referral_devices (user_id, install_id) values (p_user, v_install)
            on conflict (user_id, install_id) do update set last_seen_at = now();
    end if;
    return v_out;
end;
$$;

-- ---------------------------------------------------------------- POST /v1/referral/claim

-- 判定顺序 2–4 的预检（只读，不写任何东西）：Go 侧先问它，再决定要不要查 RevenueCat 和
-- DeviceCheck（判定顺序 5、6），最后调下面的三参数 referral_claim 真领（它把 2–4 原子地再判一遍）。
--   'again' = 这个账号填过同一个码（重领：不查 RevenueCat、不查 DeviceCheck）
--   'new'   = 第一次填
-- 拒绝照旧 raise exception：unknown_code / already_claimed / own_code。
create or replace function referral_claim_check(p_user text, p_code text, p_install text) returns text
language plpgsql as $$
declare
    v_referrer text;
    v_prior    text;
    v_install  text := lower(nullif(btrim(coalesce(p_install, '')), ''));
begin
    -- 2. 码认不出
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;
    -- 3. 这个账号填过
    select referrer into v_prior from referrals where referee = p_user;
    if v_prior is not null then
        if v_prior <> v_referrer then
            raise exception 'already_claimed';
        end if;
        return 'again';
    end if;
    -- 4. 码是自己的，或当前设备在码主人记过的设备里
    if v_referrer = p_user
       or (v_install is not null and exists (
               select 1 from referral_devices where user_id = v_referrer and install_id = v_install)) then
        raise exception 'own_code';
    end if;
    return 'new';
end;
$$;

-- 三参数版：014 的两参数版 + 判定顺序 3 在 4 之前 + 设备那半个 own_code + 记 claim_install_id +
-- 作废的推荐不再换码 + 回答多一个 fresh（这次是否新建了推荐 = Go 侧要不要置 DeviceCheck bit0）。
-- Go 在一个事务里调它、置 bit0、成功才 commit（置位失败回滚 → 码池那张码仍是 free）。
-- 两参数版保留给旧二进制（回滚用）。
create or replace function referral_claim(p_user text, p_code text, p_install text) returns json
language plpgsql as $$
declare
    v_referrer    text;
    v_install     text := lower(nullif(btrim(coalesce(p_install, '')), ''));
    v_ref         referrals%rowtype;
    v_fresh       boolean := false;
    v_offer       text;
    v_state       text;
    v_expires     timestamptz;
    v_margin      int;
    v_grace       int;
    v_cutoff      timestamptz;   -- 新发的码到期日必须晚于这个
    v_swap_before timestamptz;   -- 手上那张到期日早于这个才换
begin
    -- 2. 码认不出
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;

    select issue_margin_days, swap_grace_days into v_margin, v_grace
      from referral_settings where id = 1;
    v_cutoff      := now() + make_interval(days => coalesce(v_margin, 14));
    v_swap_before := now() + make_interval(days => coalesce(v_grace, 3));

    select * into v_ref from referrals where referee = p_user;
    if v_ref.id is not null then
        -- 3. 这个账号填过：别的码 → already_claimed；同一个码 → 重领（下面）。
        if v_ref.referrer <> v_referrer then
            raise exception 'already_claimed';
        end if;
        -- 锁住手上那张，免得和结算的 referral_mark_redeemed / ExpireCodes 交叉。
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code
           for update;
        -- 已兑换的永远原样回；issued 且还有宽限天数以上的也原样回。
        if v_ref.offer_code is not null
           and (v_state = 'redeemed' or (v_state = 'issued' and v_expires > v_swap_before)) then
            return json_build_object('offer_code', v_ref.offer_code, 'fresh', false);
        end if;
        -- 作废了的推荐（同一 Apple ID / 被拒 / 退款）不再换码：天天重领会把好码一张张烧掉。
        -- 手上有老码就回老码，没有就当填过。
        if v_ref.state = 'void' then
            if v_ref.offer_code is not null then
                return json_build_object('offer_code', v_ref.offer_code, 'fresh', false);
            end if;
            raise exception 'already_claimed';
        end if;
        -- 走到这里 = 想换：老码过期了 / 快过期了 / 状态不对 / 行没了。先选替换码，选到了才动老码。
    else
        -- 4. 码是自己的，或当前设备在码主人记过的设备里（第 2 道）
        if v_referrer = p_user
           or (v_install is not null and exists (
                   select 1 from referral_devices where user_id = v_referrer and install_id = v_install)) then
            raise exception 'own_code';
        end if;
        -- B 也要有自己的一行（FK），顺手给他造码。
        insert into referral_users (user_id, code) values (p_user, referral_new_code())
            on conflict (user_id) do nothing;
        begin
            insert into referrals (referrer, referee, claim_install_id)
                 values (v_referrer, p_user, v_install)
              returning * into v_ref;
        exception when unique_violation then
            -- 同一瞬间两台设备都在填码。晚到的那个看到的就是「已经领过了」。
            raise exception 'already_claimed';
        end;
        v_fresh := true;
    end if;

    -- 7. 余量之外的里面先到期的先发；skip locked：另一台手机正在拿的那张直接跳过。
    select code into v_offer from code_pool
     where state = 'free' and expires_at > v_cutoff
     order by expires_at
       for update skip locked
     limit 1;
    if v_offer is null then
        -- 没有留够余量的替换码。老码在苹果那边还能用就先给老码；老码也不能用了才算没货。
        -- 首次 claim 的 raise 会回滚上面插的 referral（不置 bit0：Go 侧只在拿到码之后才置）。
        if v_ref.offer_code is not null and v_state = 'issued' and v_expires > now() then
            return json_build_object('offer_code', v_ref.offer_code, 'fresh', false);
        end if;
        raise exception 'pool_empty';
    end if;

    -- 换码：老码划 expired，reissued_at 记到现在；claimed_at 不动（见 014）。
    if v_ref.offer_code is not null and v_state = 'issued' then
        update code_pool set state = 'expired' where code = v_ref.offer_code;
    end if;

    update code_pool
       set state = 'issued', issued_to = p_user, issued_at = now(), referral_id = v_ref.id
     where code = v_offer;
    update referrals
       set offer_code  = v_offer,
           reissued_at = case when v_ref.offer_code is not null then now() else reissued_at end
     where id = v_ref.id;

    return json_build_object('offer_code', v_offer, 'fresh', v_fresh);
end;
$$;

-- ---------------------------------------------------------------- 作废

-- 一条推荐作废。refunded / no_subscription（结算 ②）只动推荐本身——退款那笔钱 webhook 自己会作废；
-- duplicate_apple_id（第 3 道）/ rejected（第 4 道 reject、礼品卡审核）连带把这条推荐名下
-- pending + available 的钱一起标 voided（rewards.md ### 防刷）。
-- rejected 什么状态都能作废（礼品卡审核时可能早就 rewarded 了；RevenueCat 那边已赠的不收回）；
-- 其他原因只作废还没发出去的（claimed / redeemed / qualified / held）。
-- 回 true = 这次作废了；false = 本来就 void / 状态不让。
create or replace function referral_void(p_id bigint, p_reason text) returns boolean
language plpgsql as $$
declare
    v_n int;
begin
    update referrals set state = 'void', void_reason = p_reason
     where id = p_id
       and state <> 'void'
       and (p_reason = 'rejected' or state in ('claimed','redeemed','qualified','held'));
    get diagnostics v_n = row_count;
    if v_n = 0 then
        return false;
    end if;
    if p_reason in ('duplicate_apple_id', 'rejected') then
        update reward_ledger
           set state = 'voided', voided_at = now(), void_reason = p_reason
         where referral_id = p_id and state in ('pending', 'available');
    end if;
    return true;
end;
$$;

-- ---------------------------------------------------------------- 结算 ①：认定兑换 + 第 3 道

-- claimed → redeemed（redeemed_at = 订阅真实开始时间，code_pool 那张划 redeemed），同时记下
-- subscription_key。同一个身份已被另一条没作废的推荐用过，或在删账号留下的表里 → 这条 void
-- （duplicate_apple_id，名下的钱一起作废），不进 redeemed。
-- 回 'redeemed' / 'void' / 'skipped'（不是 claimed 了，什么都不做）。
-- 同一个身份的认定用 advisory lock 串行，「先查后写」不会两条都过；上面的 partial unique index 兜底。
create or replace function referral_mark_redeemed(p_id bigint, p_at timestamptz, p_key text) returns text
language plpgsql as $$
declare
    v_key   text := nullif(btrim(coalesce(p_key, '')), '');
    v_state text;
begin
    if v_key is not null then
        perform pg_advisory_xact_lock(hashtextextended('referral_sub:' || v_key, 0));
    end if;
    select state into v_state from referrals where id = p_id for update;
    if v_state is distinct from 'claimed' then
        return 'skipped';
    end if;
    if v_key is not null and (
           exists (select 1 from referrals
                    where subscription_key = v_key and state <> 'void' and id <> p_id)
        or exists (select 1 from referral_used_subscriptions where subscription_key = v_key)) then
        perform referral_void(p_id, 'duplicate_apple_id');
        -- 先 void 再记身份：void 的行不在唯一索引里。留着身份方便对账。
        update referrals set subscription_key = v_key where id = p_id;
        return 'void';
    end if;
    update referrals set state = 'redeemed', redeemed_at = p_at, subscription_key = v_key
     where id = p_id;
    update code_pool set state = 'redeemed', redeemed_at = p_at
     where referral_id = p_id and state = 'issued';
    return 'redeemed';
end;
$$;

-- ---------------------------------------------------------------- 结算 ②：判成功 + 第 4 道

-- redeemed → qualified，或者（这个推荐人 velocity_window_hours 内已成功的——qualified / held /
-- saved / rewarded，按 qualified_at——已经 ≥ velocity_limit 条）→ held。两种都记 qualified_at = now()，
-- 所以 held 自己也算进后面的计数（文档：含待审核的）。velocity_limit = 0 = 闸关。
-- 锁推荐人那一行把同一推荐人的判定排队（NO KEY UPDATE 不挡 referrals 插 FK）。
-- 回 'qualified' / 'held' / 'skipped'（不是 redeemed 了）。
create or replace function referral_qualify(p_id bigint) returns text
language plpgsql as $$
declare
    v_referrer text;
    v_limit    int;
    v_window   int;
    v_recent   int;
    v_to       text := 'qualified';
begin
    select referrer into v_referrer from referrals where id = p_id and state = 'redeemed';
    if v_referrer is null then
        return 'skipped';
    end if;
    perform 1 from referral_users where user_id = v_referrer for no key update;

    select velocity_limit, velocity_window_hours into v_limit, v_window
      from referral_settings where id = 1;
    v_limit  := coalesce(v_limit, 5);
    v_window := coalesce(v_window, 24);
    if v_limit > 0 then
        select count(*) into v_recent from referrals
         where referrer = v_referrer and id <> p_id
           and state in ('qualified','held','saved','rewarded')
           and qualified_at > now() - make_interval(hours => v_window);
        if v_recent >= v_limit then
            v_to := 'held';
        end if;
    end if;

    update referrals set state = v_to, qualified_at = now()
     where id = p_id and state = 'redeemed';
    return v_to;
end;
$$;

-- ---------------------------------------------------------------- 运营审核（deploy/referral-review.sh）

-- approve：held → qualified，记 reviewed_at。下一轮结算 ③ 照常发 / 攒（③ 不过闸，不会再被闸住）。
create or replace function referral_approve(p_id bigint) returns json
language plpgsql as $$
declare
    v_n int;
begin
    update referrals set state = 'qualified', reviewed_at = now()
     where id = p_id and state = 'held';
    get diagnostics v_n = row_count;
    return json_build_object('id', p_id, 'approved', v_n = 1);
end;
$$;

-- reject：任何没作废的状态 → void（rejected），名下的钱一起作废，记 reviewed_at。
-- previous_state = rewarded 时月份已经在 RevenueCat 赠出去了，不收回（脚本会提示）。
create or replace function referral_reject(p_id bigint) returns json
language plpgsql as $$
declare
    v_prev text;
    v_ok   boolean := false;
begin
    select state into v_prev from referrals where id = p_id for update;
    if v_prev is not null and v_prev <> 'void' then
        v_ok := referral_void(p_id, 'rejected');
        update referrals set reviewed_at = now() where id = p_id;
    end if;
    return json_build_object('id', p_id, 'rejected', v_ok, 'previous_state', v_prev);
end;
$$;

-- ---------------------------------------------------------------- 谁能调

-- 同 013：PostgREST 把 rpc 暴露在公网上，EXECUTE 只留给 service_role（审核脚本走 PostgREST +
-- service role；referral_reject 以调用者身份调 referral_void，所以 service_role 也要它）。
-- 后端走 pgx（postgres 角色，函数的 owner）不受影响。
revoke execute on function referral_me(text)                                 from public, anon, authenticated;
revoke execute on function referral_me(text, text)                           from public, anon, authenticated;
revoke execute on function referral_claim_check(text, text, text)            from public, anon, authenticated;
revoke execute on function referral_claim(text, text, text)                  from public, anon, authenticated;
revoke execute on function referral_void(bigint, text)                       from public, anon, authenticated;
revoke execute on function referral_mark_redeemed(bigint, timestamptz, text) from public, anon, authenticated;
revoke execute on function referral_qualify(bigint)                          from public, anon, authenticated;
revoke execute on function referral_approve(bigint)                          from public, anon, authenticated;
revoke execute on function referral_reject(bigint)                           from public, anon, authenticated;
grant  execute on function referral_me(text)                                 to service_role;
grant  execute on function referral_me(text, text)                           to service_role;
grant  execute on function referral_claim_check(text, text, text)            to service_role;
grant  execute on function referral_claim(text, text, text)                  to service_role;
grant  execute on function referral_void(bigint, text)                       to service_role;
grant  execute on function referral_approve(bigint)                          to service_role;
grant  execute on function referral_reject(bigint)                           to service_role;

-- 让 PostgREST 重新读 schema（deploy/referral-review.sh 经 PostgREST 读 referrals、调 rpc）。
notify pgrst, 'reload schema';
