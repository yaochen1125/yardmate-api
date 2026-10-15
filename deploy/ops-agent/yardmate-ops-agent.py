#!/usr/bin/env python3
"""
yardmate-ops-agent — 让告警邮件里的按钮能在手机上完成有限的几件运维动作。

背景：2026-07-28 nginx 挂了 22 小时，人不在电脑前就等于没有介入手段。这个
服务补上那条路径 —— 但刻意做得很窄：不是远程 shell，是四个预设动作。

安全设计（每一条都对应一种真实的失败方式）：

1. 只监听 127.0.0.1:8090，公网经 nginx 的 /ops/ 白名单进来，nginx 上另有限流。
2. 链接带 HMAC-SHA256 签名，密钥在 /etc/yardmate-alert/ops-secret（0600）。
3. 写操作 15 分钟过期 + nonce 一次性；只读的 status 可重复用、给 24 小时。
4. **GET 只渲染确认页，POST 才执行。** 这不是多此一举：Gmail 会预抓取邮件
   内容，企业邮件网关会主动访问链接做安全扫描。GET 直接执行的话，一封「nginx
   挂了」的告警可能在你看到之前就自己把服务重启了。预取只会拿到一个静态页面。
5. 动作是写死的白名单，不接受任何用户输入拼进命令。
"""
import hashlib
import hmac
import html
import json
import os
import secrets
import shlex
import subprocess
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET_FILE = "/etc/yardmate-alert/ops-secret"
NONCE_FILE = "/run/yardmate-ops-used-nonces"   # tmpfs：重启即清，token 本就短命
LISTEN = ("127.0.0.1", 8090)

WRITE_TTL = 15 * 60        # 写操作链接有效期
READ_TTL = 24 * 60 * 60    # 只读诊断链接有效期

# 白名单动作。exec 是不带 shell 的参数列表 —— 没有任何位置能拼进外部输入。
ACTIONS = {
    "status": {
        "label": "查看状态",
        "readonly": True,
        "desc": "只读诊断：服务状态、磁盘内存、最近日志、端点探测。不改变任何东西。",
    },
    "restart-nginx": {
        "label": "重启 nginx",
        "readonly": False,
        # detach：这个动作会掐断你自己这条 HTTP 连接（请求正是经 nginx 进来的），
        # 同步执行的话浏览器只会显示「无法访问此网站」—— 操作其实成功了，但手机
        # 上看起来像失败，而一次性链接已经用掉，人会以为搞砸了。所以先把结果页
        # 发完，再延迟到后台执行。
        "detach": True,
        "desc": "重启反代。watchdog 通常已自动做过；这是它因配置语法错误而拒绝重试时的兜底。",
        "exec": [["/usr/bin/systemctl", "reset-failed", "nginx"],
                 ["/usr/bin/systemctl", "restart", "nginx"]],
    },
    "restart-api": {
        "label": "重启 yardmate-api",
        "readonly": False,
        "desc": "重启 API 服务。覆盖「进程活着但行为不对」的情况。",
        "exec": [["/usr/bin/systemctl", "reset-failed", "yardmate-api"],
                 ["/usr/bin/systemctl", "restart", "yardmate-api"]],
    },
    "rollback-api": {
        "label": "回滚到上一个版本",
        "readonly": False,
        "desc": "把 yardmate-api.prev 换回来并重启。发版后发现问题时用 —— crash loop 是"
                "自动重启唯一救不了的故障类型。",
        "exec": [["/bin/cp", "-a", "/usr/local/bin/yardmate-api.prev",
                  "/usr/local/bin/yardmate-api"],
                 ["/usr/bin/systemctl", "restart", "yardmate-api"]],
    },
}


def load_secret():
    with open(SECRET_FILE, "rb") as f:
        return f.read().strip()


def sign(action, exp, nonce):
    payload = f"{action}:{exp}:{nonce}".encode()
    return hmac.new(load_secret(), payload, hashlib.sha256).hexdigest()


def make_link(base, action):
    ttl = READ_TTL if ACTIONS[action]["readonly"] else WRITE_TTL
    exp = int(time.time()) + ttl
    nonce = secrets.token_hex(8)
    sig = sign(action, exp, nonce)
    q = urllib.parse.urlencode({"action": action, "exp": exp, "n": nonce, "sig": sig})
    return f"{base}/ops/a?{q}"


def nonce_used(nonce):
    try:
        with open(NONCE_FILE) as f:
            return nonce in f.read().split()
    except FileNotFoundError:
        return False


def mark_nonce(nonce):
    with open(NONCE_FILE, "a") as f:
        f.write(nonce + "\n")


def verify(params):
    """返回 (action, error)。error 非 None 时拒绝。"""
    action = params.get("action", [""])[0]
    exp = params.get("exp", ["0"])[0]
    nonce = params.get("n", [""])[0]
    sig = params.get("sig", [""])[0]

    if action not in ACTIONS:
        return None, "未知动作"
    if not exp.isdigit():
        return None, "参数格式错误"
    if not hmac.compare_digest(sign(action, exp, nonce), sig):
        return None, "签名无效 —— 链接被改动过，或密钥已轮换"
    if int(exp) < time.time():
        return None, "链接已过期。到服务器上重发一封告警，或直接 ssh 处理。"
    return action, None


def run(cmds, timeout=60):
    out = []
    for c in cmds:
        p = subprocess.run(c, capture_output=True, text=True, timeout=timeout)
        tag = "ok" if p.returncode == 0 else f"exit={p.returncode}"
        out.append(f"$ {' '.join(c)}  [{tag}]\n{p.stdout}{p.stderr}".rstrip())
    return "\n\n".join(out)


def collect_status():
    def sh(cmd):
        try:
            p = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
            return (p.stdout + p.stderr).strip()
        except Exception as e:                                  # noqa: BLE001
            return f"(失败: {e})"

    parts = [
        ("服务", sh(["/usr/bin/systemctl", "is-active", "nginx", "yardmate-api",
                     "yardmate-healthcheck.timer"])),
        ("公网探测", sh(["/usr/bin/curl", "-sS", "-m", "10", "-o", "/dev/null",
                         "-w", "healthz=%{http_code} time=%{time_total}s",
                         "https://api.yardmate.ai/healthz"])),
        ("磁盘", sh(["/bin/df", "-h", "/"])),
        ("内存", sh(["/usr/bin/free", "-m"])),
        ("nginx 日志", sh(["/usr/bin/journalctl", "-u", "nginx", "-n", "15",
                           "--no-pager"])),
        ("api 日志", sh(["/usr/bin/journalctl", "-u", "yardmate-api", "-n", "20",
                         "--no-pager"])),
    ]
    return "\n\n".join(f"=== {k} ===\n{v}" for k, v in parts)


PAGE = """<!doctype html><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>YardMate Ops</title>
<style>
body{{font:16px/1.6 -apple-system,system-ui,sans-serif;margin:0;padding:24px;
background:#f6f7f9;color:#1c1e21}}
.card{{max-width:640px;margin:0 auto;background:#fff;border-radius:14px;
padding:24px;box-shadow:0 1px 3px rgba(0,0,0,.12)}}
h1{{font-size:20px;margin:0 0 4px}}
.desc{{color:#5a6270;margin:0 0 20px}}
button{{width:100%;padding:16px;font-size:17px;font-weight:600;border:0;
border-radius:12px;background:#1b7f4d;color:#fff}}
button.warn{{background:#b3421a}}
pre{{background:#0f1115;color:#d8dde5;padding:14px;border-radius:10px;
overflow-x:auto;font-size:13px;white-space:pre-wrap;word-break:break-word}}
.err{{color:#b3261e;font-weight:600}}
.meta{{color:#78808d;font-size:13px;margin-top:18px}}
@media(prefers-color-scheme:dark){{
body{{background:#14161a;color:#e6e8eb}}.card{{background:#1d2025}}
.desc{{color:#9aa3af}}}}
</style>
<div class=card>{body}</div>
"""


class Handler(BaseHTTPRequestHandler):
    server_version = "yardmate-ops"

    def log_message(self, fmt, *args):
        print(f"[ops-agent] {self.address_string()} {fmt % args}", flush=True)

    def _send(self, code, body):
        page = PAGE.format(body=body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(page)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Referrer-Policy", "no-referrer")
        self.end_headers()
        self.wfile.write(page)

    def _error(self, msg):
        self._send(400, f"<h1>无法执行</h1><p class=err>{html.escape(msg)}</p>")

    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        if u.path == "/ops/healthz":
            self._send(200, "<h1>ops-agent 正常</h1>")
            return
        if u.path != "/ops/a":
            self._send(404, "<h1>404</h1>")
            return

        params = urllib.parse.parse_qs(u.query)
        action, err = verify(params)
        if err:
            self._error(err)
            return

        meta = ACTIONS[action]
        # 只读动作没有副作用，GET 直接出结果，省一次点击。
        if meta["readonly"]:
            self._send(200, f"<h1>{meta['label']}</h1>"
                            f"<pre>{html.escape(collect_status())}</pre>")
            return

        # 写操作：GET 只渲染确认页。邮件客户端 / 安全网关的预抓取到此为止。
        nonce = params.get("n", [""])[0]
        if nonce_used(nonce):
            self._error("这个链接已经用过了。一次性设计，防止重复触发。")
            return
        cls = "warn" if action == "rollback-api" else ""
        self._send(200, f"""
<h1>{meta['label']}</h1>
<p class=desc>{html.escape(meta['desc'])}</p>
<form method=post action="{html.escape(self.path)}">
  <button class="{cls}" type=submit>确认执行</button>
</form>
<p class=meta>链接一次性，15 分钟内有效。没点确认就什么都不会发生。</p>""")

    def do_POST(self):
        u = urllib.parse.urlparse(self.path)
        if u.path != "/ops/a":
            self._send(404, "<h1>404</h1>")
            return

        params = urllib.parse.parse_qs(u.query)
        action, err = verify(params)
        if err:
            self._error(err)
            return

        meta = ACTIONS[action]
        if meta["readonly"]:
            self._send(200, f"<h1>{meta['label']}</h1>"
                            f"<pre>{html.escape(collect_status())}</pre>")
            return

        nonce = params.get("n", [""])[0]
        if nonce_used(nonce):
            self._error("这个链接已经用过了。")
            return
        # 先标记再执行：万一执行中途超时断开，也不能让同一个链接被重放。
        mark_nonce(nonce)

        print(f"[ops-agent] EXEC {action}", flush=True)

        if meta.get("detach"):
            # 先把页面发完再动手，否则重启会把这条响应连同 TCP 连接一起掐掉。
            # 结果页给一个只读的 status 链接（可反复点），让人自己确认落地情况。
            check = make_link("https://api.yardmate.ai", "status")
            self._send(200, f"""
<h1>{meta['label']} — 已提交</h1>
<p class=desc>命令已下发，nginx 正在重启（约 1-2 秒）。这个动作会切断当前连接，
所以这里不等结果 —— 过几秒点下面的链接确认。</p>
<p><a href="{html.escape(check)}"><button type=button>查看状态</button></a></p>""")
            try:
                self.wfile.flush()
                self.connection.close()
            except Exception:                                   # noqa: BLE001
                pass
            cmd = "sleep 1; " + "; ".join(shlex.join(c) for c in meta["exec"])
            subprocess.Popen(["/bin/sh", "-c", cmd],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            return

        try:
            out = run(meta["exec"])
        except Exception as e:                                  # noqa: BLE001
            self._error(f"执行出错: {e}")
            return

        after = run([["/usr/bin/systemctl", "is-active", "nginx", "yardmate-api"]])
        probe = subprocess.run(
            ["/usr/bin/curl", "-sS", "-m", "10", "-o", "/dev/null",
             "-w", "%{http_code}", "https://api.yardmate.ai/healthz"],
            capture_output=True, text=True)
        self._send(200, f"""
<h1>{meta['label']} — 已执行</h1>
<pre>{html.escape(out)}</pre>
<h1 style="font-size:16px">执行后</h1>
<pre>{html.escape(after)}
公网 healthz = {html.escape(probe.stdout.strip() or '?')}</pre>""")


def main():
    if not os.path.exists(SECRET_FILE):
        raise SystemExit(f"缺少密钥文件 {SECRET_FILE}")
    print(f"[ops-agent] listening on {LISTEN[0]}:{LISTEN[1]}", flush=True)
    HTTPServer(LISTEN, Handler).serve_forever()


if __name__ == "__main__":
    import sys
    # `--link <action> [base]` 供 notify 脚本生成签名链接
    if len(sys.argv) >= 3 and sys.argv[1] == "--link":
        base = sys.argv[3] if len(sys.argv) > 3 else "https://api.yardmate.ai"
        print(make_link(base, sys.argv[2]))
    elif len(sys.argv) >= 2 and sys.argv[1] == "--links-json":
        base = sys.argv[2] if len(sys.argv) > 2 else "https://api.yardmate.ai"
        print(json.dumps({a: make_link(base, a) for a in ACTIONS}))
    elif len(sys.argv) >= 2 and sys.argv[1] == "--links-text":
        # 供 notify 脚本嵌进纯文本邮件。只读的排最前 —— 多数时候该做的第一件事
        # 是看清楚，而不是动手。
        base = sys.argv[2] if len(sys.argv) > 2 else "https://api.yardmate.ai"
        order = sorted(ACTIONS, key=lambda a: (not ACTIONS[a]["readonly"], a))
        for a in order:
            mark = "（只读，可反复点）" if ACTIONS[a]["readonly"] else "（一次性，15 分钟内有效）"
            print(f"{ACTIONS[a]['label']}{mark}\n  {make_link(base, a)}\n")
    else:
        main()
