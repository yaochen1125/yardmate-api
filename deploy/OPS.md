# 服务器运维配置（nginx / watchdog）

这里放的是 `5.78.183.252` 上**不属于 yardmate-api 二进制、但决定它能不能被访问到**的那部分配置。
`deploy.sh` 管的是 Go 服务本身；这里管的是它前面的 nginx 反代、以及盯着它的看门狗。

在此之前这些文件只存在于服务器上，改坏了只能靠手工备份找回。

## 文件

| repo | 服务器 | 是什么 |
|---|---|---|
| `nginx/api.yardmate.ai` | `/etc/nginx/sites-available/api.yardmate.ai` | API 反代到 `127.0.0.1:8080`，限流、9M body cap、路径白名单 |
| `nginx/yardmate.ai` | `/etc/nginx/sites-available/yardmate.ai` | 官网静态站 + `/content/` 同源反代 CDN |
| `nginx/api-staging.yardmate.ai` | `/etc/nginx/sites-available/api-staging.yardmate.ai` | staging API 反代到 `127.0.0.1:8081` |
| `nginx/conf.d/ratelimit.conf` | `/etc/nginx/conf.d/ratelimit.conf` | 限流 zone 定义（被 api vhost 引用） |
| `systemd/nginx.service.d/restart.conf` | 同名 drop-in | nginx 挂了自动拉起 |
| `watchdog/yardmate-healthcheck.sh` | `/usr/local/bin/` | 每 2 分钟探活 + nginx 自愈兜底 |
| `watchdog/yardmate-notify.sh` | `/usr/local/bin/` | 发告警邮件（msmtp → Gmail） |
| `watchdog/yardmate-healthcheck.{service,timer}` | `/etc/systemd/system/` | 驱动上面那个脚本 |
| `ops-agent/yardmate-ops-agent.py` | `/usr/local/bin/` | 告警邮件里那几个按钮的后端 |
| `ops-agent/yardmate-ops-agent.service` | `/etc/systemd/system/` | 跑上面那个服务 |

**不纳管**：`/etc/yardmate-alert/smtp-pass` 和 `/etc/yardmate-alert/ops-secret`（密钥）、certbot 维护的证书、`/etc/nginx/nginx.conf`（发行版默认未改动）、`sites-available/` 里的 `default` 和 `id-photo*`（与 YardMate 无关的历史遗留，未链接到 sites-enabled 所以不生效）。

## 用法

```bash
./deploy/ops-sync.sh --check    # 对比服务器与 repo，漂移则 exit 1
./deploy/ops-sync.sh --pull     # 服务器 → repo（有人手改过，收回版本管理）
./deploy/ops-sync.sh --push     # repo → 服务器（自动 nginx -t，不过则回滚）
```

`--push` **不会自动 reload**。推完 `nginx -t` 通过后由你手动执行，避免脚本在你没看日志时就把改动生效：

```bash
ssh root@5.78.183.252 'systemctl daemon-reload && systemctl reload nginx'
```

改了 `.service` / `.timer` / drop-in 需要 `daemon-reload`；只改 watchdog 脚本则什么都不用做（下一个 timer tick 自动用新的）。

**定期跑 `--check`。** 把文件塞进 git 只解决了「有备份」，没解决「git 里这份是不是真的」。只要有人 ssh 上去顺手改一行，repo 就变成一份看起来权威、实际过期的假真相 —— 下次照着它 `--push` 会把线上的修复覆盖掉。

## 三个陷阱

### 1. 公网只放行 `/v1/*` 和 `/healthz`

`server.go` 和 `proxy/imageingest/SPEC.md` 都写着 `/internal/*` 是「internal-only, behind nginx」「unreachable externally even before the token check」，并**据此让 `/internal/imageingest/run` 故意不挂限流中间件**。

但两个 vhost 此前都是 `location /` 全转发，那个前提根本不成立 —— 该端点公网可达且零限流，只剩 admin token 一道防线（实测返回 401 而非 404）。这不是「有人写错了」，是文档描述的边界从来没有被真正实现过。

现在改成显式白名单，其余路径（含 `/internal/*`）一律 404。运维要用内部端点，**在服务器本机调**：

```bash
curl -X POST -H "X-Ingest-Admin-Token: $TOKEN" \
  'http://127.0.0.1:8080/internal/imageingest/run?slug=xxx&name=Xxx+yyy'
```

**以后往 `/v1` 之外挂新路由，记得同步改这里，否则它不会被公网访问到。** 这是有意的默认拒绝。

### 2. `sites-enabled` 三个 vhost 现在都是 symlink

历史上 `api.yardmate.ai` 在 `sites-enabled` 里是**独立的普通文件**，而 `sites-available` 里还躺着一份同名影子副本 —— 改影子那份不生效，会出现「明明改了却没效果」的鬼故事。

**已根治**：转换前先 `diff` 确认两份一致，再换成 symlink。`--check` 保留了这项检测（会报「问题不存在了」），万一将来有人又把它改回普通文件，能立刻发现。

### 3. nginx 的 upstream 别写字面域名

`proxy_pass https://images.yardmate.ai/...` 这种**字面域名**，nginx 会在**启动时**强制解析，解析不出来就 `[emerg] host not found in upstream`，**拒绝启动整个 nginx** —— 不是只让那一个 location 失败。

2026-07-28 06:03 UTC 就是这么挂的：unattended-upgrades 重启 nginx，恰好那一刻 DNS 抖了一下，nginx 起不来，`api.yardmate.ai` 和官网一起躺了 **22 小时**。期间 `yardmate-api` 进程一直 `active`（它只监听 `127.0.0.1:8080`），死的是前面的反代 —— 只看 api 状态完全看不出问题。

所以 `yardmate.ai` 里那条改成了变量 + resolver，域名在**请求期**才解析：

```nginx
location ~ ^/content/(.*)$ {
    resolver 127.0.0.53 valid=300s;
    set $cdn_host images.yardmate.ai;
    proxy_pass https://$cdn_host/content/$1$is_args$args;
    proxy_ssl_name $cdn_host;
}
```

以后**任何指向外部域名的 `proxy_pass` 都用这个写法**。DNS 抖动最多让这一个 location 502，不会拖垮 nginx 启动。

## 挂了以后会发生什么

```
nginx 倒下
  ├─ systemd    5 秒一次，5 分钟内最多 10 次，用完永久躺平   ← 挡瞬时抽风
  ├─ watchdog   每 2 分钟一次，永不放弃                      ← 接住 systemd 放弃后的长尾
  │             但先 nginx -t：语法不过就不重试（配置写错重试一万次也没用），只告警
  └─ 邮件       故障立刻一封 + 持续期间每 15 分钟一封 + 恢复一封 + 自愈也发一封
```

收件人写在 `watchdog/yardmate-notify.sh` 的 `RECIPIENTS`（空格分隔）。SMTP 密码在服务器 `/etc/yardmate-alert/smtp-pass`，不入 git；该文件缺失时 notify 脚本静默跳过（exit 0），不会让 systemd 报错。

自愈成功也会发邮件 —— 静默自愈会掩盖一个反复发生却没人知道的问题。

## 手机上的介入手段（告警邮件按钮）

自愈覆盖不了的场景（配置写错、crash loop、发版翻车），以前只能开电脑 ssh。现在每封告警邮件底部都带四个链接：

| 动作 | 性质 | 说明 |
|---|---|---|
| 查看状态 | 只读，24 小时有效，可反复点 | 服务状态、公网 healthz、磁盘内存、nginx/api 最近日志 |
| 重启 nginx | 一次性，15 分钟 | watchdog 的兜底（比如它因配置语法错误拒绝自动重试） |
| 重启 yardmate-api | 一次性，15 分钟 | 覆盖「进程活着但行为不对」 |
| 回滚到上一个版本 | 一次性，15 分钟 | 换回 `yardmate-api.prev` 并重启。crash loop 是自动重启唯一救不了的 |

密钥 `/etc/yardmate-alert/ops-secret`（0600，32 字节随机）。轮换它会让所有在途链接立即失效 —— 邮件泄露时就该这么做：

```bash
ssh root@5.78.183.252 'head -c 32 /dev/urandom | base64 > /etc/yardmate-alert/ops-secret'
```

### 四道防线，各挡一种真实的失败方式

1. **只监听 `127.0.0.1:8090`**，公网经 nginx 的 `/ops/` 白名单进来，那条 location 限流卡到 `1r/s burst=5`（实测第 7 次起 429）。正常使用频率是「一天零次，出事时点几下」，所以可以卡得比 API 严得多。
2. **HMAC-SHA256 签名**，改动链接里任何一个字符都会被拒。
3. **写操作一次性 + 15 分钟过期**；nonce 记在 `/run`（tmpfs，重启即清，反正 token 更短命）。
4. **GET 只出确认页，POST 才执行。** 这条最容易被漏掉：Gmail 会预抓取邮件内容，企业邮件网关会主动访问链接做安全扫描。GET 直接执行的话，一封「nginx 挂了」的告警可能在你看到之前就自己把服务重启了。

### 一个反直觉的实现细节

`restart-nginx` 是**异步**执行的（先把结果页发完，再延后 1 秒动手）。因为这个请求本身正是经 nginx 进来的 —— 同步重启会把你自己这条 TCP 连接掐掉，浏览器只显示「无法访问此网站」。操作其实成功了，但手机上看起来像失败，而一次性链接已经用掉，人会以为搞砸了、开始慌。所以它的结果页给的是一个「查看状态」链接，让你几秒后自己确认。

`restart-api` / `rollback-api` 不影响自身连接，同步执行并直接显示结果。

### 边界

这是**四个预设动作**，不是远程 shell。有意为之：一个能跑任意命令的入口，制造的问题比它能解决的多。要做别的，还是 ssh。

## 排查顺序

App 报网络问题、但图片和库内容正常时（图片走 Cloudflare 不经这台服务器，这个反差极易误判成客户端问题）：

```bash
systemctl is-active nginx          # ← 先查这个，不是 yardmate-api
journalctl -u nginx -n 40 --no-pager
curl -sS -m 10 -o /dev/null -w '%{http_code}\n' https://api.yardmate.ai/healthz
```

判断线上是否可达**必须走公网 curl**。`systemctl is-active yardmate-api` 显示 active 不代表用户能访问到 —— 它只监听 `127.0.0.1`。
