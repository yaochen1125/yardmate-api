-- 014_referral_pool_margin.sql
-- 推荐好友码池：发码留余量、再 claim 换码的宽限、临期 / 低库存告警的阈值。
--
-- 问题：013 的 referral_claim 只要 expires_at > now() 就发，B 可能拿到一张明天就过期
-- 的码，去 App Store 兑换时已经失效。改成：
--   * 新发只发 expires_at > now() + issue_margin_days 的码（默认 14 天），仍按 expires_at
--     升序先发快过期的；
--   * 同一人再 claim：已 redeemed 原样回；手上那张 issued 码只要还有 swap_grace_days
--     （默认 3 天）以上就原样回——换码阈值故意不等于发码余量，否则每张快到余量线的码
--     都会被再 claim 系统性换掉、白烧第二张好码；
--   * 要换码时**先选替换码再划老码**：池子里没有留够余量的替换码、老码在苹果那边还能用
--     （expires_at > now()）→ 原样回老码；老码也不能用了才 pool_empty；
--   * 换码时记 referrals.reissued_at（新列），结算 ListClaimed 的 200 天窗口按它算，不然很晚
--     才兑的会掉出去；claimed_at 不动——结算认「订阅起始晚于第一次填码」，B 可能换码前刚用
--     老码兑了、RC 还没反映，刷了 claimed_at 那笔就永远对不上（Codex P2）；
--   * pool_empty 的口径改成「没有留够余量的 free 码（且老码也不能用）」——余量期内的 free 码
--     不发、也不改状态，等结算循环的 ExpireCodes 到期划 expired。
-- 四个阈值放 referral_settings，改库即生效、不用发版（referral/settle.go 每轮读）。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 014_referral_pool_margin.sql，或 Supabase SQL Editor
-- 整份跑。可重复执行。先 staging 后 prod，且先于二进制（新二进制每轮读这几列）。

-- ---------------------------------------------------------------- 设置

alter table referral_settings
    add column if not exists issue_margin_days   int not null default 14
        check (issue_margin_days >= 0),      -- 新发的码至少还得活这么多天
    add column if not exists swap_grace_days     int not null default 3
        check (swap_grace_days >= 0),        -- 再 claim 时手上那张不足这么多天才换新
    add column if not exists low_stock_threshold int not null default 100
        check (low_stock_threshold >= 0),    -- 可发（free 且余量够）少于这个数 → 告警
    add column if not exists expiry_warn_days    int not null default 30
        check (expiry_warn_days >= 0);       -- 最早一批可发码在这么多天内到期、且到期后可发数会低于阈值 → 告警

-- 最近一次换码的时间：只用来延长结算 ListClaimed 的轮询窗口（coalesce(reissued_at, claimed_at)），
-- 没换过 = null。claimed_at 永远是第一次填码。
alter table referrals
    add column if not exists reissued_at timestamptz;

-- ---------------------------------------------------------------- POST /v1/referral/claim

-- 与 013 同名同签名，整体替换（create or replace 保留已有授权，下面仍重申一遍）。
-- p_code 已由 Go 归一化成 YM-XXXX。拒绝用 raise exception（P0001 + 错误码做 message），
-- Go 侧把它映射成 HTTP 400/503；raise 同时回滚本函数里的所有写入。
create or replace function referral_claim(p_user text, p_code text) returns json
language plpgsql as $$
declare
    v_referrer    text;
    v_ref         referrals%rowtype;
    v_offer       text;
    v_state       text;
    v_expires     timestamptz;
    v_margin      int;
    v_grace       int;
    v_cutoff      timestamptz;   -- 新发的码到期日必须晚于这个
    v_swap_before timestamptz;   -- 手上那张到期日早于这个才换
begin
    select user_id into v_referrer from referral_users where code = p_code;
    if v_referrer is null then
        raise exception 'unknown_code';
    end if;
    if v_referrer = p_user then
        raise exception 'own_code';
    end if;

    -- 两个阈值。设置行缺失（不该发生）按默认值。
    select issue_margin_days, swap_grace_days into v_margin, v_grace
      from referral_settings where id = 1;
    v_cutoff      := now() + make_interval(days => coalesce(v_margin, 14));
    v_swap_before := now() + make_interval(days => coalesce(v_grace, 3));

    -- B 也要有自己的一行（FK），顺手给他造码。
    insert into referral_users (user_id, code) values (p_user, referral_new_code())
        on conflict (user_id) do nothing;

    select * into v_ref from referrals where referee = p_user;
    if v_ref.id is not null then
        -- 换了个人的码再来：不行，一个人只能被推荐一次。
        if v_ref.referrer <> v_referrer then
            raise exception 'already_claimed';
        end if;
        -- 同一个人、同一个码再来（上次可能拿到码之后 App 崩了）。锁住手上那张，
        -- 免得和结算的 MarkRedeemed / ExpireCodes 交叉。
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code
           for update;
        -- 已兑换的永远原样回；issued 且还有宽限天数以上的也原样回。
        if v_ref.offer_code is not null
           and (v_state = 'redeemed' or (v_state = 'issued' and v_expires > v_swap_before)) then
            return json_build_object('offer_code', v_ref.offer_code);
        end if;
        -- 走到这里 = 想换：老码过期了 / 快过期了 / 状态不对 / 行没了。先去选替换码，
        -- 选到了才动老码（下面）。
    else
        begin
            insert into referrals (referrer, referee) values (v_referrer, p_user)
                returning * into v_ref;
        exception when unique_violation then
            -- 同一瞬间两台设备都在填码。晚到的那个看到的就是「已经领过了」。
            raise exception 'already_claimed';
        end;
    end if;

    -- 余量之外的里面先到期的先发；skip locked：另一台手机正在拿的那张直接跳过。
    -- （过期的码不用先划：> v_cutoff 已经把它们排除了，划 expired 交给结算的 ExpireCodes。）
    select code into v_offer from code_pool
     where state = 'free' and expires_at > v_cutoff
     order by expires_at
       for update skip locked
     limit 1;
    if v_offer is null then
        -- 没有留够余量的替换码。老码在苹果那边还能用就先给老码，总比 503 强；
        -- 老码也不能用了（过期 / 状态不对）才算没货。首次 claim 的 raise 会回滚上面插的 referral。
        if v_ref.offer_code is not null and v_state = 'issued' and v_expires > now() then
            return json_build_object('offer_code', v_ref.offer_code);
        end if;
        raise exception 'pool_empty';
    end if;

    -- 换码：老码划 expired（苹果那边它可能还活几天，但我们不再认它），
    -- reissued_at 记到现在——结算的 ListClaimed 按它延长 200 天窗口。claimed_at 不动：
    -- 订阅起始要和第一次填码比，B 可能换码前刚用老码兑了、RC 还没反映（Codex P2）。
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

    return json_build_object('offer_code', v_offer);
end;
$$;

-- ---------------------------------------------------------------- 谁能调

-- 同 013：PostgREST 把 rpc 暴露在公网上，EXECUTE 只留给 service_role。
revoke execute on function referral_claim(text, text) from public, anon, authenticated;
grant  execute on function referral_claim(text, text) to service_role;

notify pgrst, 'reload schema';
