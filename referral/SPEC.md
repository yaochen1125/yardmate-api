# `referral` package — 推荐好友（/v1/referral/*）

> 产品文档（源真）：yardmate-swiftui `docs/releases/v1/main-navigation/more/invite-friends/invite-friends.md`。
> 接口路径、字段、错误码按那份文档原样；本文只写后端怎么实现。
> 照搬 Atta Kid 后端 `referral/`，按 YardMate 三处拍板改：推荐人奖励**不限量叠加**、
> **正在付费的推荐人先攒着**、身份**验 Supabase JWT**（不是 App 自报）。
> 推荐奖励（朋友真扣费的 30% → Amazon 礼品卡）在 `rewards/`，见 `rewards/SPEC.md`；本包只给 `/me` 留 `RewardsReader` 钩子（`rewards` 字段）、把 `/claim` 的设备桶借给 `/payout`。

## 1. 五个问题（`AI_ENGINEERING_STANDARD.md` §6）

### 1.1 负责什么

- `GET /v1/referral/me`：给调用者造邀请码（第一次调才造）+ 回战绩 + 记下这次用的设备（防刷第 2 道）。
- `POST /v1/referral/claim`：B 填 A 的码 → 按文档判定顺序过防刷第 1、2 道 → 记 A→B → 从码池发一张 App Store 一次性兑换码，
  同一事务里给这台设备置 DeviceCheck `bit0`（`devicecheck.go`）。
- 防刷五道里后端的四道（§2.4）：DeviceCheck、推荐人设备、同一订阅只算一次（结算 ①）、速度闸 → `held` + 运营邮件（结算 ②、`review.go`）。
- `POST /v1/referral/redeemed`：App 回报「兑了」→ 只是让结算早点去查，不作凭据。
- 结算定时器（每 10 分钟）：查 RevenueCat 推进状态，给推荐人赠会员（`settle.go`）。
- 码池巡检（结算每轮一次，`pool.go`）：算「可发」库存（free 且到期日在发码余量之外）、最早到期日、
  各批剩余；低库存 / 临期 → WARN 日志 + 每天最多一封告警邮件。
- 删账号时删掉这个人的邀请码、推荐记录（两侧）、奖励账本（`DeleteUser`，由 `account_delete.go` 调）。
- 三个接口各自的按设备限流桶（claim 5/24h，me + redeemed 30/h），自己扫。

### 1.2 不负责什么

- 验 JWT 的实现 —— 复用 `account_delete.go` 的 `verifySupabaseToken` + JWKS 缓存，
  经 `Authenticator` 注入（`referral_wiring.go`）；本包不认识 `SUPABASE_URL`。
- 按 IP 限流 —— `/v1` 作用域的 `PerIPMiddleware` 已覆盖。
- 码池补货 —— `deploy/supabase/import-codes.sh`（CSV → `code_pool`）；看现状 `deploy/referral-pool.sh`。
- 发邮件本身 —— 复用 `/v1/feedback` 的 `enrichment.FeedbackMailer`（Gmail SMTP，`FEEDBACK_SMTP_*` /
  `FEEDBACK_EMAIL_TO`），经 `Options.Alerter` 注入（`mail_wiring.go`）；本包只认识 `Alerter` 接口。
- 苹果兑换本身、App 端判「从没订阅过」（`Transaction.all`）、App Store Connect 的 offer 配置。
- 收回已发出的 RevenueCat 赠送（删账号也不收）。

### 1.3 输入

| 来源 | 内容 |
|---|---|
| 请求头 | `Authorization: Bearer <Supabase access token>`、`X-Device-Install-Id`（UUID）、`X-App-Version` |
| `claim` body（≤ 16 KB）| `{"code": "ym 7kq2", "subscribed_before": false, "device_token": "<DCDevice base64>", "device_check_env": "production"}`；`device_token` 可缺（模拟器 / 拿不到）。`device_check_env`：**`enforce` 只认 `production`**（大小写、首尾空格不计），`development` / 缺 / 其他值一律当没带 token；`log` 按请求的环境查（`development` → 开发环境地址，其余含缺省 → 生产）|
| `redeemed` body | `{"offer_code": "XK7P2M9Q"}` |
| vault | `REVENUECAT_SECRET_KEY`（v1 secret）、`REVENUECAT_ENTITLEMENT_ID`、`REFERRAL_SETTLE_INTERVAL`（可选，默认 10m）；DeviceCheck：`DEVICECHECK_KEY_ID`、`DEVICECHECK_PRIVATE_KEY`（.p8 压一行、`\n` 转义，同 `APPLE_PRIVATE_KEY`；或 `DEVICECHECK_PRIVATE_KEY_PATH`）、`APPLE_TEAM_ID`、`DEVICECHECK_MODE`（`enforce` / `log`，缺 = `enforce`）|
| 库 | migration `013_referral.sql` 的五张表 + 两个函数，`014_referral_pool_margin.sql` 的四列设置 + 改过的 `referral_claim`，`016_referral_antiabuse.sql` 的防刷列 / 两张表 / 九个函数（经共享 pgx 池 `enrichment.DB.Pool()`）|
| Apple DeviceCheck | `POST {api,api.development}.devicecheck.apple.com/v1/query_two_bits`、`/v1/update_two_bits`（ES256 JWT：`kid` = Key ID、`iss` = Team ID、`iat` = now；每次调用 3 s 超时）|
| 告警邮件 | `FEEDBACK_SMTP_FROM/PASS` + `FEEDBACK_EMAIL_TO`（与意见反馈同一个账号、同一个收件箱）；缺任一 → 只打 WARN 日志 |
| RevenueCat | `GET /v1/subscribers/{id}`（**不存在会创建**，只对验过签的 user id 调）、`POST .../entitlements/{ent}/promotional {"end_time_ms"}` |

### 1.4 输出

`/me` → `{code, invited, months_earned, months_pending, months_saved, can_claim}`；
`claim` → `{offer_code}`；`redeemed` → `{status:"ok"}`。错误体 `{"error":"<code>"}`：

| HTTP | code | 谁给的 |
|---|---|---|
| 400 | `missing_device_id` / `missing_app_version` / `bad_json` | 请求形状 |
| 400 | `bad_code` | `Normalise` 不认 / `offer_code` 不像码 |
| 400 | `unknown_code` / `own_code` / `already_claimed` | SQL `referral_claim_check` / `referral_claim` 的 `raise exception`；`own_code` 也包括「在码主人记过的设备上填」（第 2 道）|
| 400 | `not_new_user` | App 报 `subscribed_before`，或 RevenueCat 有本人的 store 订阅记录 |
| 400 | `device_used` | DeviceCheck `bit0` 已置：这台设备当过被推荐的朋友（第 1 道，只 `enforce`）|
| 400 | `device_unverified` | 没带 `device_token`、`device_check_env` 不是 `production`，或苹果答 400（token 无效）（第 1 道，只 `enforce`）|
| 401 | `unauthorized` | token 验签失败（不说哪一步） |
| 429 | `rate_limit_device` / `rate_limit_ip` | 本包的桶 / `/v1` 的 IP 桶 |
| 502 | `store_failed` | 库或 RevenueCat 不可达 |
| 503 | `pool_empty` | 码池没有**留够余量**的 free 码（`expires_at > now() + issue_margin_days`；补货后自愈）；不置 `bit0` |
| 503 | `device_check_unavailable` | 苹果超时（3 s）/ 非 200、400 的回答 / 没配 DeviceCheck，或置 `bit0` 失败（这次领码整体回滚，码仍 free）（只 `enforce`）|
| 503 | `not_configured` | 四个依赖缺任一个 |

### 1.5 外部依赖

`github.com/jackc/pgx/v5`（共享池）、`net/http`（RevenueCat、Apple DeviceCheck）、`github.com/golang-jwt/jwt/v5`（DeviceCheck 的 ES256 JWT）、本仓库 `ratelimit`。
不引 `chi`（路由在 `server.go` 挂）。

## 2. 一次推荐怎么走完

```
A /me ──造码 YM-XXXX──▶ A 分享
B /claim ──判定顺序 2–4（referral_claim_check）→ 5 RC 查 B 无订阅史 → 6 DeviceCheck 查 bit0──▶
        事务 { referral_claim(): referrals(A→B, claimed, claim_install_id) + code_pool 一张 issued
               → update_two_bits(bit0) 失败就回滚 } commit
                                （只发 expires_at > now + issue_margin_days 的，先到期的先发）
B 在苹果兑换 ──▶ B /redeemed（只记 redeem_reported_at，并 nudge 结算）
结算 ①：RC 上 B 有 store 订阅且 original_purchase_date > claimed_at → redeemed（redeemed_at = 那个时间，记 subscription_key）；
        同一 subscription_key 已被另一条没作废的推荐 / 删号留存表占用 → void（duplicate_apple_id，名下的钱一起作废）
结算 ②：redeemed_at + hold_days ≤ now，且该订阅 refunded_at 为空 → qualified；退款 → void；
        推荐人 velocity_window_hours 内已成功（含 held）≥ velocity_limit 条 → held（等运营），新 held 给运营发信
结算 ③：A 的 RC 没有生效中的 store 订阅 → 赠：end = max(now, 账本 promo_until, RC 现有赠送到期) + 30d
        A 有（付费 / 试用 / intro / 朋友的免费月 / 家人共享）→ saved
结算 ④：有 saved 的 A 不再被 store 订阅覆盖 → 一次赠 n × 30d，全部 saved → rewarded
```

RevenueCat 的 app user id = Supabase user id **大写**（iOS 传 `UUID.uuidString`，RC id 区分大小写），
见 `AppUserID`。

### 2.1 状态机（`referrals.state`）

`claimed → redeemed → qualified → (held →) (rewarded | saved → rewarded)`，任一步可 `void`。
`void_reason`：`refunded` / `no_subscription`（②）、`duplicate_apple_id`（① 第 3 道）、`rejected`（运营 reject）。
`held → qualified` 只能是运营 `approve`；③ 不再过闸。
`/me` 的数字：`invited` = 非 void 条数；`months_earned` = rewarded + saved；
`months_pending` = redeemed + qualified + **held**（推荐人看不出审核）；`months_saved` = saved；`can_claim` = 没填过 **且** RC 无订阅史。

### 2.2 幂等 / 并发

- 造码、领码是 plpgsql 函数（`raise exception` 整体回滚；`for update skip locked` 取库存）。
- 同一人同一码再 claim → 拿回同一张；已 redeemed 永远原样回；issued 且 `expires_at > now + swap_grace_days`
  （默认 3 天）也原样回。换码阈值故意**小于**发码余量：等于的话每张快到余量线的码都会被再 claim 换掉、白烧第二张。
- 要换（老码过期 / 不足宽限 / 状态不对 / 行没了）时**先选替换码再动老码**（老码行 `for update` 锁住）：
  选到 → 老码划 expired、`referrals.offer_code` 换新、`reissued_at = now()`（`ListClaimed` 的 200 天窗口按
  `coalesce(reissued_at, claimed_at)` 算，不然很晚才兑的会掉出去）；`claimed_at` 不动——结算 ① 认「订阅起始晚于第一次填码」，
  B 可能换码前刚用老码兑了、RC 还没反映，刷了它那笔就永远对不上（Codex P2）；选不到且老码在苹果那边还能用（issued 且 `expires_at > now()`）→ 原样回老码；老码也不能用了才
  `pool_empty`（首次 claim 的 raise 会回滚刚插的 referral，再 claim 的不动已有行）。
- 发码余量：`referral_claim` 只在 `state = 'free' and expires_at > now + issue_margin_days` 里按 `expires_at`
  升序取（先用快过期的，但保证 B 有 ≥ 余量天数去兑）。余量期内 / 已过期的 free 码不发也不改状态
  （claim 不再全表划 expired，那是并发争用点），等结算的 `ExpireCodes` 到期划；巡检把余量期内的单独计作「余量不足」。
- `/redeemed` 重复调无副作用；`ListClaimed` 只看最近一次发码（`coalesce(reissued_at, claimed_at)`）200 天内的（码最长 6 个月）。
- 赠送先 RC 后账本；账本写失败会大声记日志，下一轮**再赠一次**（宁多发不少发）。
- `end_time_ms` 与现有赠送到期相差 < 2h 会被 RC 当重复忽略；我们每次至少 +30d。

### 2.3 `/claim` 判定顺序（文档 `/claim 判定顺序`，先命中的先回）

1. 鉴权 401、请求形状 400（`missing_device_id` / `missing_app_version` / `bad_json` / `bad_code`）、设备桶 429
2. `unknown_code`
3. 这个账号填过：同一个码 → 回手上那张（快过期换新，014 规则；**不查 RevenueCat、不查 DeviceCheck**，换台设备也拿得回）；别的码 → `already_claimed`
4. `own_code`：码是自己的，或当前 `X-Device-Install-Id` 在码主人 `referral_devices` 里
5. `not_new_user`：`subscribed_before` 或 RevenueCat 有 store 订阅史
6. DeviceCheck（§2.4 第 1 道）
7. 码池取码：没码 `pool_empty`（不置位）；取到 → 同一事务置 `bit0`，失败回滚 → `device_check_unavailable`

2–4 先用只读的 `referral_claim_check` 判（这样 5、6 的外部调用只对「第一次填」发生、`pool_empty` 才排得到 5、6 后面），
真领时三参数 `referral_claim` 在事务里原子地再判一遍（并发两台设备同一账号时，晚的那个拿到 `fresh = false`，不再置位）。
和 #123/#124 的区别：`subscribed_before` 从「限流之前」挪到第 5 步（会扣一次当天次数——App 本来就不会让订阅过的人看到填码入口）；
重领不再先问 RevenueCat（以前兑换过的人重领会被 RC 答成 `not_new_user`）。作废了的推荐重领只回老码、**不换码**（免得被拒的人天天重领烧库存）。

### 2.4 防刷（文档 `### 防刷` 五道，后端管前四道 + 第 5 道的数据）

**第 1 道 DeviceCheck**（`devicecheck.go`，`handlers.go` `checkDevice` / `bitHook`）

| 情况 | `enforce`（prod）| `log`（staging）|
|---|---|---|
| 没配 DeviceCheck（缺 Key ID / Team ID / .p8，或 .p8 解析不了）| 新填码一律 503 `device_check_unavailable` | 不查不置，记日志放行 |
| 没带 `device_token`（或 > 12 KB / 有控制字符）| 400 `device_unverified` | 记日志放行，不置位 |
| `device_check_env` 不是 `production`（`development` / 缺 / 其他）| 当没带 token：400 `device_unverified`，不问苹果（开发环境的 bit 与生产分开存，认它等于每台设备两条命；Release 包一律带 `production`）| 照请求的环境查 / 置（`development` → `api.development.devicecheck.apple.com`），结果只记日志 |
| 苹果答 400（token 无效）| 400 `device_unverified` | 记日志放行，不置位 |
| 苹果超时 / 其他错误 | 503 `device_check_unavailable` | 记日志放行，照样尝试置位 |
| `bit0` 已置 | 400 `device_used` | 记日志放行，照样置位 |
| 取到码后置位失败 | 回滚领码（码仍 free）→ 503 `device_check_unavailable` | 记日志，领码照常提交 |

- 苹果「没有 bit 记录」是 200 + 纯文本（实测 `Failed to find bit state`，文档写 `Bit State Not Found`），都当 `bit0 = false`。
- `update_two_bits` 只发 `bit0: true`，不发 `bit1`（同一 Team 下别的 App 若也用 DeviceCheck，会共用这两个 bit）。
- 置位在领码事务里、commit 之前：置位失败 → 回滚；置位成功但 commit 失败（极少）→ 这台设备 `bit0` 已置但没领到码，大声记日志。
- 每次苹果调用 3 s（`DefaultDeviceCheckTimeout`，HTTP 客户端和 context 双重），一次新填码最多两次。

**第 2 道 推荐人用过的设备**：`/me` 每次成功 `referral_me(user, install)` upsert `referral_devices(user_id, install_id 小写, first/last_seen_at)`；
判定第 4 步只看**码主人自己**的设备。删账号随 `referral_users` 级联删。

**第 3 道 同一个 Apple ID 只算一次**（结算 ①，`referral_mark_redeemed`）：认定 redeemed 时记 `referrals.subscription_key`；
已有另一条**没作废**的推荐用了同一个 key，或 key 在 `referral_used_subscriptions`（删账号留下的，不带用户 ID）→ 这条 `void`（`duplicate_apple_id`），
不发月份，名下 `reward_ledger` 的 pending + available 一起标 `voided`（`void_reason` 同）。同一 key 用 advisory lock 串行，
partial unique index（`subscription_key where state <> 'void'`）兜底。
⚠️ **key 不是苹果的 `original_transaction_id`**：RevenueCat REST v1 / v2 的订阅记录都不返回它（v1 `store_transaction_id` 是最新一笔、每次续费都变；
只有 webhook 事件带 `original_transaction_id`，而 RevenueCat 转移到第二个账号的订阅不会给那个账号发 `INITIAL_PURCHASE`）。
所以用订阅记录里同一条订阅链换账号 / 转移 / 续费 / 组内升降级都不变的 `original_purchase_date`：`<store>:<original_purchase_date RFC3339>`
（`Subscriber.SubscriptionKey`，不含 product id）。代价：两个不同的人同一秒开始订阅会撞，被误作废一条（运营可见）。

**第 4 道 速度闸**（结算 ②，`referral_qualify`）：判成功时数这个推荐人 `velocity_window_hours` 小时内 `qualified_at` 的成功条数
（qualified / held / saved / rewarded，**含 held**），≥ `velocity_limit` → 这条 `held`（也记 `qualified_at`，所以 held 自己也算进后面的计数）。
即 24 小时内第 6 个起待审核。`velocity_limit = 0` = 闸关。锁推荐人 `referral_users` 行（`for no key update`）让同一推荐人的判定排队。
- 运营邮件（`review.go`）：每轮 ② 之后查「有没发过信的 held、且这个推荐人 24 小时内没收过信」的推荐人（`held_notified_at` 记在库里，重启不重发、发失败下轮重试）；
  每人一封：推荐人 ID、规则、待审核条数（本封新增几条）、这批朋友填码设备数了几台不同（`claim_install_id`，016 之前的没记）、待审核 + 窗口内成功的列表、
  approve / reject 命令。主题 ASCII（`[YardMate] Referral review: N held for one referrer`），正文中文。没配邮件 → 只打 WARN 并照样标记。
- 运营 `deploy/referral-review.sh list | approve <id> | reject <id>`（PostgREST + service role，rpc `referral_approve` / `referral_reject`）：
  `approve` = held → qualified + `reviewed_at`，下一轮 ③ 照常发 / 攒（推荐人的 `months_earned` 变了，App 照常发到账通知）；
  `reject` = 任何没作废的状态 → void（`rejected`）+ `reviewed_at`，名下的钱一起作废；已 rewarded 的 RevenueCat 赠送不收回（脚本提示）。

**第 5 道 礼品卡人工审核**：数据由 `rewards/` 在申请邮件里出（见 `rewards/SPEC.md`），本包只提供 `claim_install_id` / `void_reason` / `held`。

### 2.5 限流

| 接口 | 桶 | 在哪扣 |
|---|---|---|
| `/claim` | 5 / 24h / 设备 | handler 内，在 `bad_json` / `bad_code` 之后、判定第 2 步之前（打错字不扣；`subscribed_before` 现在在第 5 步，会扣）|
| `/me`、`/redeemed` | 30 / 1h / 设备 | `ReadLimiter()` 中间件 |

两个桶在结算 tick 里 `Sweep`。IP 桶沿用 `/v1` 作用域。

## 3. 配置与运维

- 四个依赖：`SUPABASE_URL`、`SUPABASE_DB_URL`、`REVENUECAT_SECRET_KEY`、`REVENUECAT_ENTITLEMENT_ID`。
  缺任一 → 路由仍注册，答 `503 not_configured`；结算不启动（`StartSettler` 打日志退出）。
- `REVENUECAT_ENTITLEMENT_ID` 就是 RevenueCat 后台显示的 identifier 字符串，
  本项目实测为 `YardMate: AI Plant ID & Care Pro`（含空格冒号，写进 secrets.env 不加引号）。
- 迁移：`proxy/enrichment/migrations/013_referral.sql`，可重复跑；**先 staging 后 prod，且先于二进制**。
  `016_referral_antiabuse.sql`（依赖 013 / 014 / 015）同样先于二进制：新二进制调三参数 `referral_claim`、两参数 `referral_me` 等 016 的函数，
  没跑 016 时 `/me`、`/claim` 会 502、结算 ① ② 每行报错。旧二进制用的 013/014 函数签名都保留，回滚二进制不用回滚库。
  删账号路径把「表不存在」当无事可删（42P01 → nil），所以顺序反了也不会把删账号打成 500。
- `referral_settings` 改库即生效，不用发版：

  | 列 | 默认 | 用处 |
  |---|---|---|
  | `hold_days` | 7 | B 兑换后满几天 A 才拿得到 |
  | `issue_margin_days` | 14 | 发码余量：**新发**只发到期日在 `now + 这么多天` 之后的码 |
  | `swap_grace_days` | 3 | 再 claim 时手上那张 issued 码不足这么多天才换新；必须 < `issue_margin_days` |
  | `low_stock_threshold` | 100 | 可发码少于这个数 → 告警；0 = 两种告警都关（临期是投影的低库存） |
  | `expiry_warn_days` | 30 | 最早一批可发码在这么多天内到期、**且它到期那天仍可发的会低于阈值**（只算比它晚到期超过 `issue_margin_days` 的批）→ 告警；0 = 关；必须 > `issue_margin_days` 才可达 |
  | `velocity_limit` | 5 | 速度闸（016）：推荐人窗口内已成功这么多条后，后面的进 held；0 = 闸关 |
  | `velocity_window_hours` | 24 | 速度闸窗口，按 `qualified_at` 数 |

  `014_referral_pool_margin.sql` 加的四列 + 替换 `referral_claim`；**先于二进制**（新二进制每轮读这四列，
  列不存在只是每轮记一条 `pool status` 错误 + `rep.Errors`，结算其它步照跑；旧 `referral_claim` 也能用，只是没余量）。
  组合不可达（`expiry_warn_days <= issue_margin_days` / `swap_grace_days > issue_margin_days`）→ 巡检打一条
  `WARN referral pool settings: …`，改好之前不重复、改好后再错再打。
- 码池告警（`pool.go`）：结算每轮算一次可发数 / 最早到期 / 它死后还剩多少 / 各批剩余；`low_stock`（可发 < 阈值）
  或 `expiring`（最早可发批 `expiry_warn_days` 内到期 **且** 它到期那天仍可发 < 阈值——投影口径，后面有新批顶着就不吵；
  「仍可发」= 比它晚到期超过 `issue_margin_days` 的批，紧挨着到期的那批几乎同时被余量挡掉，不算顶替（Codex P2））
  成立 → 每轮一条 `WARN referral pool: …` 日志 + **每天每种条件组合最多一封邮件**（内存记当天已发的条件组合，
  条件来回翻也只各一封；日期按 UTC，重启后可能重复一封；发信失败不算发过、下轮重试）。
  邮件走 `FEEDBACK_SMTP_*` 同一账号到 `FEEDBACK_EMAIL_TO`（`FeedbackMailer` 由 main 建一次、`/v1/feedback` 和
  本包共用），主题 ASCII（`[YardMate] Referral code pool: low stock (42 usable), expiring 2026-11-10`），
  正文中文：各批剩余、最早到期日及其死后余量、补货命令。时区：去重键、日志、邮件里的日期全是 UTC，
  到期日是 import 时填的 `YYYY-MM-DDT00:00:00Z`，看起来会比本地日历早一天，正常。
  结算通过日志（`referral settle: … usable=N alerted=N …`）在有告警那轮也会打。
- 补货节奏：Apple 一次性码**每批最长 6 个月**、**每季度每 App 上限 15 万个**。建议**每 5 个月补一批**
  （新批到货时旧批还剩 ≥ 1 个月 > 余量 14 天 + 预警 30 天的窗口，两批重叠期间先发旧批）。
  补货：`SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… deploy/supabase/import-codes.sh codes.csv "Referral Friend Month" 2027-04-19`。
- 看现状（只读）：`SUPABASE_DB_URL=… deploy/referral-pool.sh`（各批 free / issued / redeemed / expired、
  可发数、最早到期、告警判定；`--sql` 打印 SQL 给 SQL Editor）。没加 HTTP 接口：`/v1/referral/*` 的鉴权是
  用户 JWT，库存不是任何一个用户的数据；仓库里的运维接口都在 `/internal/*` 配各自的 admin token，
  码池这点数不值得再加一个 secret。
- 冒烟：`TOKEN_A=… TOKEN_B=… deploy/referral-smoke.sh`（默认打 staging）。
- nginx：`location /v1/` 前缀转发已覆盖 `/v1/referral/*`，**不需要改 api vhost**。官网 vhost `deploy/nginx/yardmate.ai` 加了
  邀请链接两段（`/i/*` → `/i/index.html`、AASA `default_type application/json` 不重定向），见 invite-links.md。
- DeviceCheck：`DEVICECHECK_MODE` prod `enforce`、staging `log`；启动日志一行 `referral devicecheck ready: mode=…`，缺东西是 WARN（说缺哪个，不打 key）。
  上线顺序见 `deploy/README.md` §9。
- 冒烟脚本不带 DeviceCheck token，只在 `log` 档能领到码。
- 日志只有计数和错误文本，没有 user id、token、码。

## 4. 测试

防刷（016）：`claim_gates_test.go`（memStore 复刻 016 的判定规则 + fake DeviceCheck）：判定顺序每一步的先后（unknown 先于一切、already_claimed 先于 own_code、
重领不问 RC 也不问 DeviceCheck、own_code 先于 not_new_user、not_new_user 先于 DeviceCheck、DeviceCheck 先于码池）/ 推荐人设备 own_code（大小写无关、别人的码不拦）/
enforce 四种拒绝都不取码不置位 / 置位失败回滚码仍在、再试成功 / pool_empty 不置位 / 换账号同设备 device_used / log 档六种情况都放行且照查照置 /
没配 DeviceCheck 两档 / 苹果挂起 50 ms 超时两档都不卡 / enforce 只认 `production`（`development` / 缺 / 其他值 → `device_unverified`、不问苹果）、log 按请求的环境查 / 置（含 development + bit0 已置仍放行）/ 16 KB body、12 KB token 上限。
`devicecheck_test.go`（httptest 假苹果）：JWT 用公钥验得过、`kid` / `iss` / `iat`、请求体三字段、update 只发 `bit0`、按环境选 host、
九种回答（含两种「没有 bit」文本、400 = token 无效、401 / 5xx / 怪 200 = 不可用）、客户端超时和 context 超时、坏配置不回显 key、模式解析。
`settle_gates_test.go`：subscription key 跟订阅链走（续费 / 换 product 不变）/ 同一订阅第二条 void、只发一个月 / 删号留存的 key 照样拦 / 作废了的推荐不占 key /
第 6 个 held 不发、一封邮件（内容逐项）、下轮不重发、approve 后照常发 / 正好 5 个全发 / 窗口外不算、闸关、阈值来自设置 / reject 永不发 /
同一推荐人 24 小时一封、过了再发 / 发信失败下轮重试、没配邮件只记一次、读库失败不挡结算 / 邮件认出共用设备。
主包 `referral_wiring_test.go`：DeviceCheck 私钥一行 `\n` / `_PATH` 两种写法、缺任一项或坏模式都回落 enforce 且没有客户端。
016 的 SQL：`pgstore_antiabuse_pg_test.go`（同一个 `REFERRAL_PG_TEST_URL` 门控）：016 可重跑 + held 合法 / 判定顺序 + 设备 + `claim_install_id` + `fresh` /
hook 失败回滚 / 作废推荐不换码 / 同一订阅作废 + 钱一起作废 + 删号留存 / 速度闸 + /me 含 held + 邮件摘要 + approve / reject（含 rewarded）+ 退款作废不动钱 / 窗口与闸关。
**本 PR 写的时候本机没有 Postgres，PG 测试一条都没实跑过**（`go test` 里是 SKIP）；上线前先在 scratch 库跑一遍，再 staging 跑 016。

原有：

`go test ./referral/`：码归一化；三个接口每个错误码分支（缺头 / 503 / 401 / 400 系 / 429 / 502 / 503 pool_empty）；
RevenueCat 对象解码与五个判断（订阅史 / 填码后开始 / 退款 / 生效中 / 赠送到期）；Client 的路径、鉴权头、`end_time_ms`；
结算：直接发 / 攒着 → 付费结束补发 / 多个 saved 一次发 / 第 11 个照样 +30d / 退款不算 /
取消不退款照算 / 填码前的订阅不算 / hold_days 读设置 / 账本写失败重发 / RC 挂了跳过；
码池巡检（`pool_test.go`）：余量口径（恰在 cutoff 上不算）/ 最早到期取可发批次里最早的（同日期的批一起算死后余量）/
投影口径（后面有 ≥ 阈值顶着不报、不够才报、恰好等于阈值不报、下一批晚到期不超过余量不算顶替）/ 阈值与 0 = 关 / 不可达组合的 `SettingsProblem` /
低库存每天一封 / 临期一封 / 补货后旧批滑进窗口 25 天静默 / 同日条件变了再发一封、翻回去不再发 / 健康静默 /
发信失败计错下轮重试 / 库读失败不挡结算 / 没配邮件只打日志 / 设置问题只打一次、修好后清 / 邮件正文含各批、到期日、补货命令。
SQL 函数（013 + 014）：`pgstore_pg_test.go` 要一个空库（`REFERRAL_PG_TEST_URL=postgres://… go test ./referral/ -run PG`，
在临时 schema 里跑完就删，没设就 skip；每批码同一个时间戳插入，和 import-codes.sh 口径一致）：
余量过滤 / 升序 + claim 不划过期码 / 重领：3 天宽限外原样回 → 宽限内换新 + 老码 expired + `reissued_at` 记上、`claimed_at` 不动 → 已兑换永远原样回 /
没替换码时老码还能用就回老码（不动任何行）→ 老码也死了才 pool_empty 且已有 referral 不动 → 补货后换 /
`ListClaimed` 窗口跟最近一次发码、`ClaimedAt` 仍是第一次填码 / 首次 claim 只剩余量不足的码 = pool_empty 且 referral 回滚 / PoolStatus 按批分组 + 四列默认值。
013 的函数此前在 staging 用临时 id 实跑过（own_code / unknown_code / 领码 / 重领同一张 / pool_empty / already_claimed）；
**014 还没在 staging 跑过**，上线前先 staging 跑迁移 + `deploy/referral-smoke.sh`。

## 5. 待确认

- **第 3 道用 `subscription_key` 代替 `original_transaction_id`**（§2.4；2026-10-20 api#126 已定接受）：RevenueCat REST 拿不到原始交易号，
  用 `<store>:<original_purchase_date>` 代替。**已知代价：两个不同的人在同一秒开始订阅，后认定的那条会被误作废为 `duplicate_apple_id`**
  （推荐人少一个月、名下的钱一起作废）。运营从 `referrals.subscription_key` / `void_reason` 能看到；出现了再考虑接 App Store Server API
  （另一种 In-App Purchase 密钥 + Issuer ID，用 `store_transaction_id` 查 `originalTransactionId`）换成真的原始交易号。
- `log` 档里「苹果超时 / 5xx」也只记日志放行（文档「503 不放行」那句没分档；按「log = 只记日志不拦」理解，api#126 已定保留）。
- `enforce` 下 `device_check_env` 缺省也当没带 token（文档原写「缺了当 production」；api#126 定为只认显式 `production`，Release 包本来就一律带）。
- 速度闸的「24 小时内成功超过 5 个」按 `qualified_at` 数，朋友是 7 天前批量兑换的，会在同一轮结算里一起到期——第 6 个起全进 held。

- 推荐人正在付费时的「覆盖」把家人共享也算进去（赠了也用不上）；要不要反过来算「没付费」直接发。
- `ListClaimed` 200 天截止之后那条 referral 永远停在 claimed；要不要定期 void。
- RevenueCat 对一个 customer 多次 `end_time_ms` 赠送是「替换到期日」还是「叠加」—— 文档只说 2h 内算重复；
  我们按「替换为更晚的到期日」理解，并把 RC 现有到期日也算进基准。prod 第一笔赠送后看一眼 customer 页核实。
- 再 claim 换码（老码不足 3 天宽限）时：如果 B 其实已经在苹果那边用旧码兑了、结算还没跑到（≤ 10 分钟）、
  App 也没报 `/redeemed`，旧码在库里被划 expired、新码白发一张（结算 ① 照样按第一次填码对上 RC 订阅，A 的奖励不受影响；
  `MarkRedeemed` 会把新码记成 redeemed，库存账少一张）。窗口极窄（老码只剩 ≤ 3 天 × 结算 10 分钟内），先记着。
