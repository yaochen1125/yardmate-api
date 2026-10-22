-- 019_referral_w9_threshold.sql
-- 礼品卡申请邮件里「W-9 FIRST」的门槛改成可配置（rewards.md ### 税务：W-9，2026-10-22）。
--
-- 美国 One Big Beautiful Bill Act 把 1099-NEC / 1099-MISC 申报门槛从 $600 提到 $2,000，适用于 2026
-- 年起支付的款项，2027 年起按通胀调整。门槛放这里，改库即生效、不用发版：每年 1 月按 IRS 公布的新值改
--   update referral_settings set w9_threshold_cents = <新值> where id = 1;
-- （先 staging 后 prod）。申请邮件在「本自然年已发 + 申请中（含这一张）≥ 门槛 − 一张卡（$100）」时
-- 写 W-9 FIRST，默认就是 $1,900。读不到这一列时后端按 $2,000 算并记日志（rewards.DefaultW9ThresholdCents）。
--
-- 用法：psql "$SUPABASE_DB_URL" -X -v ON_ERROR_STOP=1 -1 -f 019_referral_w9_threshold.sql，或 Supabase
-- SQL Editor 整份跑。可重复执行。先后都行（没有这一列时只是邮件按默认 $2,000 算）。

alter table referral_settings
    add column if not exists w9_threshold_cents int not null default 200000
        check (w9_threshold_cents > 0);
