#!/usr/bin/env bash
#
# ops-sync.sh — 让服务器上的 nginx / watchdog 运维配置与本 repo 保持同一份真相。
#
#   ./deploy/ops-sync.sh --check    对比服务器与 repo，报出漂移（CI/例行巡检用，漂移时 exit 1）
#   ./deploy/ops-sync.sh --pull     服务器 → repo（服务器上手改过，把改动收回版本管理）
#   ./deploy/ops-sync.sh --push     repo → 服务器（改完 repo 部署上去；自动 nginx -t，不过则回滚）
#
# 为什么需要它：把文件塞进 git 只解决了「有备份」，没解决「git 里这份是不是真的」。
# 只要有人 ssh 上去顺手改一行，repo 就变成一份看起来权威、实际过期的假真相 —— 下次
# 照着它 --push 会把线上的修复覆盖掉。--check 就是防这个的，建议定期跑。
#
# 不管的东西：/etc/yardmate-alert/smtp-pass（密钥）、certbot 自己维护的证书文件、
# /etc/nginx/nginx.conf（发行版默认，没改过）。
set -uo pipefail

HOST="${YARDMATE_OPS_HOST:-root@5.78.183.252}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# repo 相对路径 | 服务器绝对路径 | 权限
MANIFEST=(
    "nginx/api.yardmate.ai|/etc/nginx/sites-available/api.yardmate.ai|644"
    "nginx/yardmate.ai|/etc/nginx/sites-available/yardmate.ai|644"
    "nginx/api-staging.yardmate.ai|/etc/nginx/sites-available/api-staging.yardmate.ai|644"
    "nginx/conf.d/ratelimit.conf|/etc/nginx/conf.d/ratelimit.conf|644"
    "systemd/nginx.service.d/restart.conf|/etc/systemd/system/nginx.service.d/restart.conf|644"
    "watchdog/yardmate-healthcheck.sh|/usr/local/bin/yardmate-healthcheck.sh|755"
    "watchdog/yardmate-notify.sh|/usr/local/bin/yardmate-notify.sh|755"
    "watchdog/yardmate-healthcheck.service|/etc/systemd/system/yardmate-healthcheck.service|644"
    "watchdog/yardmate-healthcheck.timer|/etc/systemd/system/yardmate-healthcheck.timer|644"
)

RED=$'\033[31m'; GRN=$'\033[32m'; YEL=$'\033[33m'; RST=$'\033[0m'
ok()   { echo "${GRN}✓${RST} $*"; }
warn() { echo "${YEL}!${RST} $*"; }
bad()  { echo "${RED}✗${RST} $*"; }

usage() { sed -n '2,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 1; }

# api.yardmate.ai 在 sites-enabled 里是普通文件而非 symlink（另两个 vhost 是
# symlink）。于是 sites-available 里那份同名文件是「影子」：改它不生效，改
# sites-enabled 才生效。两份一旦分叉，下一个人几乎必然改错地方。
check_shadow() {
    local out
    out=$(ssh -o ConnectTimeout=12 "$HOST" '
        if [ -L /etc/nginx/sites-enabled/api.yardmate.ai ]; then
            echo SYMLINK_NOW
        elif [ -f /etc/nginx/sites-available/api.yardmate.ai ]; then
            if diff -q /etc/nginx/sites-available/api.yardmate.ai \
                       /etc/nginx/sites-enabled/api.yardmate.ai >/dev/null 2>&1; then
                echo SHADOW_SAME
            else
                echo SHADOW_DIVERGED
            fi
        else
            echo NO_SHADOW
        fi' 2>/dev/null)
    # 返回值必须被 do_check 计入 drift：否则「所有 manifest 文件都一致、只有影子
    # 副本分叉」时会打印红叉却 exit 0，例行 --check 恰好告不了这个警 —— 而它正是
    # 为此加的。
    case "$out" in
        SYMLINK_NOW)     ok    "api.yardmate.ai 已是 symlink，影子副本问题不存在了"; return 0 ;;
        SHADOW_SAME)     warn  "sites-available/api.yardmate.ai 是影子副本（当前内容一致，但改它不生效 —— 要改请改 sites-enabled 那份）"; return 0 ;;
        SHADOW_DIVERGED) bad   "sites-available/api.yardmate.ai 与生效版本已分叉！有人改错了地方，改动没生效"; return 1 ;;
        NO_SHADOW)       ok    "无影子副本"; return 0 ;;
        *)               bad   "影子副本检查失败（连不上服务器？）"; return 1 ;;
    esac
}

do_check() {
    local drift=0
    for entry in "${MANIFEST[@]}"; do
        IFS='|' read -r rel remote _mode <<< "$entry"
        local local_file="$HERE/$rel"
        if [ ! -f "$local_file" ]; then
            bad "$rel — repo 里缺这个文件"; drift=1; continue
        fi
        # 必须重定向到文件，不能用 $(...) —— 命令替换会吞掉末尾换行符，
        # 对「文件末尾无换行」的配置（如 api-staging）会报出不存在的假差异。
        local remote_tmp
        remote_tmp=$(mktemp)
        ssh -o ConnectTimeout=12 "$HOST" "cat '$remote' 2>/dev/null" > "$remote_tmp"
        if [ ! -s "$remote_tmp" ]; then
            bad "$rel — 服务器上 $remote 不存在或为空"; drift=1; rm -f "$remote_tmp"; continue
        fi
        if diff -q "$remote_tmp" "$local_file" >/dev/null 2>&1; then
            ok "$rel"
        else
            bad "$rel — 与服务器不一致（repo 左 / 服务器右）"
            diff "$local_file" "$remote_tmp" | head -25
            drift=1
        fi
        rm -f "$remote_tmp"
    done
    echo
    check_shadow || drift=1
    echo
    if [ "$drift" -eq 0 ]; then
        ok "无漂移：repo 与服务器一致"
    else
        bad "检测到漂移 —— 先判断哪边是对的，再 --pull（认服务器）或 --push（认 repo）"
    fi
    return "$drift"
}

do_pull() {
    # 任何一个文件没拉下来都必须让整体失败：否则 repo 里会留下「一半新一半旧」
    # 的混合快照，而输出说「拉取完成」、退出码还是 0 —— 有人照着它 commit，
    # git 里那份就成了从未真实存在过的服务器状态。
    local failed=0
    for entry in "${MANIFEST[@]}"; do
        IFS='|' read -r rel remote _mode <<< "$entry"
        mkdir -p "$(dirname "$HERE/$rel")"
        if scp -q "$HOST:$remote" "$HERE/$rel"; then
            ok "拉回 $rel"
        else
            bad "拉取失败 $rel（repo 里仍是旧内容）"; failed=1
        fi
    done
    chmod +x "$HERE"/watchdog/*.sh 2>/dev/null
    echo
    if [ "$failed" -eq 0 ]; then
        ok "拉取完成 —— 用 git diff 看服务器上都被手改了什么"
        return 0
    fi
    bad "拉取不完整 —— repo 现在是新旧混合快照，别 commit，修好连接后重跑"
    return 1
}

rollback_push() {
    local backup_dir="$1"
    for entry in "${MANIFEST[@]}"; do
        IFS='|' read -r _rel remote _mode <<< "$entry"
        local flat="$backup_dir/$(echo "$remote" | tr / _)"
        ssh -o ConnectTimeout=12 "$HOST" \
            "[ -f '$flat' ] && cp -a '$flat' '$remote' || true"
    done
    if ssh -o ConnectTimeout=12 "$HOST" 'nginx -t >/dev/null 2>&1'; then
        ok "已回滚，配置恢复可用（线上未 reload，本就没受影响）"
    else
        bad "回滚后 nginx -t 仍失败 —— 需要人工上机处理，备份在 $backup_dir"
    fi
}

do_push() {
    local stamp backup_dir failed=0
    stamp=$(date +%Y%m%d-%H%M%S)
    backup_dir="/root/ops-sync-backups/$stamp"
    ssh -o ConnectTimeout=12 "$HOST" "mkdir -p '$backup_dir'" || { bad "无法在服务器建备份目录"; return 1; }

    for entry in "${MANIFEST[@]}"; do
        IFS='|' read -r rel remote mode <<< "$entry"
        local local_file="$HERE/$rel"
        [ -f "$local_file" ] || { bad "$rel — repo 里没有这个文件"; failed=1; continue; }
        # 先备份线上原件（扁平化文件名，避免目录层级）
        ssh -o ConnectTimeout=12 "$HOST" \
            "[ -f '$remote' ] && cp -a '$remote' '$backup_dir/$(echo "$remote" | tr / _)' || true"
        if scp -q "$local_file" "$HOST:$remote" && \
           ssh -o ConnectTimeout=12 "$HOST" "chmod $mode '$remote'"; then
            ok "推送 $rel → $remote"
        else
            bad "推送失败 $rel"; failed=1
        fi
    done

    # 任何一个文件没推成功就整体回滚。半推上去的状态最危险：nginx -t 可能对着
    # 旧文件通过（watchdog 脚本 / systemd unit 更是 nginx -t 根本不校验的），
    # 于是脚本报成功、还叫人去 reload，实际生效的是一份没人设计过的混合配置。
    if [ "$failed" -ne 0 ]; then
        echo
        bad "有文件未能推送 —— 整体回滚，不做部分部署"
        rollback_push "$backup_dir"
        return 1
    fi

    echo
    echo "--- nginx -t ---"
    ssh -o ConnectTimeout=12 "$HOST" 'nginx -t' 2>&1 | tail -2
    if ssh -o ConnectTimeout=12 "$HOST" 'nginx -t >/dev/null 2>&1'; then
        ok "配置语法通过"
        echo
        warn "本脚本不会自动 reload。确认无误后手动执行："
        echo "    ssh $HOST 'systemctl daemon-reload && systemctl reload nginx'"
        echo "  （只改了 watchdog 脚本则无需 reload nginx；改了 .service/.timer/drop-in 需要 daemon-reload）"
        echo "  备份在服务器 $backup_dir"
        return 0
    fi
    bad "nginx -t 未通过 —— 正在回滚"
    rollback_push "$backup_dir"
    return 1
}

case "${1:-}" in
    --check) do_check ;;
    --pull)  do_pull ;;
    --push)  do_push ;;
    *)       usage ;;
esac
