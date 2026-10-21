-- 017_referral_review_fixes.sql
-- 推荐好友防刷（016）评审修补。三处库里的改动，Go 侧同 PR（referral/SPEC.md）：
--
--   ① 结算批次轮转 —— referrals.last_checked_at：结算 ①（claimed）/ ④（saved）每查完一条 RevenueCat 记一笔。
--      原来每轮固定按同一顺序取 200 条，积压超过 200 条「填了码一直没兑」的 claimed 之后，每轮拿到同一批，
--      新来的永远查不到（推荐人拿不到月份；webhook 对 claimed 态的首购也回 not_redeemed）。现在
--      ListClaimed 先取「上次查之后又报过 /redeemed 的」，其次 last_checked_at 没有 / 最早的；ListSavers 同理
--      （进 saved 时清空，见 referral/pgstore.go MarkSaved）。
--   ② 重领不白烧码 —— referral_claim（三参数）：推荐已不在 claimed（redeemed / qualified / held / saved /
--      rewarded / void）时重领一律回原来那张码，不换码、不动库存。码在到期前被兑了、结算 ① 晚于 ExpireCodes
--      才认定兑换时，码池那行已是 expired，以前会走到「换码」再发一张（App 重进填码页会自动重领）。
--      另外读推荐那一行改成 for update，和结算 ① 的 referral_mark_redeemed 串行。
--   ③ 速度闸按「理论成功时间」数 —— referral_qualify：理论成功时间 = redeemed_at + hold_days；「这个推荐人过去
--      velocity_window_hours 小时内已成功的」按它数，不按结算跑到的时刻（qualified_at）。结算停摆后恢复时，
--      积压的成功会在同一轮一起认定，按 qualified_at 数第 6 个起全进 held（误拦）。所有推荐用同一个 hold_days，
--      两条的理论成功时间之差就是 redeemed_at 之差，所以下面直接比 redeemed_at。
--
-- 用法：psql "$SUPABASE_DB_URL" -f 017_referral_review_fixes.sql，或 Supabase SQL Editor 整份跑。
-- 依赖 013 / 014 / 015 / 016。可重复执行。先 staging 后 prod，且先于二进制（新二进制读写 last_checked_at；
-- 没跑 017 时结算 ① ④ 每轮报错、/claim 照常）。函数签名一个没动：两个函数是同名同签名整体替换，
-- 旧二进制（016 那版）照常调，回滚二进制不用回滚库。
-- ⚠️ 017 之后若重跑 016，会把这两个函数换回 016 的版本 —— 重跑 016 后要再跑一遍 017。

-- ---------------------------------------------------------------- ① 结算批次轮转

alter table referrals
    add column if not exists last_checked_at timestamptz;   -- 结算 ① / ④ 上次就这一条问过 RevenueCat 的时间（进 saved 时清空）

-- ---------------------------------------------------------------- ② POST /v1/referral/claim

-- 与 016 同名同签名整体替换。和 016 的区别只有两处（标了 017）：读推荐行 for update；推荐不在 claimed
-- 时原样回老码。判定顺序、fresh、claim_install_id、发码余量、换码规则都照 016。
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
        if v_ref.state <> 'claimed' then
            if v_ref.offer_code is not null then
                return json_build_object('offer_code', v_ref.offer_code, 'fresh', false);
            end if;
            raise exception 'already_claimed';
        end if;
        -- 锁住手上那张，免得和结算的 ExpireCodes 交叉。
        select state, expires_at into v_state, v_expires
          from code_pool where code = v_ref.offer_code
           for update;
        -- 已兑换的永远原样回；issued 且还有宽限天数以上的也原样回。
        if v_ref.offer_code is not null
           and (v_state = 'redeemed' or (v_state = 'issued' and v_expires > v_swap_before)) then
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

-- ---------------------------------------------------------------- ③ 结算 ②：判成功 + 第 4 道

-- 与 016 同名同签名整体替换。redeemed → qualified，或者 held：这个推荐人在「这一条的理论成功时间」之前
-- velocity_window_hours 小时内（含那一刻）已成功的——qualified / held / saved / rewarded，按理论成功时间——
-- 已经 ≥ velocity_limit 条。理论成功时间 = redeemed_at + hold_days，所有推荐同一个 hold_days，比较时消掉，
-- 等价于比 redeemed_at：(这一条 redeemed_at − 窗口, 这一条 redeemed_at]。只往前看：比这一条晚兑换的
-- 理论上在它之后才成功（结算按 redeemed_at 升序处理，正常情况下它们这时还没判）。
-- 两种结果都照旧记 qualified_at = now()（运营邮件、/me 不受影响）；held 自己也算进后面的计数。
-- velocity_limit = 0 = 闸关。锁推荐人那一行把同一推荐人的判定排队（NO KEY UPDATE 不挡 referrals 插 FK）。
-- 回 'qualified' / 'held' / 'skipped'（不是 redeemed 了）。
create or replace function referral_qualify(p_id bigint) returns text
language plpgsql as $$
declare
    v_referrer text;
    v_redeemed timestamptz;
    v_limit    int;
    v_window   int;
    v_recent   int;
    v_to       text := 'qualified';
begin
    select referrer, redeemed_at into v_referrer, v_redeemed
      from referrals where id = p_id and state = 'redeemed';
    if v_referrer is null then
        return 'skipped';
    end if;
    perform 1 from referral_users where user_id = v_referrer for no key update;

    select velocity_limit, velocity_window_hours into v_limit, v_window
      from referral_settings where id = 1;
    v_limit    := coalesce(v_limit, 5);
    v_window   := coalesce(v_window, 24);
    v_redeemed := coalesce(v_redeemed, now());   -- redeemed 一定有 redeemed_at（结算 ① 记的）；兜底
    if v_limit > 0 then
        select count(*) into v_recent from referrals
         where referrer = v_referrer and id <> p_id
           and state in ('qualified','held','saved','rewarded')
           and coalesce(redeemed_at, qualified_at) >  v_redeemed - make_interval(hours => v_window)
           and coalesce(redeemed_at, qualified_at) <= v_redeemed;
        if v_recent >= v_limit then
            v_to := 'held';
        end if;
    end if;

    update referrals set state = v_to, qualified_at = now()
     where id = p_id and state = 'redeemed';
    return v_to;
end;
$$;

-- ---------------------------------------------------------------- 谁能调

-- create or replace 保留原有权限；这里照 016 再写一遍，单独跑 017 也对。
revoke execute on function referral_claim(text, text, text) from public, anon, authenticated;
revoke execute on function referral_qualify(bigint)         from public, anon, authenticated;
grant  execute on function referral_claim(text, text, text) to service_role;

-- 让 PostgREST 重新读 schema（deploy/referral-review.sh 经 PostgREST 读 referrals）。
notify pgrst, 'reload schema';
