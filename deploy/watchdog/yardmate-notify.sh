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

printf 'To: %s\nFrom: %s\nSubject: [YardMate] %s\n\n%s\n\n-- \nsent by %s at %s\n' \
    "$TO_HEADER" "$FROM" "$SUBJECT" "$BODY" "$HOST" "$(date)" \
    | msmtp -a gmail $RECIPIENTS \
    && logger -t yardmate-notify "sent: $SUBJECT -> $TO_HEADER" \
    || logger -t yardmate-notify "FAILED to send: $SUBJECT (check /var/log/msmtp.log)"
