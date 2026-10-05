#!/usr/bin/env bash
# Send an alert email via msmtp (Gmail relay). Usage: yardmate-notify.sh "subject" "body"
# No-ops gracefully (logs to syslog, exit 0) if the SMTP app password isn't set
# yet, so callers/systemd never fail just because alerting isn't provisioned.
set -uo pipefail

# 多收件人：空格分隔传给 msmtp，逗号分隔写进 To: 头。
RECIPIENTS="luckyao1125@gmail.com chen650450@gmail.com emanon.me@gmail.com"
FROM=luckyao1125@gmail.com
SUBJECT="${1:-YardMate alert}"
BODY="${2:-}"
HOST=$(hostname)

TO_HEADER=$(echo "$RECIPIENTS" | tr ' ' ',')

if [ ! -s /etc/yardmate-alert/smtp-pass ]; then
    logger -t yardmate-notify "smtp-pass not set — skipping email: $SUBJECT"
    exit 0
fi

# 操作链接：让人在手机上就能介入，不必开电脑（2026-07-28 那次 22 小时的直接
# 教训）。ops-agent 或密钥缺失时整段留空 —— 告警本身永远比按钮重要，绝不能因
# 为按钮生成失败就发不出告警。
ACTIONS=""
if [ -s /etc/yardmate-alert/ops-secret ] && [ -f /usr/local/bin/yardmate-ops-agent.py ]; then
    links=$(python3 /usr/local/bin/yardmate-ops-agent.py --links-text 2>/dev/null)
    if [ -n "$links" ]; then
        ACTIONS="
---- 手机上可直接操作 ----

$links
链接打开后是确认页，不会立刻执行 —— 需要在页面上再点一次。
"
    fi
fi

printf 'To: %s\nFrom: %s\nSubject: [YardMate] %s\n\n%s\n%s\n-- \nsent by %s at %s\n' \
    "$TO_HEADER" "$FROM" "$SUBJECT" "$BODY" "$ACTIONS" "$HOST" "$(date)" \
    | msmtp -a gmail $RECIPIENTS \
    && logger -t yardmate-notify "sent: $SUBJECT -> $TO_HEADER" \
    || logger -t yardmate-notify "FAILED to send: $SUBJECT (check /var/log/msmtp.log)"
