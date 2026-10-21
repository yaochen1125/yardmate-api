#!/usr/bin/env bash
# Watchdog: scan yardmate-api journal for /v1/identify anomalies and email
# via yardmate-notify.sh. Fills the healthz watchdog's blind spot — the
# 2026-08-26 Pl@ntNet outage produced 502s and 28s "invisible 200s" while
# /healthz stayed green the whole time, so nobody was paged.
#
# 每 10 分钟一轮，扫最近 11 分钟（1 分钟重叠防 timer 漂移漏缝）。触发条件任一：
#   - identify 502 ≥ 1                       （用户已经看到失败）
#   - identify 200 但耗时 > SLOW_SECS ≥ 1    （危险区：接近客户端 30s 计时器）
#   - GPT 兜底失败/预算耗尽跳过 ≥ 1          （兜底链路自身出问题）
#   - Pl@ntNet 失败 ≥ 2                      （上游 outage 进行中，即使兜底救回）
#     = `identify plantnet down:` 行数：引擎赛跑每个请求最多打一行（reason=error 快速失败 /
#       slow 6s 没回 / deadline 赛跑截止），不会因同一请求的 hedge + skip/budget 多行重复计数
# 30 分钟冷却：持续 outage 期间最多每半小时一封，不淹邮箱（healthz watchdog
# 的 2026-07-28 教训是只发一封会被淹没；这里反向教训是别变成邮件炸弹）。
set -uo pipefail

WINDOW="11 min ago"
NOTIFY=/usr/local/bin/yardmate-notify.sh
COOLDOWN_FILE=/run/yardmate-identify-alert.last   # tmpfs, 重启即清
COOLDOWN_SECS=1800
SLOW_SECS=18

LOG=$(journalctl -u yardmate-api --since "$WINDOW" --no-pager 2>/dev/null | grep -E "identify|plantnet trace:" || true)
[ -z "$LOG" ] && exit 0

count() { grep -cF "$1" <<<"$LOG" 2>/dev/null || true; }

c502=$(grep -cE 'v1/identify HTTP/1\.[01]" from [0-9.]+ - 502 ' <<<"$LOG" || true)
slow200=$(grep -E 'v1/identify HTTP/1\.[01]" from [0-9.]+ - 200 ' <<<"$LOG" \
    | awk -v t="$SLOW_SECS" '{d=$NF; sub(/s$/,"",d); if (d+0 > t) n++} END {print n+0}')
rescue_err=$(count "gpt on-demand fallback err:")
rescue_nobudget=$(count "fallback skipped (budget exhausted)")
pid_capped=$(count "plantid-budget:")
pn_slow=$(grep -F "identify plantnet down:" <<<"$LOG" | grep -cF "reason=slow" || true)
pid_takeover=$(( $(count "identify plantnet fallback:") + $(count "并行叫 Plant.id") ))
pnfail=$(count "identify plantnet down:")
race_lost=$(grep -F "identify engine race:" <<<"$LOG" | grep -cF "winner=" || true)
rescued_ok=$(count "rescued=true")
genus_kept=$(count "identify genus-keep:")

fire=0
[ "${c502:-0}" -ge 1 ] && fire=1
[ "${slow200:-0}" -ge 1 ] && fire=1
[ "${rescue_err:-0}" -ge 1 ] && fire=1
[ "${rescue_nobudget:-0}" -ge 1 ] && fire=1
[ "${pnfail:-0}" -ge 2 ] && fire=1
[ "$fire" = "0" ] && exit 0

now=$(date +%s)
last=$(cat "$COOLDOWN_FILE" 2>/dev/null || echo 0)
if [ $((now - last)) -lt "$COOLDOWN_SECS" ]; then
    logger -t yardmate-identify-alert "anomaly detected but in cooldown ($(((now-last)/60))min ago): 502=$c502 slow200=$slow200 pnfail=$pnfail"
    exit 0
fi
echo "$now" > "$COOLDOWN_FILE"

SUBJECT="identify 异常: 502=${c502} slow200=${slow200} PlantNet失败=${pnfail}"
BODY="最近 11 分钟 /v1/identify 异常汇总：

  502 错误           : ${c502}
  200 但 >${SLOW_SECS}s（危险区）: ${slow200}
  GPT 兜底失败       : ${rescue_err}
  兜底预算耗尽跳过   : ${rescue_nobudget}
  Pl@ntNet 失败      : ${pnfail}（其中 6s 没回: ${pn_slow}）
  后备抢先出结果     : ${race_lost}
  Plant.id 接管      : ${pid_takeover}
  Plant.id 上限挡下  : ${pid_capped}
  GPT 兜底救回       : ${rescued_ok}
  只认到属（保留属）: ${genus_kept}

解读：
  - Pl@ntNet失败多 + 救回多 = 上游 outage 但兜底在工作，观察即可
  - 502 / 兜底失败 > 0     = 用户正在看到错误，需要介入
  - slow200 > 0            = 响应逼近客户端 30s 计时器，用户可能已看不到结果
  - Plant.id 上限挡下 > 0  = 当日 Plant.id 额度（全站 / 单 IP）已用满，后续只靠 GPT

止血手段（上游持续挂时）：/etc/yardmate-api/secrets.env 设 PLANTID_IDENTIFY_FALLBACK=outage
并 systemctl restart yardmate-api → Pl@ntNet 故障期由 Plant.id 接管（走 credit，受
PLANTID_DAILY_CAP 全站 / PLANTID_PER_IP_DAILY_CAP 单 IP 每日上限约束）。

最近相关日志（最多 20 条）：
$(grep -E "identify (plantnet down|plantid-skip|plantnet fallback|plantnet hedge|engine race|plantid-budget|genus-keep|gpt on-demand fallback|upstream err)|plantnet trace|v1/identify.*- (502|200) " <<<"$LOG" | tail -20)

排查: journalctl -u yardmate-api --since '30 min ago' | grep identify
30 分钟冷却已启动，持续异常将再次提醒。"

"$NOTIFY" "$SUBJECT" "$BODY"
logger -t yardmate-identify-alert "alert sent: 502=$c502 slow200=$slow200 rescue_err=$rescue_err pnfail=$pnfail"
