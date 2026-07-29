# 服务器运维配置（nginx / watchdog）

这里放的是 `5.78.183.252` 上**不属于 yardmate-api 二进制、但决定它能不能被访问到**的那部分配置。
`deploy.sh` 管的是 Go 服务本身；这里管的是它前面的 nginx 反代、以及盯着它的看门狗。

在此之前这些文件只存在于服务器上，改坏了只能靠手工备份找回。

## 文件

| repo | 服务器 | 是什么 |
|---|---|---|
| `nginx/api.yardmate.ai` | `/etc/nginx/sites-enabled/api.yardmate.ai` | API 反代到 `127.0.0.1:8080`，限流、9M body cap |
| `nginx/yardmate.ai` | `/etc/nginx/sites-available/yardmate.ai` | 官网静态站 + `/content/` 同源反代 CDN |
| `nginx/api-staging.yardmate.ai` | `/etc/nginx/sites-available/api-staging.yardmate.ai` | staging API 反代到 `127.0.0.1:8081` |
| `nginx/conf.d/ratelimit.conf` | `/etc/nginx/conf.d/ratelimit.conf` | 限流 zone 定义（被 api vhost 引用） |
| `systemd/nginx.service.d/restart.conf` | 同名 drop-in | nginx 挂了自动拉起 |
| `watchdog/yardmate-healthcheck.sh` | `/usr/local/bin/` | 每 2 分钟探活 + nginx 自愈兜底 |
| `watchdog/yardmate-notify.sh` | `/usr/local/bin/` | 发告警邮件（msmtp → Gmail） |
| `watchdog/yardmate-healthcheck.{service,timer}` | `/etc/systemd/system/` | 驱动上面那个脚本 |

**不纳管**：`/etc/yardmate-alert/smtp-pass`（密钥）、certbot 维护的证书、`/etc/nginx/nginx.conf`（发行版默认未改动）、`sites-available/` 里的 `default` 和 `id-photo*`（与 YardMate 无关的历史遗留，未链接到 sites-enabled 所以不生效）。

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

## 两个陷阱

### 1. `sites-enabled/api.yardmate.ai` 是普通文件，不是 symlink

另外两个 vhost 都是 `sites-enabled/x -> sites-available/x` 的软链，唯独 api 这个是**独立的普通文件**，而 `sites-available/` 里还躺着一份同名的影子副本。

**改影子那份不会生效。** 目前两份内容一致，但一旦有人改错地方，就会出现「明明改了却没效果」的鬼故事。`--check` 每次都会检测这两份是否分叉。

要根治就把它换成 symlink（`--check` 会识别并报「问题不存在了」），但那是个独立的改动，需要单独验证。

### 2. nginx 的 upstream 别写字面域名

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

## 排查顺序

App 报网络问题、但图片和库内容正常时（图片走 Cloudflare 不经这台服务器，这个反差极易误判成客户端问题）：

```bash
systemctl is-active nginx          # ← 先查这个，不是 yardmate-api
journalctl -u nginx -n 40 --no-pager
curl -sS -m 10 -o /dev/null -w '%{http_code}\n' https://api.yardmate.ai/healthz
```

判断线上是否可达**必须走公网 curl**。`systemctl is-active yardmate-api` 显示 active 不代表用户能访问到 —— 它只监听 `127.0.0.1`。
