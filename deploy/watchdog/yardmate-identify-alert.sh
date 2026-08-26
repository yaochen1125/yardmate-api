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
#   - Pl@ntNet 失败（plantid-skip）≥ 2       （上游 outage 进行中，即使兜底救回）
# 30 分钟冷却：持续 outage 期间最多每半小时一封，不淹邮箱（healthz watchdog
# 的 2026-07-28 教训是只发一封会被淹没；这里反向教训是别变成邮件炸弹）。
set -uo pipefail

WINDOW="11 min ago"
NOTIFY=/usr/local/bin/yardmate-notify.sh
COOLDOWN_FILE=/run/yardmate-identify-alert.last   # tmpfs, 重启即清
COOLDOWN_SECS=1800
SLOW_SECS=18

LOG=$(journalctl -u yardmate-api --since "$WINDOW" --no-pager 2>/dev/null | grep -F "identify" || true)
[ -z "$LOG" ] && exit 0

count() { grep -cF "$1" <<<"$LOG" 2>/dev/null || true; }

c502=$(grep -cE 'v1/identify HTTP/1\.[01]" from [0-9.]+ - 502 ' <<<"$LOG" || true)
slow200=$(grep -E 'v1/identify HTTP/1\.[01]" from [0-9.]+ - 200 ' <<<"$LOG" \
    | awk -v t="$SLOW_SECS" '{d=$NF; sub(/s$/,"",d); if (d+0 > t) n++} END {print n+0}')
rescue_err=$(count "gpt on-demand fallback err:")
rescue_nobudget=$(count "fallback skipped (budget exhausted)")
pnfail=$(count "identify plantid-skip:")
rescued_ok=$(count "rescued=true")

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
  Pl@ntNet 失败      : ${pnfail}
  GPT 兜底救回       : ${rescued_ok}

解读：
  - Pl@ntNet失败多 + 救回多 = 上游 outage 但兜底在工作，观察即可
  - 502 / 兜底失败 > 0     = 用户正在看到错误，需要介入
  - slow200 > 0            = 响应逼近客户端 30s 计时器，用户可能已看不到结果

止血手段（上游持续挂时）：/etc/yardmate-api/secrets.env 置空 PLANTNET_API_KEY
并 systemctl restart yardmate-api → Plant.id 接管（走 credit，恢复后翻回）。

最近相关日志（最多 20 条）：
$(grep -E "identify (plantid-skip|gpt on-demand fallback|upstream err)|v1/identify.*- (502|200) " <<<"$LOG" | tail -20)

排查: journalctl -u yardmate-api --since '30 min ago' | grep identify
30 分钟冷却已启动，持续异常将再次提醒。"

"$NOTIFY" "$SUBJECT" "$BODY"
logger -t yardmate-identify-alert "alert sent: 502=$c502 slow200=$slow200 rescue_err=$rescue_err pnfail=$pnfail"
