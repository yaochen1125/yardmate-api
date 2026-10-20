-- 014_referral_pool_margin.sql
-- 推荐好友码池：发码留余量 + 临期 / 低库存告警的三个阈值。
--
-- 问题：013 的 referral_claim 只要 expires_at > now() 就发，B 可能拿到一张明天就过期
-- 的码，去 App Store 兑换时已经失效。改成：
--   * 只发 expires_at > now() + issue_margin_days 的码（默认 14 天），仍按 expires_at
--     升序先发快过期的；
--   * 同一人再 claim 时，手上那张 issued 码若已进入余量期（不只是已过期）也划 expired
--     换新；已 redeemed 的原样回；
--   * pool_empty 的口径同步改成「没有留够余量的 free 码」——余量期内的 free 码不发、
--     也不改状态，等结算循环的 ExpireCodes 到期划 expired。
-- 三个阈值放 referral_settings，改库即生效、不用发版（referral/settle.go 每轮读）。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 014_referral_pool_margin.sql，或 Supabase SQL Editor
-- 整份跑。可重复执行。先 staging 后 prod，且先于二进制（新二进制每轮读这三列）。

-- ---------------------------------------------------------------- 设置

alter table referral_settings
    add column if not exists issue_margin_days   int not null default 14
        check (issue_margin_days >= 0),      -- 发码时码至少还得活这么多天
    add column if not exists low_stock_threshold int not null default 100
        check (low_stock_threshold >= 0),    -- 可发（free 且余量够）少于这个数 → 告警
    add column if not exists expiry_warn_days    int not null default 30
        check (expiry_warn_days >= 0);       -- 最早一批可发码在这么多天内到期 → 告警

-- ---------------------------------------------------------------- POST /v1/referral/claim

-- 与 013 同名同签名，整体替换（create or replace 保留已有授权，下面仍重申一遍）。
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
    v_margin   int;
    v_cutoff   timestamptz;
begin
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;
    if v_referrer = p_user then
        raise exception 'own_code';
    end if;

    -- 发码余量：只发 B 来得及去兑换的码。设置行缺失（不该发生）按 14 天。
    select issue_margin_days into v_margin from referral_settings where id = 1;
    v_cutoff := now() + make_interval(days => coalesce(v_margin, 14));

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
        -- 已兑换的原样回；还在余量之外的 issued 码原样回；
        -- 过期了、或者已进入余量期（B 可能来不及兑）的 → 划 expired、换一张新的。
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code;
        if v_ref.offer_code is not null
           and (v_state = 'redeemed' or (v_state = 'issued' and v_expires > v_cutoff)) then
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

    -- 真过期的先划掉（余量期内的留着，状态如实，等 ExpireCodes 到期再划）。
    update code_pool set state = 'expired'
     where state = 'free' and expires_at <= now();

    -- 余量之外的里面先到期的先发；skip locked：另一台手机正在拿的那张直接跳过。
    select code into v_offer from code_pool
     where state = 'free' and expires_at > v_cutoff
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

-- 同 013：PostgREST 把 rpc 暴露在公网上，EXECUTE 只留给 service_role。
revoke execute on function referral_claim(text, text) from public, anon, authenticated;
grant  execute on function referral_claim(text, text) to service_role;

notify pgrst, 'reload schema';
