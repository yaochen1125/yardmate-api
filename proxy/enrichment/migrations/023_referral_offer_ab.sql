-- 023_referral_offer_ab.sql
-- 推荐好友：朋友免费月分月付组 / 年付组做实验（yardmate-swiftui docs: invite-friends.md
-- ### 兑换码分组实验（月付组 / 年付组））。Go 侧同 PR（referral/SPEC.md）。
--
--   ① 设置：referral_settings.offer_year_pct（年付组占比 0–100，默认 0 = 全部月付）、
--      offer_ref_month / offer_ref_year（两组兑换码在 code_pool.offer_ref_name 里的名字）。
--   ② referrals.offer_variant：'month' / 'year'，朋友第一次领码成功时写死；老推荐为 null，一律算 month。
--   ③ referral_offer_variant(p_user)：按账号哈希出 0–99，小于 offer_year_pct → 'year'，否则 'month'。
--   ④ referral_claim（三参数，同名同签名整体替换 022 的版本）：只从本组那种码里取——年付组只取
--      offer_ref_name = offer_ref_year 的码，月付组取其余所有码（老码、手动导入的、名字不一样的都算月付，
--      向后兼容）；回答多带 offer_variant。
--      一组发完不借另一组（pool_empty）。其余规则一字不差照 022。
--   ⑤ referral_pending_offer（同名同签名整体替换 022 的版本）：回答多带 offer_variant。
--   ⑥ referral_switch_unredeemed(p_to, p_execute)：把新版 App 领的、领了码没兑（也没报过 /redeemed）、不在目标组的推荐
--      换成目标组的码，记 offer_variant_switched_at（这些推荐不进主统计）。
--   ⑦ referral_ab_stats()：按组统计（领码、兑换、付费、续费、退款、每个领码 / 每个兑换的
--      30 / 90 / 180 / 365 / 425 天实收）。只算领码那一刻占比在 1–99 之间（offer_year_pct_at_claim）、没被换过组的。
--   ⑧ referral_set_offer_year_pct(p_pct)：改年付组占比；第一次从 0 改成大于 0 时记 offer_ab_started_at。
--   ⑨ referral_ab_overview()：后台概况（比例、开始时间、两组库存、没兑人数、不认识的码名）。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 023_referral_offer_ab.sql，或 Supabase SQL Editor 整份跑。
-- 依赖 013–017、022。可重复执行。先 staging 后 prod，且先于二进制。函数签名没动（④⑤），旧二进制照常调：
-- offer_year_pct 默认 0 时所有人都进月付组、只取月付码，和改版前一样。
-- ⚠️ 023 之后若重跑 016 / 017 / 022，会把 ④⑤ 换回旧版 —— 重跑后要再跑一遍 023。

-- ---------------------------------------------------------------- ①② 设置 + 组别

alter table referral_settings
    add column if not exists offer_year_pct  int  not null default 0
        check (offer_year_pct between 0 and 100),                      -- 年付组占比；0 = 全部月付
    add column if not exists offer_ab_started_at timestamptz,          -- 第一次从 0 调成 >0 的时间；只做记录，统计不看它
    add column if not exists offer_ref_month text not null default 'Referral Friend Month',
    add column if not exists offer_ref_year  text not null default 'Referral Friend Year';

alter table referrals
    add column if not exists offer_variant text
        check (offer_variant in ('month', 'year')),                    -- null = 本功能上线前领的，算 month
    add column if not exists offer_variant_switched_at timestamptz,    -- 被一键换过组的时间（不进主统计）
    add column if not exists offer_year_pct_at_claim int;              -- 领码那一刻的年付组占比；主统计只算 1–99 之间领的

-- ---------------------------------------------------------------- ③ 分组

-- 同一个账号每次算出来一样：md5 前 8 个十六进制位当无符号 32 位数对 100 取余，不随 Postgres 版本变。
-- 只在第一次领码时调，结果写进 referrals。
create or replace function referral_offer_variant(p_user text) returns text
language plpgsql stable as $$
declare
    v_pct    int;
    v_bucket int;
begin
    select offer_year_pct into v_pct from referral_settings where id = 1;
    -- 老 App 包不认识组别（请求没带 offer_variant_aware）→ Go 侧在事务里设 referral.force_month = on，一律月付。
    if coalesce(current_setting('referral.force_month', true), '') = 'on' then
        return 'month';
    end if;
    -- 前 8 个十六进制位补到 16 位再转 64 位整数：恒为非负。
    v_bucket := (('x' || lpad(substr(md5(coalesce(p_user, '') || ':offer_ab'), 1, 8), 16, '0'))::bit(64)::bigint % 100)::int;
    if v_bucket < coalesce(v_pct, 0) then
        return 'year';
    end if;
    return 'month';
end;
$$;

-- ---------------------------------------------------------------- ④ POST /v1/referral/claim

-- 与 022 同名同签名整体替换。区别只有标了 023 的几处：组别、按组取码、回答带 offer_variant。
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
    v_variant     text;          -- 023
    v_ref_month   text;
    v_ref_year    text;          -- 023：年付组兑换码在码池里的名字
begin
    -- 2. 码认不出
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;

    select issue_margin_days, swap_grace_days, offer_ref_month, offer_ref_year
      into v_margin, v_grace, v_ref_month, v_ref_year
      from referral_settings where id = 1;
    v_cutoff      := now() + make_interval(days => coalesce(v_margin, 14));
    v_swap_before := now() + make_interval(days => coalesce(v_grace, 3));
    v_ref_month   := coalesce(v_ref_month, 'Referral Friend Month');
    v_ref_year    := coalesce(v_ref_year, 'Referral Friend Year');

    select * into v_ref from referrals where referee = p_user for update;
    if v_ref.id is not null then
        -- 3. 这个账号填过：别的码 → already_claimed；同一个码 → 重领（下面）。
        if v_ref.referrer <> v_referrer then
            raise exception 'already_claimed';
        end if;
        -- 022：推荐已不在 claimed → already_claimed。
        if v_ref.state <> 'claimed' then
            raise exception 'already_claimed';
        end if;
        v_variant := coalesce(v_ref.offer_variant, 'month');   -- 023：重领 / 换码沿用原组
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code
           for update;
        -- 已兑换的永远原样回；issued 且还有宽限天数以上的也原样回；
        -- 022：报过 /redeemed 且 issued 没过期 → 原样回（临期也不换）。
        if v_ref.offer_code is not null
           and (v_state = 'redeemed'
                or (v_state = 'issued' and v_expires > v_swap_before)
                or (v_state = 'issued' and v_ref.redeem_reported_at is not null and v_expires > now())) then
            return json_build_object('offer_code', v_ref.offer_code, 'fresh', false, 'offer_variant', v_variant);
        end if;
    else
        -- 4. 码是自己的，或当前设备在码主人记过的设备里（第 2 道）
        if v_referrer = p_user
           or (v_install is not null and exists (
                   select 1 from referral_devices where user_id = v_referrer and install_id = v_install)) then
            raise exception 'own_code';
        end if;
        v_variant := referral_offer_variant(p_user);            -- 023：第一次领码时定组
        insert into referral_users (user_id, code) values (p_user, referral_new_code())
            on conflict (user_id) do nothing;
        begin
            insert into referrals (referrer, referee, claim_install_id, offer_variant, offer_year_pct_at_claim)
                 values (v_referrer, p_user, v_install, v_variant,
                         case when coalesce(current_setting('referral.force_month', true), '') = 'on' then null
                              else (select offer_year_pct from referral_settings where id = 1) end)
              returning * into v_ref;
        exception when unique_violation then
            raise exception 'already_claimed';
        end;
        v_fresh := true;
    end if;

    -- 7. 本组的码里（023：年付组 = 年付 offer 名下的码，月付组 = 其余所有码），余量之外先到期的先发；skip locked。
    select code into v_offer from code_pool
     where state = 'free' and expires_at > v_cutoff
       and (case when v_variant = 'year' then offer_ref_name = v_ref_year
                 else offer_ref_name <> v_ref_year end)
     order by expires_at
       for update skip locked
     limit 1;
    if v_offer is null then
        if v_ref.offer_code is not null and v_state = 'issued' and v_expires > now() then
            return json_build_object('offer_code', v_ref.offer_code, 'fresh', false, 'offer_variant', v_variant);
        end if;
        raise exception 'pool_empty';
    end if;

    if v_ref.offer_code is not null and v_state = 'issued' then
        update code_pool set state = 'expired' where code = v_ref.offer_code;
    end if;

    update code_pool
       set state = 'issued', issued_to = p_user, issued_at = now(), referral_id = v_ref.id
     where code = v_offer;
    update referrals
       set offer_code    = v_offer,
           offer_variant = v_variant,                            -- 023：老推荐换码时补上 month
           reissued_at   = case when v_ref.offer_code is not null then now() else reissued_at end
     where id = v_ref.id;

    return json_build_object('offer_code', v_offer, 'fresh', v_fresh, 'offer_variant', v_variant);
end;
$$;

-- ---------------------------------------------------------------- ⑤ GET /v1/referral/me 的 pending offer

-- 与 022 同名同签名整体替换。区别只有回答多带 offer_variant（023）。
create or replace function referral_pending_offer(p_user text, p_install text) returns json
language plpgsql as $$
declare
    v_ref    referrals%rowtype;
    v_friend text;
    v_out    json;
begin
    select * into v_ref from referrals where referee = p_user;
    if v_ref.id is null or v_ref.state <> 'claimed' or v_ref.offer_code is null then
        return null;
    end if;
    if exists (select 1 from code_pool where code = v_ref.offer_code and state = 'redeemed') then
        return null;
    end if;
    select code into v_friend from referral_users where user_id = v_ref.referrer;
    if v_friend is null then
        return null;
    end if;
    begin
        v_out := referral_claim(p_user, v_friend, p_install);
    exception when raise_exception then
        return null;
    end;
    if coalesce(v_out->>'offer_code', '') = '' then
        return null;
    end if;
    return json_build_object('offer_code', v_out->>'offer_code', 'friend_code', v_friend,
                             'offer_variant', coalesce(v_out->>'offer_variant', 'month'));
end;
$$;

-- ---------------------------------------------------------------- ⑥ 没兑的一键换组

-- p_to = 'month' / 'year'。候选 = 新版 App 领的（offer_year_pct_at_claim 不为空：老推荐和老 App 包被强制月付的
-- 都是空，老包不认识组别、换成年付码会照旧显示「每月」）、推荐 claimed、手上码 issued（没兑）、没报过 /redeemed
-- （报过的可能已经兑了，换了白烧一张）、组别不是 p_to。整个函数一个事务：任何一条取不到目标组的码就整体回滚。
-- p_execute = false 只数不动：{to, candidates, available}。true 时目标组可发的码（同发码余量口径）
-- 不够候选数就一条都不动（raise 'not_enough_codes'）；够就逐条：老码划 expired（不放回去再发，防同一张
-- 被两个人兑）、发一张目标组的码、组别改成 p_to、reissued_at 记到现在。回 {to, candidates, available, moved}。
create or replace function referral_switch_unredeemed(p_to text, p_execute boolean) returns json
language plpgsql as $$
declare
    v_margin     int;
    v_cutoff     timestamptz;
    v_ref_year   text;
    v_candidates int;
    v_available  int;
    v_moved      int := 0;
    rec          record;
    v_offer      text;
begin
    if p_to not in ('month', 'year') then
        raise exception 'bad_variant';
    end if;
    select issue_margin_days, coalesce(offer_ref_year, 'Referral Friend Year')
      into v_margin, v_ref_year
      from referral_settings where id = 1;
    v_cutoff := now() + make_interval(days => coalesce(v_margin, 14));

    select count(*) into v_candidates
      from referrals r join code_pool c on c.code = r.offer_code
     where r.state = 'claimed' and c.state = 'issued'
       and r.redeem_reported_at is null
       and r.offer_year_pct_at_claim is not null
       and coalesce(r.offer_variant, 'month') <> p_to;
    select count(*) into v_available
      from code_pool
     where state = 'free' and expires_at > v_cutoff
       and (case when p_to = 'year' then offer_ref_name = v_ref_year else offer_ref_name <> v_ref_year end);

    if not p_execute then
        return json_build_object('to', p_to, 'candidates', v_candidates, 'available', v_available);
    end if;
    if v_available < v_candidates then
        raise exception 'not_enough_codes';
    end if;

    for rec in
        select r.id, r.referee, r.offer_code
          from referrals r join code_pool c on c.code = r.offer_code
         where r.state = 'claimed' and c.state = 'issued'
           and r.redeem_reported_at is null
           and r.offer_year_pct_at_claim is not null
           and coalesce(r.offer_variant, 'month') <> p_to
         order by r.id
           for update of r, c
    loop
        select code into v_offer from code_pool
         where state = 'free' and expires_at > v_cutoff
       and (case when p_to = 'year' then offer_ref_name = v_ref_year else offer_ref_name <> v_ref_year end)
         order by expires_at
           for update skip locked
         limit 1;
        if v_offer is null then
            raise exception 'not_enough_codes';   -- 整个事务回滚，一条都不动
        end if;
        update code_pool set state = 'expired' where code = rec.offer_code;
        update code_pool
           set state = 'issued', issued_to = rec.referee, issued_at = now(), referral_id = rec.id
         where code = v_offer;
        update referrals
           set offer_code = v_offer, offer_variant = p_to, reissued_at = now(),
               offer_variant_switched_at = now()
         where id = rec.id;
        v_moved := v_moved + 1;
    end loop;

    return json_build_object('to', p_to, 'candidates', v_candidates, 'available', v_available, 'moved', v_moved);
end;
$$;

-- ---------------------------------------------------------------- ⑦ 按组统计

-- invite-friends.md #### 后台：比例 / 统计 / 换码。只算领码那一刻年付组占比在 1–99 之间
-- （offer_year_pct_at_claim，真随机分组的那些）、没被一键换过组的推荐；作废原因是 duplicate_apple_id / rejected 的剔除，其余作废
-- （refunded / no_subscription）留在分母。兑换 = redeemed_at 不为空。扣费 = reward_ledger 里这条推荐的行
-- （price_usd_cents 恒 > 0）。实收 = 没被作废（state <> 'voided'）的扣费的 price × takehome。
-- 窗口 N 天：「每个领码」按 claimed_at + N 天截止、分母 = 领码已满 N 天的；「每个兑换」按 redeemed_at +
-- N 天截止、分母 = 兑换已满 N 天的。switched 单独回被换过组的人数（不进其余各列）。
create or replace function referral_ab_stats()
returns table (variant text, claimed int, redeemed int, paid int, renewed int, refunded int, switched int,
               window_days int, claim_cohort int, claim_proceeds_cents bigint,
               redeem_cohort int, redeem_proceeds_cents bigint)
language sql stable as $$
    with base as (
        select r.id, r.offer_variant, r.claimed_at, r.redeemed_at, r.offer_variant_switched_at
          from referrals r
         where r.offer_variant is not null
           and r.offer_year_pct_at_claim between 1 and 99
           and not (r.state = 'void' and coalesce(r.void_reason, '') in ('duplicate_apple_id', 'rejected'))
    ), refs as (
        select * from base where offer_variant_switched_at is null
    ), led as (
        select l.referral_id, l.purchased_at, l.state, coalesce(l.void_reason, '') as void_reason,
               round(l.price_usd_cents * l.takehome_percentage)::bigint as proceeds
          from reward_ledger l join refs on refs.id = l.referral_id
    ), led_by_ref as (
        select referral_id, count(*) as charges, bool_or(void_reason = 'refunded') as was_refunded
          from led group by referral_id
    ), per as (
        select refs.id, refs.offer_variant, refs.claimed_at, refs.redeemed_at,
               coalesce(lb.charges, 0) as charges,
               coalesce(lb.was_refunded, false) as was_refunded
          from refs left join led_by_ref lb on lb.referral_id = refs.id
    ), windows as (
        select unnest(array[30, 90, 180, 365, 425]) as days
    ), variants as (
        select unnest(array['month', 'year']) as variant
    ), agg as (
        select v.variant,
               count(per.id)::int                                            as claimed,
               count(per.id) filter (where per.redeemed_at is not null)::int as redeemed,
               count(per.id) filter (where per.charges >= 1)::int            as paid,
               count(per.id) filter (where per.charges >= 2)::int            as renewed,
               count(per.id) filter (where per.was_refunded)::int            as refunded,
               (select count(*) from base where base.offer_variant = v.variant
                   and base.offer_variant_switched_at is not null)::int      as switched
          from variants v left join per on per.offer_variant = v.variant
         group by v.variant
    )
    select agg.variant, agg.claimed, agg.redeemed, agg.paid, agg.renewed, agg.refunded, agg.switched,
           w.days,
           (select count(*) from per where per.offer_variant = agg.variant
               and per.claimed_at <= now() - make_interval(days => w.days))::int,
           (select coalesce(sum(led.proceeds), 0) from per join led on led.referral_id = per.id
             where per.offer_variant = agg.variant
               and per.claimed_at <= now() - make_interval(days => w.days)
               and led.state <> 'voided'
               and led.purchased_at < per.claimed_at + make_interval(days => w.days))::bigint,
           (select count(*) from per where per.offer_variant = agg.variant and per.redeemed_at is not null
               and per.redeemed_at <= now() - make_interval(days => w.days))::int,
           (select coalesce(sum(led.proceeds), 0) from per join led on led.referral_id = per.id
             where per.offer_variant = agg.variant and per.redeemed_at is not null
               and per.redeemed_at <= now() - make_interval(days => w.days)
               and led.state <> 'voided'
               and led.purchased_at < per.redeemed_at + make_interval(days => w.days))::bigint
      from agg cross join windows w
     order by agg.variant, w.days;
$$;

-- ---------------------------------------------------------------- ⑧ 改比例

-- 运营脚本 referral-ab.sh set 调。第一次从 0 改成大于 0 时记 offer_ab_started_at（之后不再改，
-- 调回 0 再调上去也沿用第一次的时间）。回 {before, after, started_at}。
create or replace function referral_set_offer_year_pct(p_pct int) returns json
language plpgsql as $$
declare
    v_before  int;
    v_started timestamptz;
begin
    if p_pct is null or p_pct < 0 or p_pct > 100 then
        raise exception 'bad_pct';
    end if;
    select offer_year_pct, offer_ab_started_at into v_before, v_started
      from referral_settings where id = 1 for update;
    if v_started is null and p_pct > 0 then
        v_started := now();
    end if;
    update referral_settings set offer_year_pct = p_pct, offer_ab_started_at = v_started where id = 1;
    return json_build_object('before', v_before, 'after', p_pct, 'started_at', v_started);
end;
$$;

-- ---------------------------------------------------------------- ⑨ 后台概况

-- 运营脚本 referral-ab.sh show 调：比例、开始时间、两组可发库存（同发码余量口径）、没兑的人数、
-- 不认识的码名（既不是 offer_ref_month 也不是 offer_ref_year，会被当月付码发）。
create or replace function referral_ab_overview() returns json
language sql stable as $$
    with cfg as (
        select offer_year_pct, offer_ab_started_at, offer_ref_month, offer_ref_year,
               now() + make_interval(days => issue_margin_days) as cutoff
          from referral_settings where id = 1
    )
    select json_build_object(
        'offer_year_pct',      cfg.offer_year_pct,
        'offer_ab_started_at', cfg.offer_ab_started_at,
        'offer_ref_month',     cfg.offer_ref_month,
        'offer_ref_year',      cfg.offer_ref_year,
        'usable_month', (select count(*) from code_pool c where c.state = 'free' and c.expires_at > cfg.cutoff
                            and c.offer_ref_name <> cfg.offer_ref_year),
        'usable_year',  (select count(*) from code_pool c where c.state = 'free' and c.expires_at > cfg.cutoff
                            and c.offer_ref_name = cfg.offer_ref_year),
        'unredeemed_month', (select count(*) from referrals r join code_pool c on c.code = r.offer_code
                              where r.state = 'claimed' and c.state = 'issued' and coalesce(r.offer_variant, 'month') = 'month'),
        'unredeemed_year',  (select count(*) from referrals r join code_pool c on c.code = r.offer_code
                              where r.state = 'claimed' and c.state = 'issued' and r.offer_variant = 'year'),
        'unknown_names', coalesce((select json_agg(distinct c.offer_ref_name) from code_pool c
                                    where c.state = 'free'
                                      and c.offer_ref_name not in (cfg.offer_ref_month, cfg.offer_ref_year)), '[]'::json))
      from cfg;
$$;

revoke execute on function referral_offer_variant(text)               from public, anon, authenticated;
revoke execute on function referral_switch_unredeemed(text, boolean)  from public, anon, authenticated;
revoke execute on function referral_ab_stats()                         from public, anon, authenticated;
revoke execute on function referral_set_offer_year_pct(int)            from public, anon, authenticated;
revoke execute on function referral_ab_overview()                      from public, anon, authenticated;
grant  execute on function referral_offer_variant(text)               to service_role;
grant  execute on function referral_switch_unredeemed(text, boolean)  to service_role;
grant  execute on function referral_ab_stats()                         to service_role;
grant  execute on function referral_set_offer_year_pct(int)            to service_role;
grant  execute on function referral_ab_overview()                      to service_role;
