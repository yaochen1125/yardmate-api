-- 022_referral_pending_offer.sql
-- 推荐好友：领了码没兑，换机 / 重装后找回（yardmate-swiftui docs: invite-friends.md
-- ### 领了码没兑，换机 / 重装后找回）。Go 侧同 PR（referral/SPEC.md）。
--
--   ① referral_pending_offer(p_user, p_install)：GET /v1/referral/me 在库里说「填过码」时调。
--      这个账号作为朋友有一条推荐还在 claimed、手上有码 → {offer_code, friend_code}；否则 null。
--      回之前保证码能用：就是拿当初的朋友码走一遍重领（三参数 referral_claim），换不换码全按
--      重领的规矩（下面 ②），换不到又已过期（pool_empty）→ null。码池那张已是 redeemed 的不回。
--   ② referral_claim / referral_claim_check（同名同签名整体替换）：
--      * 同一个码、推荐已不在 claimed → already_claimed（原来 referral_claim 原样回老码，check 回 'again'）。
--      * 重领时报过 /redeemed（redeem_reported_at）且老码 issued 没过期 → 原样回，临期也不换：可能已兑掉、
--        结算还没认定，换码会白烧一张。过期了才换（重装的人手上没朋友码，不换就回不去；真兑掉了最多
--        白烧这一张，App 端「订阅过不恢复」也不会摆出来）。/me、填码页自动重领、手动重填同一套。
--      其余照 016 / 017。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 022_referral_pending_offer.sql，或 Supabase SQL Editor 整份跑。
-- 依赖 013–017。可重复执行。先 staging 后 prod，且先于二进制（新二进制的 /me 调 ①；没跑 022 时
-- ① 报错 → /me 记一行日志、不回 pending_offer，其余照常）。函数签名没动，旧二进制照常调；
-- 回滚二进制不用回滚库（旧二进制只是看不到 ①，② 对它同样生效）。
-- ⚠️ 022 之后若重跑 016 或 017，会把 ② 换回旧版 —— 重跑后要再跑一遍 022。

-- ---------------------------------------------------------------- ② POST /v1/referral/claim

-- 与 016 同名同签名整体替换。区别只有标了 022 的那一处。
create or replace function referral_claim_check(p_user text, p_code text, p_install text) returns text
language plpgsql as $$
declare
    v_referrer text;
    v_prior    text;
    v_state    text;
    v_install  text := lower(nullif(btrim(coalesce(p_install, '')), ''));
begin
    -- 2. 码认不出
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;
    -- 3. 这个账号填过
    select referrer, state into v_prior, v_state from referrals where referee = p_user;
    if v_prior is not null then
        -- 022：同一个码但推荐已不在 claimed（兑换了 / 作废了）也算填过。
        if v_prior <> v_referrer or v_state <> 'claimed' then
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

-- 与 017 同名同签名整体替换。区别只有标了 022 的两处。
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

    -- 017：锁住这一行，和结算 ① 的 referral_mark_redeemed（也 for update 这一行）串行。
    select * into v_ref from referrals where referee = p_user for update;
    if v_ref.id is not null then
        -- 3. 这个账号填过：别的码 → already_claimed；同一个码 → 重领（下面）。
        if v_ref.referrer <> v_referrer then
            raise exception 'already_claimed';
        end if;
        -- 017：推荐已经不在 claimed —— 兑换了（redeemed / qualified / held / saved / rewarded）或作废了（void）——
        -- 一律回原来那张，不换码、不动库存。兑换过的码在苹果那边用不了第二次，换给他也没用；码池那行可能已被
        -- ExpireCodes 划成 expired（码在到期前兑了、结算 ① 晚于到期才认定）。作废的同 016：天天重领会把好码
        -- 一张张烧掉。手上没有老码（不该发生）就当填过。
        -- 022：推荐已经不在 claimed（兑换了 / 作废了）→ already_claimed，不再原样回老码。
        -- 那张码兑换过就用不了第二次，作废的不再发；回老码会让 App 停在「领到码」摆一张用不了的码。
        if v_ref.state <> 'claimed' then
            raise exception 'already_claimed';
        end if;
        -- 锁住手上那张，免得和结算的 ExpireCodes 交叉。
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code
           for update;
        -- 已兑换的永远原样回；issued 且还有宽限天数以上的也原样回。
        -- 022：报过 /redeemed 的 issued 码只要没过期也原样回（临期不换）。
        if v_ref.offer_code is not null
           and (v_state = 'redeemed'
                or (v_state = 'issued' and v_expires > v_swap_before)
                or (v_state = 'issued' and v_ref.redeem_reported_at is not null and v_expires > now())) then
            return json_build_object('offer_code', v_ref.offer_code, 'fresh', false);
        end if;
        -- 走到这里 = 还在 claimed、想换：老码过期了 / 快过期了 / 状态不对 / 行没了。先选替换码，选到了才动老码。
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

-- ---------------------------------------------------------------- ① GET /v1/referral/me

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
    -- 重领：锁推荐行、要不要换码、码池空时回还没过期的老码，都在 referral_claim 里。
    -- 拒绝（pool_empty；同一瞬间推荐被兑换 / 作废 → already_claimed）→ 这次不回。
    begin
        v_out := referral_claim(p_user, v_friend, p_install);
    exception when raise_exception then
        return null;
    end;
    if coalesce(v_out->>'offer_code', '') = '' then
        return null;
    end if;
    return json_build_object('offer_code', v_out->>'offer_code', 'friend_code', v_friend);
end;
$$;

revoke execute on function referral_pending_offer(text, text) from public, anon, authenticated;
grant  execute on function referral_pending_offer(text, text) to service_role;
