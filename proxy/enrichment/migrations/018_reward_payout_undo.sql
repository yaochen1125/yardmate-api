-- 018_reward_payout_undo.sql
-- 礼品卡「标记已发」：两次确认 + 1 小时缓冲 + 可撤回（rewards.md ### 标记已发，2026-10-22）。
--
-- 原来 deploy/referral-payout.sh mark-sent 直接 PATCH state = sent，立即生效、不能撤回。改成：
--   * reward_payouts.undo_until：标记时 = now() + 1 hour，撤回后清空；老行为空 = 没有缓冲。
--   * reward_payout_mark_sent(p_id, p_email)：只在 state = requested 且 email 仍是脚本列给运营看的
--     那个时才改（两次确认之间用户可能改了邮箱）；改不了返回空集，脚本据此报错、什么都不动。
--   * reward_payout_undo(p_id)：只在 state = sent 且 undo_until > now() 时改回 requested。
--   * 两个函数都在库里一步完成「判断 + 更新」，时间取数据库 now()。缓冲期的其他判断（/me 仍按
--     requested 返回、/payout 当作在路上、改邮箱 409）也在 Go 的 SQL 里用 undo_until > now()
--     （rewards/pgstore.go），不用 API 服务器的钟：两边差几秒就可能 /me 已显示 Sent 而 undo 还能撤。
-- 可兑余额不受影响：requested 和 sent 本来都从余额里扣。
--
-- 撤回回到 requested 不会撞「每人只能有一张 requested」的唯一索引：缓冲期里 /payout 回
-- payout_in_progress，同一个人建不出第二张（新二进制；所以本迁移先于二进制上线也没关系，
-- 旧二进制期间极少数撞索引的 undo 会报错、什么都不改）。
--
-- 用法：psql "$SUPABASE_DB_URL" -X -v ON_ERROR_STOP=1 -1 -f 018_reward_payout_undo.sql，或 Supabase
-- SQL Editor 整份跑。可重复执行。先 staging 后 prod，且先于二进制（新二进制读 undo_until）。

alter table reward_payouts add column if not exists undo_until timestamptz;

create or replace function reward_payout_mark_sent(p_id bigint, p_email text)
returns setof reward_payouts
language sql as $$
    update reward_payouts
       set state = 'sent', sent_at = now(), undo_until = now() + interval '1 hour'
     where id = p_id and state = 'requested' and email = p_email
    returning *;
$$;

create or replace function reward_payout_undo(p_id bigint)
returns setof reward_payouts
language sql as $$
    update reward_payouts
       set state = 'requested', sent_at = null, undo_until = null
     where id = p_id and state = 'sent' and undo_until > now()
    returning *;
$$;

-- 同 013：Supabase 默认给 public 执行权，收回；只给运营脚本用的 service_role。
revoke execute on function reward_payout_mark_sent(bigint, text) from public, anon, authenticated;
revoke execute on function reward_payout_undo(bigint)            from public, anon, authenticated;
grant  execute on function reward_payout_mark_sent(bigint, text) to service_role;
grant  execute on function reward_payout_undo(bigint)            to service_role;

-- 让 PostgREST 重新读 schema（脚本经 /rest/v1/rpc/ 调这两个函数）。
notify pgrst, 'reload schema';
