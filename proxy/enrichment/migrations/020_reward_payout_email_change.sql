-- 020_reward_payout_email_change.sql
-- 申请中的礼品卡可以改收卡邮箱（POST /v1/referral/payout/email，rewards.md ### 邮箱：确认、拼写提示、修改，2026-10-22）。
--
-- reward_payouts.email_changed_at：用户最近一次改邮箱的时间，没改过为空。只有 state = requested 的那一行
-- 能改（Go 侧 select … for update 锁住再改，和 018 的 reward_payout_mark_sent 按行锁排先后）。
-- deploy/referral-payout.sh mark-sent 在两次确认前列出「邮箱 {时间} 被用户改过」，旧地址在「邮箱已改」通知邮件里。
--
-- 用法：psql "$SUPABASE_DB_URL" -X -v ON_ERROR_STOP=1 -1 -f 020_reward_payout_email_change.sql，或 Supabase
-- SQL Editor 整份跑。可重复执行。先 staging 后 prod，且先于二进制（新二进制写这一列）和新脚本（脚本读这一列）。

alter table reward_payouts add column if not exists email_changed_at timestamptz;

-- 让 PostgREST 重新读 schema（脚本经 PostgREST 读这一列）。
notify pgrst, 'reload schema';
