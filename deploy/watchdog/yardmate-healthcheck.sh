#!/usr/bin/env bash
# Watchdog: curl the public healthz through the full nginx+TLS path.
#
# 告警节奏：healthy→down 立刻发一封；持续 down 期间每 REPEAT_SECS(15min) 再发一封
# 提醒（带已持续时长），down→healthy 发一封恢复。2026-07-28 那次只在故障开始时发了
# 一封，22 小时里再无提醒，邮件被淹没后就等同于没有告警。
#
# nginx 自愈：systemd 的 Restart=on-failure 只在 5min 内扶 10 次，用完就永久躺平。
# 这里做兜底 —— 每 2 分钟一次、永不放弃。但带闸门：先 `nginx -t`，语法通过才拉
# （环境类偶发问题重试有意义），语法不过说明配置真写错了，重试无用，只告警。
set -uo pipefail

URL=https://api.yardmate.ai/healthz
STATE=/run/yardmate-health.state   # tmpfs; resets to "up" assumption on reboot
NOTIFY=/usr/local/bin/yardmate-notify.sh
REPEAT_SECS=900                    # 持续故障重复提醒间隔（15 分钟）

probe() {
    # 双栈重试时 curl 会多次输出 %{http_code}，只取最后三位
    curl -s -o /dev/null -m 10 -w "%{http_code}" "$URL" 2>/dev/null | tail -c 3 || echo 000
}

code=$(probe)
code="${code: -3}"   # 兜底：不论 curl 输出几遍 http_code，只保留最后三位
now=$(date +%s)
nginx_note=""

# ---- nginx 自愈兜底（仅在探测失败时介入）----
if [ "$code" != "200" ] && ! systemctl is-active --quiet nginx; then
    if nginx -t >/dev/null 2>&1; then
        logger -t yardmate-healthcheck "nginx down, config valid -> self-heal attempt"
        systemctl reset-failed nginx >/dev/null 2>&1
        systemctl start nginx >/dev/null 2>&1
        sleep 2
        if systemctl is-active --quiet nginx; then
            logger -t yardmate-healthcheck "nginx self-heal SUCCEEDED"
            nginx_note="
[watchdog] 检测到 nginx 已停止且配置语法正常，已自动拉起成功。
建议查 journalctl -u nginx 看它当初为什么倒下。
"
            code=$(probe); code="${code: -3}"
        else
            logger -t yardmate-healthcheck "nginx self-heal FAILED (start did not stick)"
            nginx_note="
[watchdog] nginx 已停止，配置语法正常，但自动拉起失败 —— 需要人工介入。
  journalctl -u nginx -n 40 --no-pager
"
        fi
    else
        logger -t yardmate-healthcheck "nginx down but config INVALID -> not restarting"
        nginx_note="
[watchdog] nginx 已停止，且 nginx -t 配置语法检查不通过 —— 重试无用，不会自动拉起。
需要人工修配置：
  nginx -t
备份在 /root/nginx-backups/
"
    fi
fi

# state 格式："<up|down> <down_since_epoch> <last_notify_epoch>"
prev_status=up
down_since=0
last_notify=0
if [ -s "$STATE" ]; then
    read -r prev_status down_since last_notify < "$STATE" 2>/dev/null || true
fi
prev_status=${prev_status:-up}
down_since=${down_since:-0}
last_notify=${last_notify:-0}

hint="Check on the box:
  systemctl status yardmate-api nginx
  journalctl -u nginx -n 40 --no-pager
  journalctl -u yardmate-api -n 80 --no-pager"

if [ "$code" = "200" ]; then
    if [ "$prev_status" != "up" ]; then
        mins=$(( (now - down_since) / 60 ))
        "$NOTIFY" "RECOVERED: healthz 200" \
"api.yardmate.ai/healthz is back to 200 after ~${mins} min of downtime.
$nginx_note"
    elif [ -n "$nginx_note" ]; then
        # 上一轮还是 up，本轮 nginx 却倒过又被自己扶起来了（两次探测之间的短故障）。
        # 静默会掩盖问题 —— 这种「自己救回来了」必须让人知道，否则会反复发生而无人察觉。
        "$NOTIFY" "SELF-HEALED: nginx 曾停止，已自动恢复" \
"healthz 当前 200，但本轮 watchdog 发现 nginx 处于停止状态并介入了。
$nginx_note"
    fi
    printf 'up 0 0\n' > "$STATE"
else
    if [ "$prev_status" = "up" ]; then
        "$NOTIFY" "DOWN: healthz=$code" \
"api.yardmate.ai/healthz returned $code (expected 200).
The service or nginx may be down, crash-looping, or overloaded.
$nginx_note
$hint"
        printf 'down %s %s\n' "$now" "$now" > "$STATE"
    elif [ $(( now - last_notify )) -ge "$REPEAT_SECS" ]; then
        mins=$(( (now - down_since) / 60 ))
        "$NOTIFY" "STILL DOWN (${mins}min): healthz=$code" \
"api.yardmate.ai/healthz is STILL returning $code — down for ~${mins} minutes.
$nginx_note
$hint"
        printf 'down %s %s\n' "$down_since" "$now" > "$STATE"
    else
        printf 'down %s %s\n' "$down_since" "$last_notify" > "$STATE"
    fi
fi
