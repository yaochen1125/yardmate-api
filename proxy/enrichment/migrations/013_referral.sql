-- 013_referral.sql
-- 推荐好友（yardmate-swiftui docs: main-navigation/more/invite-friends）。
-- A 分享邀请码 → B 填码得一张 App Store 一次性兑换码（1 个月免费 premium.month）
-- → B 兑换满 hold_days 天 → A 得 1 个月（走 RevenueCat 赠送会员，不限量叠加；
-- A 正在付费时先攒着，付费订阅结束后一次发出）。
--
-- 五张表 + 三个函数。原子性要求高的两步（造码、领兑换码）放在库里的函数里：
-- 两台手机同时来领码池里最后一张码，靠应用层读一遍再写一遍保证不了。结算
-- （查 RevenueCat → 推进状态 → 赠送）由 yardmate-api 的定时器用普通语句驱动
-- （referral/settle.go）。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 013_referral.sql，或 Supabase SQL Editor 整份跑。
-- 可重复执行。先 staging 后 prod。

-- ---------------------------------------------------------------- 表

-- 邀请码。user_id = Supabase auth user id（JWT sub，小写 uuid 字符串）。
-- 一个用户第一次调 /v1/referral/me 时才造码，从不提前。
create table if not exists referral_users (
    user_id    text        primary key,
    code       text        not null unique,     -- YM-XXXX，字母表去掉 0/O/1/I/L
    created_at timestamptz not null default now()
);

-- 推荐记录：A（referrer）→ B（referee）。一个人一辈子只能被推荐一次。
--   claimed    B 填了码、拿到了兑换码（offer_code）
--   redeemed   RevenueCat 上查到 B 的订阅开始于填码之后（redeemed_at = 订阅开始时间）
--   qualified  兑换满 hold_days 天且没退款 —— 这次推荐成功，A 记 +1 个月
--   saved      A 正在付费，这 1 个月先攒着
--   rewarded   已经通过 RevenueCat 赠给 A
--   void       作废（B 退款）
create table if not exists referrals (
    id                 bigint generated always as identity primary key,
    referrer           text not null references referral_users(user_id) on delete cascade,
    referee            text not null references referral_users(user_id) on delete cascade,
    state              text not null default 'claimed'
                       check (state in ('claimed','redeemed','qualified','saved','rewarded','void')),
    claimed_at         timestamptz not null default now(),
    offer_code         text,              -- 当前发给 B 的兑换码（code_pool.code）；过期会换
    redeem_reported_at timestamptz,       -- App 调 /redeemed 的时间（只是提醒，不作凭据）
    redeemed_at        timestamptz,
    qualified_at       timestamptz,
    rewarded_at        timestamptz,
    void_reason        text,
    unique (referee),
    check (referrer <> referee)
);
create index if not exists referrals_referrer_idx on referrals (referrer);
create index if not exists referrals_state_idx    on referrals (state);

-- 兑换码池：App Store Connect 生成的一次性 offer code（offer「Referral Friend Month」，
-- 挂在 com.chenyao.plantapp.premium.month 上）。是库存：会用完，也会过期（最长 6 个月）。
-- 补货：deploy/supabase/import-codes.sh。
create table if not exists code_pool (
    code           text        primary key,
    offer_ref_name text        not null,     -- 那批码在 App Store Connect 里的 reference name
    state          text        not null default 'free'
                   check (state in ('free','issued','redeemed','expired')),
    expires_at     timestamptz not null,     -- Apple 那边的过期日
    issued_to      text        references referral_users(user_id) on delete set null,
    issued_at      timestamptz,
    referral_id    bigint      references referrals(id) on delete set null,
    redeemed_at    timestamptz,
    imported_at    timestamptz not null default now()
);
create index if not exists code_pool_stock_idx on code_pool (state, expires_at);

-- 推荐人奖励账本：已赠月数 + 我们记的 RevenueCat 赠送到期日（叠加的基准）。
-- 攒着的月数不单独存，= referrals 里 state='saved' 的条数。
create table if not exists referral_rewards (
    user_id        text        primary key references referral_users(user_id) on delete cascade,
    months_granted int         not null default 0,
    promo_until    timestamptz,
    updated_at     timestamptz not null default now()
);

-- 能改而不用发版的数。一行。
create table if not exists referral_settings (
    id        int primary key default 1 check (id = 1),
    hold_days int not null default 7    -- B 兑换后要满几天，A 才拿得到
);
insert into referral_settings (id) values (1) on conflict (id) do nothing;

-- RLS 开、不加任何 policy：anon / authenticated 一行也看不到。只有后端
-- （DB 密码 pgx 池 / service_role）能读写。
alter table referral_users    enable row level security;
alter table referrals         enable row level security;
alter table code_pool         enable row level security;
alter table referral_rewards  enable row level security;
alter table referral_settings enable row level security;

-- ---------------------------------------------------------------- 造码

create or replace function referral_new_code() returns text
language plpgsql as $$
declare
    -- 31 个字符，去掉 0/O 和 1/I/L。与 referral/code.go 的 alphabet 保持一致。
    alphabet constant text := '23456789ABCDEFGHJKMNPQRSTUVWXYZ';
    raw bytea;
    out text;
begin
    for attempt in 1..20 loop
        -- gen_random_uuid 底下是 pg_strong_random，不是 random()。
        raw := decode(replace(gen_random_uuid()::text, '-', ''), 'hex');
        out := 'YM-';
        for i in 0..3 loop
            out := out || substr(alphabet, 1 + (get_byte(raw, i) % 31), 1);
        end loop;
        if not exists (select 1 from referral_users where code = out) then
            return out;
        end if;
    end loop;
    raise exception 'code_exhausted';
end;
$$;

-- ---------------------------------------------------------------- GET /v1/referral/me

-- 也是造码的地方。can_claim 这里只看「有没有填过」；「服务端查到订阅记录」那一半
-- 由 Go 侧问 RevenueCat 之后再压下去。
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
           count(*) filter (where state in ('redeemed','qualified')),
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

-- ---------------------------------------------------------------- POST /v1/referral/claim

-- p_code 已由 Go 归一化成 YM-XXXX。拒绝用 raise exception（P0001 + 错误码做 message），
-- Go 侧把它映射成 HTTP 400/503；raise 同时回滚本函数里的所有写入。
create or replace function referral_claim(p_user text, p_code text) returns json
language plpgsql as $$
declare
    v_referrer text;
    v_ref      referrals%rowtype;
    v_offer    text;
    v_state    text;
    v_expires  timestamptz;
begin
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;
    if v_referrer = p_user then
        raise exception 'own_code';
    end if;

    -- B 也要有自己的一行（FK），顺手给他造码。
    insert into referral_users (user_id, code) values (p_user, referral_new_code())
        on conflict (user_id) do nothing;

    select * into v_ref from referrals where referee = p_user;
    if v_ref.id is not null then
        -- 换了个人的码再来：不行，一个人只能被推荐一次。
        if v_ref.referrer <> v_referrer then
            raise exception 'already_claimed';
        end if;
        -- 同一个人、同一个码再来：把上次那张兑换码再给他（上次可能拿到码之后 App 崩了）。
        -- 那张过期了就划掉、换一张新的。
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code;
        if v_ref.offer_code is not null and v_state is not null
           and (v_state = 'redeemed' or v_expires > now()) then
            return json_build_object('offer_code', v_ref.offer_code);
        end if;
        if v_ref.offer_code is not null and v_state = 'issued' then
            update code_pool set state = 'expired' where code = v_ref.offer_code;
        end if;
    else
        begin
            insert into referrals (referrer, referee) values (v_referrer, p_user)
                returning * into v_ref;
        exception when unique_violation then
            -- 同一瞬间两台设备都在填码。晚到的那个看到的就是「已经领过了」。
            raise exception 'already_claimed';
        end;
    end if;

    -- 放坏在货架上的先划掉，免得下面挑到一张不能用的。
    update code_pool set state = 'expired'
     where state = 'free' and expires_at <= now();

    -- 先到期的先发；skip locked：另一台手机正在拿的那张直接跳过。
    select code into v_offer from code_pool
     where state = 'free' and expires_at > now()
     order by expires_at
       for update skip locked
     limit 1;
    if v_offer is null then
        raise exception 'pool_empty';
    end if;

    update code_pool
       set state = 'issued', issued_to = p_user, issued_at = now(), referral_id = v_ref.id
     where code = v_offer;
    update referrals set offer_code = v_offer where id = v_ref.id;

    return json_build_object('offer_code', v_offer);
end;
$$;

-- ---------------------------------------------------------------- 谁能调

-- Postgres 建函数时默认把 EXECUTE 授给 PUBLIC，而 PostgREST 把 rpc 暴露在公网上。
-- 不收回来，任何人拿着 anon key 就能自己给自己领码。后端走 pgx（postgres 角色）不受影响。
revoke execute on function referral_new_code()        from public, anon, authenticated;
revoke execute on function referral_me(text)          from public, anon, authenticated;
revoke execute on function referral_claim(text, text) from public, anon, authenticated;
grant  execute on function referral_me(text)          to service_role;
grant  execute on function referral_claim(text, text) to service_role;

-- 让 PostgREST 重新读 schema（import-codes.sh 经 PostgREST 写 code_pool）。
notify pgrst, 'reload schema';
