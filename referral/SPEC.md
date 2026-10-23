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
  同一事务里、一把全局设备锁下「查 `bit0` → 取码 → 置 `bit0`」（`devicecheck.go`、`ClaimGate`）。
- 防刷五道里后端的四道（§2.4）：DeviceCheck、推荐人设备、同一订阅只算一次（结算 ①）、速度闸 → `held` + 运营邮件（结算 ②、`review.go`）。
- `POST /v1/referral/redeemed`：App 回报「兑了」→ 只是让结算早点去查，不作凭据。
- 结算定时器（每 10 分钟）：查 RevenueCat 推进状态，给推荐人赠会员（`settle.go`）。
- 当场认定（`RedeemForCharge`，`redeem_now.go`，2026-10-22）：rewards 的 webhook 收到 claimed 推荐的朋友在填码之后的 `RENEWAL`
  （免费月里升级到年付，扣费先于结算 ①）时，经 main 的 `chargeRedeemer` 调过来，替这一条推荐跑一次 ①（§2）。
- 码池巡检（结算每轮一次，`pool.go`）：算「可发」库存（free 且到期日在发码余量之外）、最早到期日、
  各批剩余；低库存 / 临期 → WARN 日志 + 每天最多一封告警邮件。
- 码池自动补货（021，**默认关**，`restock.go` + `appstoreconnect.go`，§3.1）：巡检那一步里先补货再判告警——
  可发不够或最早一批快到期时调 App Store Connect API 生成一批一次性码、取码值灌进 `code_pool`。
- 删账号时删掉这个人的邀请码、推荐记录（两侧）、奖励账本（`DeleteUser`，由 `account_delete.go` 调）。
- 三个接口各自的按设备限流桶（claim 5/24h，me + redeemed 30/h），自己扫。

### 1.2 不负责什么

- 验 JWT 的实现 —— 复用 `account_delete.go` 的 `verifySupabaseToken` + JWKS 缓存，
  经 `Authenticator` 注入（`referral_wiring.go`）；本包不认识 `SUPABASE_URL`。
- 按 IP 限流 —— `/v1` 作用域的 `PerIPMiddleware` 已覆盖。
- 手动补货 —— `deploy/supabase/import-codes.sh`（CSV → `code_pool`，老路子保留、开关开关都能用）；看现状 `deploy/referral-pool.sh`。
- App Store Connect 里建 offer、选密钥 —— 人手做（offer `Referral Friend Month` 已建；密钥复用 App Manager key，见产品文档 ## 待确认）。
- 发邮件本身 —— 复用 `/v1/feedback` 的 `enrichment.FeedbackMailer`（Gmail SMTP，`FEEDBACK_SMTP_*` /
  `FEEDBACK_EMAIL_TO`），经 `Options.Alerter` 注入（`mail_wiring.go`）；本包只认识 `Alerter` 接口。
- 苹果兑换本身、App 端判「从没订阅过」（`Transaction.all`）、App Store Connect 的 offer 配置。
- 收回已发出的 RevenueCat 赠送（删账号也不收）。

### 1.3 输入

| 来源 | 内容 |
|---|---|
| 请求头 | `Authorization: Bearer <Supabase access token>`、`X-Device-Install-Id`（UUID）、`X-App-Version` |
| `claim` body（≤ 16 KB）| `{"code": "ym 7kq2", "subscribed_before": false, "device_token": "<DCDevice base64>", "device_check_env": "production"}`；`device_token` 可缺（模拟器 / 拿不到）。`device_check_env`：**`enforce` 只认 `production`**（大小写、首尾空格不计），`development` / 缺 / 其他值一律当没带 token；例外：`DEVICECHECK_ALLOW_DEVELOPMENT=true`（**只给 staging**）时 `enforce` 也认 `development`、走开发环境地址；`log` 按请求的环境查（`development` → 开发环境地址，其余含缺省 → 生产）|
| `redeemed` body | `{"offer_code": "XK7P2M9Q"}` |
| vault | `REVENUECAT_SECRET_KEY`（v1 secret）、`REVENUECAT_ENTITLEMENT_ID`、`REFERRAL_SETTLE_INTERVAL`（可选，默认 10m）；DeviceCheck：`DEVICECHECK_KEY_ID`、`DEVICECHECK_PRIVATE_KEY`（.p8 压一行、`\n` 转义，同 `APPLE_PRIVATE_KEY`；或 `DEVICECHECK_PRIVATE_KEY_PATH`）、`APPLE_TEAM_ID`、`DEVICECHECK_MODE`（`enforce` / `log`，缺 = `enforce`）、`DEVICECHECK_ALLOW_DEVELOPMENT`（`true` / `false`，缺 = `false`；staging 设 `true`，**prod 不设**；开着时启动打一条 WARN）|
| 库 | migration `013_referral.sql` 的五张表 + 两个函数，`014_referral_pool_margin.sql` 的四列设置 + 改过的 `referral_claim`，`016_referral_antiabuse.sql` 的防刷列 / 两张表 / 九个函数，`017_referral_review_fixes.sql` 的 `last_checked_at` + 替换的 `referral_claim`（三参数）/ `referral_qualify`，`021_referral_auto_restock.sql` 的六列补货设置 + `code_restocks`（经共享 pgx 池 `enrichment.DB.Pool()`）|
| vault（自动补货）| `ASC_KEY_ID`、`ASC_ISSUER_ID`、`ASC_PRIVATE_KEY`（.p8 压一行、`\n` 转义，同 `APPLE_PRIVATE_KEY`）、`ASC_REFERRAL_OFFER_ID`、`ASC_OFFER_CODE_ENVIRONMENT`（`PRODUCTION` / `SANDBOX`）；缺任一 → 开关开着也不补（§3.1）|
| App Store Connect API（自动补货）| `POST /v1/subscriptionOfferCodeOneTimeUseCodes`（生成一批）、`GET /v1/subscriptionOfferCodes/{offer}/oneTimeUseCodes?limit=200`（列批次，对账用）、`GET /v1/subscriptionOfferCodeOneTimeUseCodes/{batch}/values`（`Accept: text/csv`，取码值）；ES256 JWT（`kid` = Key ID、`iss` = Issuer ID、`aud` = `appstoreconnect-v1`、`exp` = `iat` + 15 分钟，标准库签、r‖s 原始 64 字节）；每次调用 60 s 超时 |
| Apple DeviceCheck | `POST {api,api.development}.devicecheck.apple.com/v1/query_two_bits`、`/v1/update_two_bits`（ES256 JWT：`kid` = Key ID、`iss` = Team ID、`iat` = now；每次调用 3 s 超时）|
| 告警邮件 | `FEEDBACK_SMTP_FROM/PASS` + `FEEDBACK_EMAIL_TO`（与意见反馈同一个账号、同一个收件箱）；缺任一 → 只打 WARN 日志 |
| RevenueCat | `GET /v1/subscribers/{id}`（**不存在会创建**，只对验过签的 user id 调）、`POST .../entitlements/{ent}/promotional {"end_time_ms"}` |

### 1.4 输出

`/me` → `{code, invited, months_earned, months_pending, months_saved, can_claim}`，作为朋友有一条推荐还在 `claimed` 时多一个
`pending_offer: {offer_code, friend_code}`（022，换机 / 重装后找回待兑换码；见 §2.2）；
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
| 503 | `device_check_unavailable` | 苹果超时（3 s）/ 非 200、400 的回答 / 没配 DeviceCheck，或置 `bit0` 失败且回读也不是已置（这次领码整体回滚，码仍 free）（只 `enforce`）；3 s 内等不到设备锁（两档都是，码没动）|
| 503 | `not_configured` | 四个依赖缺任一个 |

### 1.5 外部依赖

`github.com/jackc/pgx/v5`（共享池）、`net/http`（RevenueCat、Apple DeviceCheck、App Store Connect）、`github.com/golang-jwt/jwt/v5`（DeviceCheck 的 ES256 JWT）、本仓库 `ratelimit`。
App Store Connect 的 JWT 只用标准库（`crypto/ecdsa` + `crypto/x509.ParsePKCS8PrivateKey`），不引第三方。
不引 `chi`（路由在 `server.go` 挂）。

## 2. 一次推荐怎么走完

```
A /me ──造码 YM-XXXX──▶ A 分享
B /claim ──判定顺序 2–4（referral_claim_check）→ 5 RC 查 B 无订阅史 → 6 本地能判的（没配 / 没 token）──▶
        事务 { 全局设备锁（pg_advisory_xact_lock，lock_timeout 3 s）→ 2–4 再判（同账号刚提交 → 重领，不问苹果）
               → 6 query_two_bits(bit0)
               → referral_claim(): referrals(A→B, claimed, claim_install_id) + code_pool 一张 issued
               → update_two_bits(bit0)；报错先回读，已置当成功，否则回滚 } commit
                                （只发 expires_at > now + issue_margin_days 的，先到期的先发）
B 在苹果兑换 ──▶ B /redeemed（记 redeem_reported_at（每次报都刷新），并 nudge 结算）
结算 ①：每轮 200 条按轮转顺序取（上次查之后又报过 /redeemed 的 → 没查过的 → 最久没查的），查完一条记 last_checked_at；
        RC 上 B 有 store 订阅且 original_purchase_date > claimed_at → redeemed（redeemed_at = 那个时间，记 subscription_key）；
        同一 subscription_key 已被另一条没作废的推荐 / 删号留存表占用 → void（duplicate_apple_id，名下的钱一起作废）
当场 ①：webhook 的 RedeemForCharge(推荐, 扣费的 product_id)：RC 上这个产品那条记录的 original_purchase_date ≤ claimed_at（或不是本人购买）→ 不算；
        > claimed_at → 照 ① 走 StartedAfter + referral_mark_redeemed（含第 3 道）；RC 还没这条 / 查失败 → ErrChargeNotReady（webhook 回 503）
结算 ②：redeemed_at + hold_days ≤ now，且这条订阅链没有真付过钱的记录被退款 → qualified；退款 / 订阅没了 → void；
        推荐人在这一条理论成功时间（redeemed_at + hold_days）之前 velocity_window_hours 内已成功（含 held）≥ velocity_limit 条
        → held（等运营），新 held 给运营发信
结算 ③：运营 approve 出来的（reviewed_at 有值）先按 ② 再查一次朋友的退款 / 订阅，不过就 void；
        A 的 RC 没有生效中的 store 订阅 → 赠：end = max(now, 账本 promo_until, RC 现有赠送到期) + 30d
        A 有（付费 / 试用 / intro / 朋友的免费月 / 家人共享）→ saved（清空 last_checked_at）
结算 ④：有 saved 的 A 按轮转顺序取（有没查过的 saved → 最久没查的）；不再被 store 订阅覆盖 → 一次赠 n × 30d，
        全部 saved → rewarded；仍被覆盖 → 记 last_checked_at
```

RevenueCat 的 app user id = Supabase user id **大写**（iOS 传 `UUID.uuidString`，RC id 区分大小写），
见 `AppUserID`。

### 2.1 状态机（`referrals.state`）

`claimed → redeemed → qualified → (held →) (rewarded | saved → rewarded)`，任一步可 `void`。
`void_reason`：`refunded` / `no_subscription`（②）、`duplicate_apple_id`（① 第 3 道）、`rejected`（运营 reject）。
`held → qualified` 只能是运营 `approve`；③ 不再过闸，但先按 ② 再查一次朋友的退款 / 订阅（审核期间退了款 → `void`（`refunded`），不发）。
`/me` 的数字：`invited` = 非 void 条数；`months_earned` = rewarded + saved；
`months_pending` = redeemed + qualified + **held**（推荐人看不出审核）；`months_saved` = saved；`can_claim` = 没填过 **且** RC 无订阅史。

### 2.2 幂等 / 并发

- 造码、领码是 plpgsql 函数（`raise exception` 整体回滚；`for update skip locked` 取库存）。
- 同一人同一码再 claim → 拿回同一张；**推荐已不在 `claimed`**（redeemed / qualified / held / saved / rewarded / void）→ `already_claimed`
  （022；017 是原样回老码——不换码不扣库存这点不变，但 App 会停在「领到码」摆一张用不了的码。017 防的是码在到期前兑了、`ExpireCodes`
  先划了 expired、结算 ① 之后才认定时换码再发一张）；码池那张 redeemed 原样回；issued 且 `expires_at > now + swap_grace_days`（默认 3 天）
  也原样回；**报过 `/redeemed`（`redeem_reported_at`）的 issued 码只要没过期也原样回、临期不换**（022：可能已兑掉、结算还没认定）。换码阈值故意**小于**发码余量：等于的话每张快到余量线的码都会被再 claim 换掉、白烧第二张。
  读推荐行 `for update`（017），和结算 ① 的 `referral_mark_redeemed` 串行。
- `/me` 的 `pending_offer`（022 `referral_pending_offer(user, install)`）：库里 `can_claim` 为 false（填过码）才调。推荐还在 `claimed`、
  有码、码池那张不是 redeemed → 拿当初的朋友码走一遍上面的重领（三参数 `referral_claim`，换不换码全按重领规矩），回
  `{offer_code, friend_code}`；拒绝（pool_empty、同一瞬间被兑换 / 作废）→ null。只回调用者自己。Go 侧失败（没跑 022 / 出错）只记日志、
  不回这个字段，`/me` 照常 200。重领不算进 `/claim` 的设备桶、不问 RC、不问 DeviceCheck（重领本来就不问）。
- 要换（老码过期 / 不足宽限 / 状态不对 / 行没了）时**先选替换码再动老码**（老码行 `for update` 锁住）：
  选到 → 老码划 expired、`referrals.offer_code` 换新、`reissued_at = now()`（`ListClaimed` 的 200 天窗口按
  `coalesce(reissued_at, claimed_at)` 算，不然很晚才兑的会掉出去）；`claimed_at` 不动——结算 ① 认「订阅起始晚于第一次填码」，
  B 可能换码前刚用老码兑了、RC 还没反映，刷了它那笔就永远对不上（Codex P2）；选不到且老码在苹果那边还能用（issued 且 `expires_at > now()`）→ 原样回老码；老码也不能用了才
  `pool_empty`（首次 claim 的 raise 会回滚刚插的 referral，再 claim 的不动已有行）。
- 发码余量：`referral_claim` 只在 `state = 'free' and expires_at > now + issue_margin_days` 里按 `expires_at`
  升序取（先用快过期的，但保证 B 有 ≥ 余量天数去兑）。余量期内 / 已过期的 free 码不发也不改状态
  （claim 不再全表划 expired，那是并发争用点），等结算的 `ExpireCodes` 到期划；巡检把余量期内的单独计作「余量不足」。
- `/redeemed` 重复调只刷新 `redeem_reported_at`（推荐还在 claimed 时）；`ListClaimed` 只看最近一次发码（`coalesce(reissued_at, claimed_at)`）200 天内的（码最长 6 个月）。
- **结算批次轮转**（017，`last_checked_at`）：① / ④ 每轮各取 200（`settleBatch`）。原来固定顺序，积压超过 200 条「填了码一直没兑」的 claimed
  （或一直在付费的 saver）之后每轮拿同一批，新来的永远查不到。现在 `ListClaimed` 按「上次查之后又报过 /redeemed → 没查过 → 最久没查」，
  ① 每条 RC 答了就逐条 `MarkClaimedChecked`（时间用这一轮的开始时刻，这一轮里来的报告下一轮仍算「之后」；逐条写，一轮超时也往前转）；
  RC 查失败的不记，留在前面。`ListSavers` 按推荐人 `min(coalesce(last_checked_at, -infinity))`，④ 仍被覆盖的 `MarkSaverChecked`；
  `MarkSaved` 清空 `last_checked_at`（那是 claimed 时 ① 记的）。
- **当场 ① 和结算 ① 同时认定同一条**：`referral_mark_redeemed` 先锁行、不是 claimed 就 `skipped`，只有一方生效；第 3 道只查 `id <> 自己`，
  不会把自己判成重复。`RedeemForCharge` 碰到 skipped 也答「已认定」，由 rewards 重读推荐状态再决定记不记（被作废了就不记）。
- **一条订阅链多条记录**（2026-10-22）：免费月里升级后，RC 的 `subscriptions` 里同一条链有免费月（`premium.month`）和新产品两条，
  `original_purchase_date` 相同。`StartedAfter` 同一开始时间取产品 id 小的（以前按 map 随机顺序）；② 的 `Refunded(key)` 看这条链
  （同一 `SubscriptionKey` 的本人 store 记录 + key 那条本身）里任何一条**付过钱的**记录有 `refunded_at` → 作废。「没付钱」= `period_type` 是 `trial`，
  或记录里有 `price.amount` 且为 0（RC v1 没给 `price` 的 schema，`price` 按原始 JSON 存、读不懂当付过钱；绝不让它解析失败拖垮整条 Subscriber）。
  RevenueCat 文档说苹果升级的部分退款不写 `refunded_at`；免费月那条实际的 `period_type` / `refunded_at` 待沙盒核实。
- 赠送先 RC 后账本；账本写失败会大声记日志，下一轮**再赠一次**（宁多发不少发）。
- `end_time_ms` 与现有赠送到期相差 < 2h 会被 RC 当重复忽略；我们每次至少 +30d。

### 2.3 `/claim` 判定顺序（文档 `/claim 判定顺序`，先命中的先回）

1. 鉴权 401、请求形状 400（`missing_device_id` / `missing_app_version` / `bad_json` / `bad_code`）、设备桶 429
2. `unknown_code`
3. 这个账号填过：同一个码且推荐还在 `claimed` → 回手上那张（快过期换新，014 规则；**不查 RevenueCat、不查 DeviceCheck**，换台设备也拿得回）；
   别的码，或同一个码但推荐已不在 `claimed`（022）→ `already_claimed`
4. `own_code`：码是自己的，或当前 `X-Device-Install-Id` 在码主人 `referral_devices` 里
5. `not_new_user`：`subscribed_before` 或 RevenueCat 有 store 订阅史
6. DeviceCheck（§2.4 第 1 道）：没配 / 没 token 在事务外判；问苹果在事务里、设备锁下
7. 码池取码：没码 `pool_empty`（不置位）；取到 → 同一事务置 `bit0`，失败（且回读不是已置）回滚 → `device_check_unavailable`

2–4 先用只读的 `referral_claim_check` 判（这样 5、6 的外部调用只对「第一次填」发生、`pool_empty` 才排得到 5、6 后面），
新填码的事务拿到设备锁后再 `referral_claim_check` 一遍（同账号另一台设备刚提交 → 当重领，不问苹果），
真领时三参数 `referral_claim` 在事务里原子地再判一遍（晚的那个拿到 `fresh = false`，不再置位）。
和 #123/#124 的区别：`subscribed_before` 从「限流之前」挪到第 5 步（会扣一次当天次数——App 本来就不会让订阅过的人看到填码入口）；
重领不再先问 RevenueCat（以前兑换过的人重领会被 RC 答成 `not_new_user`）。作废了的推荐重领回 `already_claimed`（022；之前只回老码），**不换码**（免得被拒的人天天重领烧库存）。

### 2.4 防刷（文档 `### 防刷` 五道，后端管前四道 + 第 5 道的数据）

**第 1 道 DeviceCheck**（`devicecheck.go`，`handlers.go` `deviceGate` / `deviceRun`，`store.go` `ClaimGate`）

| 情况 | `enforce`（prod）| `log`（staging）|
|---|---|---|
| 没配 DeviceCheck（缺 Key ID / Team ID / .p8，或 .p8 解析不了）| 新填码一律 503 `device_check_unavailable` | 不查不置，记日志放行 |
| 没带 `device_token`（或 > 12 KB / 有控制字符）| 400 `device_unverified` | 记日志放行，不置位 |
| `device_check_env` 不是 `production`（`development` / 缺 / 其他）| 当没带 token：400 `device_unverified`，不问苹果（开发环境的 bit 与生产分开存，认它等于每台设备两条命；Release 包一律带 `production`）。例外：`DEVICECHECK_ALLOW_DEVELOPMENT=true`（只 staging）时 `development` 也认，查 / 置走开发环境地址，照常拒 | 照请求的环境查 / 置（`development` → `api.development.devicecheck.apple.com`），结果只记日志 |
| 苹果答 400（token 无效）| 400 `device_unverified` | 记日志放行，不置位 |
| 苹果超时 / 其他错误 | 503 `device_check_unavailable` | 记日志放行，照样尝试置位 |
| `bit0` 已置 | 400 `device_used` | 记日志放行，照样置位 |
| 取到码后置位报错（超时 / 5xx）| 先回读一次 `bit0`：已置（苹果置上了、只是回答丢了）→ 当成功提交；未置 / 回读也失败 → 回滚领码（码仍 free）→ 503 `device_check_unavailable` | 记日志，领码照常提交，不回读 |
| 设备锁 3 s 内等不到（`lock_timeout`，55P03）| 503 `device_check_unavailable`（码没动）| 同左 |

- 苹果「没有 bit 记录」是 200 + 纯文本（实测 `Failed to find bit state`，文档写 `Bit State Not Found`），都当 `bit0 = false`。
- `update_two_bits` 只发 `bit0: true`，不发 `bit1`（同一 Team 下别的 App 若也用 DeviceCheck，会共用这两个 bit）。
- **查和置是原子的**（`ClaimGate`，评审 P2）：苹果的 bit 不在我们的事务里，同一台设备可以同时以多个账号、各自伪造的 `X-Device-Install-Id` 来填码；
  事务外「先查后置」会让它们都查到 `false`、都拿到码。所以新填码的事务先拿一把**全局**事务级 advisory lock
  （`pg_advisory_xact_lock(hashtextextended('referral_devicecheck', 0))`，填码量很小，一把就够；事务级 → 不怕 Supabase 的事务模式连接池，也不会比事务活得久），
  `set local lock_timeout = '3000ms'`（也管这一事务里的行锁），锁到 commit / rollback：锁内再判 2–4 → 查 → 取码 → 置。重领不拿锁、不问苹果。
  领码事务的 context 预算 = 10 s + 3 s 锁 + 3 × 苹果超时（查、置、回读），苹果调用自己各 3 s。
- 置位在领码事务里、commit 之前：置位报错 → 回读（见上表）；置位成功但 commit 失败（极少）→ 这台设备 `bit0` 已置但没领到码，大声记日志。
  回读也失败时仍回滚：苹果其实可能已置上，这台设备之后会是 `device_used`（剩余风险，只在「置位和回读都失败」时）。
- 每次苹果调用 3 s（`DefaultDeviceCheckTimeout`，HTTP 客户端和 context 双重），一次新填码最多三次（查、置、置位报错时的回读）。

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

**第 4 道 速度闸**（结算 ②，`referral_qualify`，017）：按**理论成功时间** = `redeemed_at + hold_days` 数，不按结算跑到的时刻：
这一条的理论成功时间之前 `velocity_window_hours` 小时内（含那一刻）这个推荐人已成功的条数（qualified / held / saved / rewarded，**含 held**），
≥ `velocity_limit` → 这条 `held`（也记 `qualified_at`，held 自己也算进后面的计数）。所有推荐同一个 `hold_days`，比较时消掉，SQL 里直接比 `redeemed_at`。
即 24 小时内第 6 个起待审核。原来按 `qualified_at` 数：结算停摆后恢复，积压的成功在同一轮一起认定，第 6 个起全进 held（误拦，评审 P3）。
只往前看：比这一条晚兑换的理论上在它之后才成功（`ListDue` 按 `redeemed_at` 升序给，正常时它们还没判）。
`velocity_limit = 0` = 闸关。锁推荐人 `referral_users` 行（`for no key update`）让同一推荐人的判定排队。
- 运营邮件（`review.go`）：每轮 ② 之后查「有没发过信的 held、且这个推荐人 24 小时内没收过信」的推荐人（`held_notified_at` 记在库里，重启不重发、发失败下轮重试）；
  每人一封：推荐人 ID、规则、待审核条数（本封新增几条）、这批朋友填码设备数了几台不同（`claim_install_id`，016 之前的没记）、
  待审核 + 算进某条 held 窗口的成功（同上口径）的列表（按理论成功时间排，列出理论成功 / 结算认定两个时间）、approve / reject 命令。主题 ASCII（`[YardMate] Referral review: N held for one referrer`），正文中文。没配邮件 → 只打 WARN 并照样标记。
- 运营 `deploy/referral-review.sh list | approve <id> | reject <id>`（PostgREST + service role，rpc `referral_approve` / `referral_reject`）：
  `approve` = held → qualified + `reviewed_at`，下一轮 ③ 先按 ② 再查一次朋友的退款 / 订阅（审核期间退了 → `void`），过了照常发 / 攒（推荐人的 `months_earned` 变了，App 照常发到账通知）；
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
  `017_referral_review_fixes.sql`（依赖 013–016，可重复跑）**先于二进制、先 staging 后 prod**：新二进制读写 `referrals.last_checked_at`、
  `reviewed_at`；没跑 017 时结算 ① ③ ④ 的列表每轮报错（`/me`、`/claim` 照常）。017 只同名同签名替换 `referral_claim(text,text,text)` 和
  `referral_qualify(bigint)`，016 那版二进制照常调，回滚二进制不用回滚库。⚠️ 017 之后若重跑 016，会把这两个函数换回 016 版——重跑 016 后再跑一遍 017。
  `022_referral_pending_offer.sql`（依赖 013–017，可重复跑）**先于二进制、先 staging 后 prod**：新增 `referral_pending_offer(text,text)`，
  同名同签名替换 `referral_claim_check(text,text,text)` 和 `referral_claim(text,text,text)`（已不在 claimed 的重领 → already_claimed；
  报过 /redeemed 的没过期不换）。没跑 022 时新二进制的 `/me` 每次记一行日志、不回 `pending_offer`，其余照常；旧二进制照常调，回滚二进制不用回滚库。
  ⚠️ 022 之后若重跑 016 或 017，会把这两个函数换回旧版——重跑后再跑一遍 022。
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
  | `velocity_window_hours` | 24 | 速度闸窗口，按理论成功时间（`redeemed_at + hold_days`）数（017）|

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
  本包共用），主题 ASCII（`[YardMate] Referral code pool: low stock (42 usable), expiring 2026-11-10`；配了 `ASC_OFFER_CODE_ENVIRONMENT` 时
  在 `[YardMate]` 后加 `[SANDBOX]` / `[PRODUCTION]`，见 §3.1），
  正文中文：各批剩余、最早到期日及其死后余量、补货命令。时区：去重键、日志、邮件里的日期全是 UTC，
  到期日是 import 时填的 `YYYY-MM-DDT00:00:00Z`，看起来会比本地日历早一天，正常。
  结算通过日志（`referral settle: … usable=N alerted=N …`）在有告警那轮也会打。
- 补货节奏：Apple 一次性码**每批 500–25,000 张、最长 6 个月**（到期日太平洋时间 0 点失效）、**每 App 每季度上限 100 万张、所有订阅共用**
  （2026-10-22 核对 App Store Connect Help；此前写的「15 万」是旧数）。手动补货时建议**每 5 个月补一批**
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
  `DEVICECHECK_ALLOW_DEVELOPMENT`：staging 设 `true`（切 `enforce` 测第 1 道时 DEBUG 包的 `development` token 也认、走开发环境地址），
  **prod 不设**（开发环境的 bit 与生产分开存，prod 认它等于每台设备两条命）；开着时启动打 `WARN: DEVICECHECK_ALLOW_DEVELOPMENT=true …`。
  上线顺序见 `deploy/README.md` §9。
- 冒烟脚本不带 DeviceCheck token，只在 `log` 档能领到码。
- 日志只有计数和错误文本，没有 user id、token、码。RevenueCat 客户端的错误只带操作名（`get subscriber` / `grant promotional`）+ 状态码 / 底层错误：
  net/http 的 `*url.Error` 会打出整条 URL（`/subscribers/<APP_USER_ID>`），拆掉只留原因（仍 `%w`，超时照样认得出）。

### 3.1 码池自动补货（021，`restock.go` / `appstoreconnect.go`，默认关）

产品规则见产品文档 `### 码池自动补货`；这里是后端怎么落。

- **在哪跑**：`watchPool`（结算每轮看库存那一步）里，读完 `PoolStatus` 先 `restockStep`，有码灌进去就重读一次 `PoolStatus`，再判低库存 / 临期告警。
  `restockStep` 返回 `busy`（还有没收尾、没超时的补货）时这两种告警**只记 WARN 日志、不发邮件**。
- **先后顺序**（每轮）：读 `RestockStatus`（021 没跑 → 当作关、只记一行日志，不计错）→ 开关关 → 什么都不做（不调苹果、不判超时、不发邮件；
  库里若还有没收尾的 `creating` / `created`，每个 UTC 日记一条 WARN 说它停在哪、要么开回开关让它收尾、要么手动导入）→
  设置组合检查（只 WARN，同一组问题只打一次）→ 配置缺 → 「没配置」告警 → 码环境不一致 / 库里为空 → 「码环境」告警 →
  有没收尾的（`creating` / `created`）→ 收尾它，**这一轮不发起新的** → 判两条触发 → `restock_daily_max = 0` = **暂停**（不发起、不发「到上限」告警、
  一天记一行日志；没收尾的照样收尾，低库存告警照旧）→ 每天上限 → 1 小时间隔 → 发起。
  没配置、码环境不一致不写库，所以不算发起；也不去碰没收尾的那一行（配置修好后再收尾 / 判超时）。
- **两条触发**（同 `TakeStock` 口径）：`Usable < restock_below`；或 `Earliest` 在 `restock_lead_days` 天内（含当天）且 `AfterEarliest < restock_below`。
- **发起**：`BeginRestock` 一条条件 `insert`（同一语句里再判一遍每天上限和 1 小时间隔），两个进程同时插 → partial unique index
  （`code_restocks_one_open_uidx`，`((true)) where state in ('creating','created')`）让第二个拿 23505 → 当「别的进程先发起了」。
  然后 `POST`：到期日 = 当天（UTC）+ 179 天，`code_restocks.expires_at` / `code_pool.expires_at` 记成那天 00:00 UTC；张数 = `restock_batch_size`（PRODUCTION 夹到 500–25,000）。
- **要码的结果分三种**（`ASCError.Uncertain`）：
  | 情况 | 判定 | 处理 |
  |---|---|---|
  | 201 带 `data.id` | 成功 | 马上 `MarkRestockCreated` 记批次 ID，同一轮接着取码值、灌池 |
  | 401 / 403 / 其他 4xx / 3xx，或请求根本没写出去（DNS、连接被拒、TLS 握手失败——用 `httptrace.WroteRequest` 判） | 明确失败 | `failed` + 告警（不带批次 ID，按 `http_<状态码>` / `unreachable` 一天一封）|
  | 请求写出去之后超时 / 断连、5xx、2xx 但没有批次 ID、进程在 `creating` 途中重启 | 结果不确定 | 保持 `creating`，之后每轮对账 |
  记批次 ID 失败（库写不进）也保持 `creating`，下一轮对账自然找回这批。
- **对账**：列这个 offer 的一次性码批次（`limit=200`，跟 `links.next`，只跟同 host，最多 10 页；**每一页单独 60 s**，客户端里按请求限时；整个列表另有 3 页份的总上限（`restockListBudgetPages × CallTimeout`，默认 3 分钟），慢的长列表不会吃掉同一轮后面的 RevenueCat 结算），候选 = `createdDate ≥ started_at − 5 分钟`、
  `numberOfCodes` / `expirationDate` / `environment` 都相同；再去掉 `code_restocks` 里已有行认领的（`RestockBatchesTaken`）；
  剩下按 `createdDate` 取最早的一批 → `created`；`asc_batch_id` 的唯一约束兜底。发起满 30 分钟还没对上 → `failed` + 告警（一天一封）。
- **取码值、灌池**：`GET …/values`（`Accept: text/csv`）→ 按 `import-codes.sh` 的口径逐行挑 `^[A-Z0-9]{6,20}$`、去重；
  每次取都记一行日志：CSV 非空行数、认出的码数、要的张数（不记码值）。**拿到的码少于要的张数当「还没好」**，下一轮再取（不重新要码）；
  但**非空行数 ≥ 要的张数、认出的码却不够** = 格式认不出，等也没用 → 马上发一封「码值格式认不出」告警（按批次只一封），之后照样每轮再试；
  够了 → `ImportRestock` 一个事务：`insert … on conflict (code) do nothing`（`offer_ref_name` = `Referral Friend Month`）+ 行转 `imported`、
  `imported_count` = 真正新进池的张数。发起满 6 小时那一轮**先最后再试一次**取码灌池，还不行才 `failed` + 告警（带批次 ID，按批次各一封）。
  码值不进日志、不进邮件。
- **告警 / 通知**：都走 `Alerter`（和码池告警同一个邮箱），去重沿用巡检的「当天已发 key」（内存，UTC 换日清，重启可能重发一封；发信失败不算发过）。
  key：`restock:not_configured` / `restock:environment` / `restock:daily_max` / `restock:create_failed:<kind>` / `restock:reconcile_timeout`（一天一封），
  `restock:values_unrecognised:<批次>` / `restock:import_timeout:<批次>` / `restock:imported:<批次>`（按批次：进程活着期间每批只一封，跨 UTC 日也不重发，
  内存记、重启可能重发）。主题 `[YardMate] [SANDBOX] Referral code restock: …`（环境取 secrets 的，
  没有再取库里的，都没有写 `UNSET`）；正文中文，附手动导入命令。成功通知：张数（新进池）、环境、到期日、苹果批次 ID。
  每天上限的告警只在可发仍 < `restock_below` 时发（只是提前补那条成立时不发）。
  **码池低库存 / 临期告警**的主题在配了 `ASC_OFFER_CODE_ENVIRONMENT` 时也带环境（`[YardMate] [SANDBOX] Referral code pool: …`，
  staging 和 prod 发到同一个邮箱能分清），没配（或不是两个合法值之一）时和原来一样不带。
- **设置组合**（`planRestock`，只 WARN、照常补）：`restock_batch_size ≤ restock_below`；`restock_below ≤ low_stock_threshold`（阈值 > 0 时）；
  `restock_lead_days ≤ expiry_warn_days`（> 0 时）；PRODUCTION 批量不在 500–25,000（夹住）；`restock_lead_days + issue_margin_days ≥ 179`
  （提前天数夹到 `179 − issue_margin_days − 1`，如 170 → 164）。沙盒码不受 500 下限（staging 用 10 张）。
- **配置**（`referral_wiring.go` `buildRestockConfig`）：五项 `ASC_*`，环境大小写不计；缺任一或 .p8 解析不了 → `RestockConfig.Missing` 记名字（不记值），
  启动打一行 INFO（开关默认关，不是 WARN）。`appleP8Bytes` 读 `ASC_PRIVATE_KEY`，不支持 `_PATH`（产品文档只列了这五项）。
- **迁移**：`021_referral_auto_restock.sql`（依赖 013 / 014，可重复跑）**先 staging 后 prod、先于二进制**。没跑 021 时新二进制把自动补货当作关
  （`ErrRestockNotMigrated`，只记一行日志），巡检和结算其它步照跑；旧二进制不认识这些列，回滚二进制不用回滚库。
- 看现状：`deploy/referral-pool.sh` 多打一段「自动补货」：补货设置 + 最近 10 次补货（先查 `information_schema` 判断 021 的列 / 表在不在，
  不在只提示一句；其他错误照常报、脚本失败）。上线步骤见 `deploy/README.md` §10。

### 3.2 朋友免费月分组实验（023，`offer_variant.go`，默认 0% = 全部月付）

产品规则在 yardmate-swiftui `docs/releases/v1/main-navigation/more/invite-friends/invite-friends.md` `### 兑换码分组实验（月付组 / 年付组）`。后端这一侧：

- **分组**：`referral_claim` 新建推荐时调 `referral_offer_variant(user)`：md5(用户 + `:offer_ab`) 前 8 个十六进制位当无符号数对 100 取余，小于 `referral_settings.offer_year_pct` → `year`。写进 `referrals.offer_variant`，领码时的占比写进 `offer_year_pct_at_claim`。重领、换码、`pending_offer` 一律沿用原组；老推荐（空）按 `month`。
- **按组发码**：年付组只取 `offer_ref_name = offer_ref_year` 的码，月付组取其余所有码（别名批次、测试批次都算月付）。一组没码就 `pool_empty`，不借另一组。
- **老 App 包**：请求没带 `offer_variant_aware: true` → handler 给 ctx 打 `WithForceMonth`，`PGStore.Claim` 在事务里 `set_config('referral.force_month','on', true)`，新建推荐一律月付、`offer_year_pct_at_claim` 记空。事务级设置，不会漏给连接池里的下一个请求（`TestPGForceMonthForUnawareApps`）。
- **回答**：`/claim` 回 `{offer_code, offer_variant}`；组别取自 `referral_claim` 同一事务的回答（`PGStore.ClaimWithVariant`）；只有不支持它的 store（内存测试替身）才在领码后另读一次（`Store.OfferVariant`，读失败答 `month`）。`/me` 的 `pending_offer` 带 `offer_variant`（023 换了 `referral_pending_offer`）。
- **库存和告警**：`watchPool` 把码池拆成两组（`PoolStatus.SplitByVariant`）。月付组照旧（告警 key、标题不变）；年付组在占比 > 0 或还有没兑的年付组推荐时才看，告警 key 前缀 `year:`、标题带 `yearly group`。不认识的码名（既不是月付名也不是年付名）每轮记一行 WARN。
- **自动补货只补月付组**：`restockStep` 只拿到月付组的库存。两组各自补（`code_restocks` 加 `offer_ref_name`、唯一约束和每日上限按组）是下一步，见 §5。
- **运营**：`deploy/referral-ab.sh`（PostgREST 调 023 的函数）：`show`（`referral_ab_overview` + `referral_ab_stats`）、`set`（`referral_set_offer_year_pct`，第一次从 0 调上去记 `offer_ab_started_at`）、`switch-unredeemed`（`referral_switch_unredeemed`：新版 App 领的（`offer_year_pct_at_claim` 不为空）、没兑、没报过 `/redeemed`、不在目标组的推荐换成目标组的码；老码划 `expired`；一个事务，码不够整体回滚。老推荐 / 老 App 包的不换：老包会把年付码显示成「每月」）。
- **统计**（`referral_ab_stats`）：只算 `offer_year_pct_at_claim` 在 1–99、没被换过组的推荐；剔除 `duplicate_apple_id` / `rejected` 作废的；每组按 30 / 90 / 180 / 365 / 425 天给「每个领码的实收」（主指标）和「每个兑换的实收」。
- **迁移顺序**：023 依赖 022（api#140）。023 整体替换 022 的 `referral_claim` 和 `referral_pending_offer`；重跑 016 / 017 / 022 后要再跑 023。

## 4. 测试

换机找回待兑换码（022）：`handlers_test.go` `TestMePendingOfferOnlyForClaimedAndFailsSoft`（没填过码不查、填过码回码 + 朋友码、没有 / 查失败不回字段且 200）；
`pgstore_pending_pg_test.go`（本机 postgresql@17 实跑）：claimed 回原码 + 朋友码、推荐人自己问不到、宽限内换新老码 expired、码池 redeemed / 推荐 redeemed / void 不回、
报过 /redeemed 临期 /me 与重领都不换、过期才换、码池空临期回老码过期回空、022 可重跑；`TestPGReclaimAfterRedeemingNeverBurnsAnotherCode` /
`TestPGVoidReferralNeverSwaps` 改为 already_claimed。

自动补货（021）：`restock_test.go`（memStore 复刻 021 的两条唯一约束 + `fakeASC`）：开关关不调苹果、留着的 `created` 不动、低库存告警照旧 /
可发 < 300 发起一批（1,000 张、到期 当天 + 179 天、offer / 环境对）→ 灌池 → `imported` → 成功通知（张数、环境、到期日、批次 ID，不含码值）→ 同轮低库存告警不发 /
staging `SANDBOX`、prod `PRODUCTION` / 600 张全在一批且 40 天内到期 → 提前补、之后 45 天临期告警不发、不再补 / 提前补边界（45 天补、46 天不补、后面 300 张不补、299 补、只晚一个余量的不算顶替）/
`restock_lead_days = 170` → 按 164 算、WARN 只打一次、新批算得进「到期后可发」不会一直补 / 结果不确定时低库存告警只记日志、每轮对账、30 分钟没找到 → `failed` + 告警 + 低库存邮件这才发、只要过一次码 /
5xx 但苹果已生成 → 对上那批（创建时间早 3 分钟在 5 分钟余量内；早 6 分钟的、张数 / 环境 / 到期日不同的都不算）/ 同一天第 2 次对账不认领第 1 次的批次、30 分钟后 `failed`；有另一批没被认领的就认它 /
要码途中重启 → 下一轮对账 / 苹果已给批次但记库失败 → 下一轮对账找回 / `created` 之后重启 → 接着取码灌池、不再要码；取码失败下轮再取 / 码值少于张数先不灌 /
6 小时没灌进 → `failed` + 告警带批次 ID、两批各一封 / 1 小时间隔（59 分钟不发、61 分钟发）+ 每天 2 次 + 第 3 次告警一封 + 第二天恢复 / 到上限但可发够了不发告警 /
403 立刻 `failed`、原因带苹果的错误码、同种一天一封、1 小时后再试、连接被拒算 `unreachable` 另一封、第二天再发 / 没配置：不调苹果、不写库（不算发起）、一天一封、全空时列出缺的名字 /
码环境库里为空或不一致：拒绝、一天一封、低库存告警照发 / PRODUCTION 批量 100 → 500 + WARN，SANDBOX 10 张不夹 / `planRestock` 五种组合 / 重复码忽略（998 + 邮件写忽略 2 张）/
4 个服务并发结算 25 轮每轮只发起一次 / `RestockStatus` 出错计错不挡告警、`ErrRestockNotMigrated` 不计错 / 到期日按 UTC 日算 /
复审修补：满 6 小时那一轮最后再试一次成功 → `imported`、不发超时告警 / 码值格式认不出：日志有行数和码数、按批次一封（跨 UTC 日不重发）、6 小时后照样 `failed`、码值不进日志 /
开关关但有没收尾的行：WARN 每天一条、不调苹果不发邮件 / `restock_daily_max = 0` 暂停：不发起、不发上限告警、低库存告警照发、没收尾的照样收尾 /
对账列批次的总上限在 (2, 3] × CallTimeout / 码池告警主题按 `ASC_OFFER_CODE_ENVIRONMENT` 带环境（大小写不计；没配或写错不带）。
`appstoreconnect_test.go`（httptest 假苹果）：JWT header / claims / r‖s 签名用公钥验得过 / 生成请求的路径、头、body 逐字段 / 401、403、409、422、429、302 明确失败（带苹果错误码）、500、503、201 没 id 不确定 /
请求写出去后客户端超时、context 超时都是不确定 / 连接被拒是 `unreachable` / 列批次解析字段、跟同 host 的 next、不跟外域 / 三页各 70 ms、单页限时 150 ms 照样列全 / CSV（BOM、CRLF、引号、垃圾行、重复、非空行数）/ 错误里没有 token / 坏 .p8（非 PEM、乱码、RSA、P-384、缺 ID）不回显 key /
真客户端 + 假苹果端到端（正常、502 后对账）。主包 `referral_wiring_test.go`：五项齐 / 各缺一项 / 环境写错 / .p8 坏 / 全空。
021 的 SQL：`pgstore_restock_pg_test.go`（同一个 `REFERRAL_PG_TEST_URL` 门控）：021 跑两遍 + 默认值 + 各 check / 发起 → 只允许一条没收尾 → 记批次 → 已认领 → 灌池（已有码忽略、计数 2）→ 已 imported 不再灌 /
间隔和每天上限在插入时再判 / 批次 ID 不能被第二行认领 / 8 路并发发起只成一个。**本机没有 Postgres，这三条 PG 测试只写了没跑过**（SKIP），上线前先在 scratch 库跑一遍再 staging 跑 021。

评审修补（017，api#126 之后）：`claim_review_test.go`：8 个 goroutine 同一设备 token、各自账号 + 伪造 install id 并发 /claim 只成一个
（假实现用 `deviceMu` 模拟全局锁，去掉锁这条即失败）/ 同账号连点两下只问一次苹果、同一张码 / 置位报错但苹果已置 → 保留 / 置位报错回读未置 → 回滚、可重试 /
置位和回读都失败 → 回滚 / log 档置位失败不回读 / `DEVICECHECK_ALLOW_DEVELOPMENT`：development 走开发地址查置、照样 `device_used`、production 照常、其他 env 仍 `device_unverified`、缺省关。
`settle_review_test.go`：claimed 超过一批（250 条）第二轮拿到没查过的 + 新来的被认定 / 查过之后又报 /redeemed 的插到没查过的前面 / RC 查失败不记 /
saver 超过一批第二轮轮到新 saver、`MarkSaved` 清空 / held 期间退款或订阅没了 → approve 后 void 不发、没退款照常发 /
相隔 26 小时兑换的积压同一轮认定不 held / 同一小时兑换、晚 3 天认定照样 held、邮件按理论时间 / 窗口外的老成功不进邮件。
免费月里换套餐（2026-10-22，`redeem_now_test.go`）：升级后当场认定 → redeemed_at = 免费月开始、key = store + 开始时间、重发仍答已认定、
结算 ① 不再认定、第 6 天不发、第 7 天发一个月且只发一次 / 订阅早于填码（恢复购买）不算、仍 claimed / RC 没这个产品、RC 出错、没配 RC 都是 not ready /
当场第 3 道作废 / 家人共享不算 / 降级（到期才换）照常认定、第 7 天发、换产品之后不再动 / 判退款看整条链（年付退款作废、两条都退作废、只有免费月带退款标记不作废、都没退不作废，各跑 40 次防 map 顺序）/
`StartedAfter` 同开始时间稳定取同一条 / `free()`（trial、价格 0 → 没付钱；有价、没价、null、奇怪形状 → 付过钱，且解析不失败）。
主包 `rewards_wiring_test.go`：`chargeRedeemer` 在没配 RC 时把 `ErrChargeNotReady` 原样传出。
`revenuecat_test.go`：连不上 / 超时的错误里没有 app user id 和路径、操作名在、`errors.Is(DeadlineExceeded)`；主包 `account_delete_logs_test.go` 删账号同理；
`referral_wiring_test.go`：`DEVICECHECK_ALLOW_DEVELOPMENT` 五种写法。
017 的 SQL（`pgstore_review_pg_test.go`，同一个 `REFERRAL_PG_TEST_URL` 门控）：017 可重跑 + 各代函数签名都在 / `ListClaimed` 轮转 + 报告插队 + 非 claimed 不记 /
`ListSavers` 轮转 + `MarkSaved` 清空 / 6 路并发同设备只成一个 / 设备锁被占 3 s 后 `device_check_unavailable` / 只有 approve 过的 `Reviewed` /
已兑换（码池那行已 expired）五个状态重领都回原码、不烧新码 / 速度闸按理论成功时间（积压不 held、晚认定的爆发照样 held、摘要只列爆发 + `DueAt`）；
`pgstore_antiabuse_pg_test.go` 里 gate 的 Check 拒绝回滚、锁内重领不问苹果。**这些 PG 测试本机没有 Postgres，都只写了没跑过**（`go test` 里 SKIP）。

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

- 分组实验的自动补货按组补（`code_restocks` 加 `offer_ref_name`，`code_restocks_one_open_uidx` 改按组，间隔 / 每日上限按组，年付组 offer ID `ASC_REFERRAL_OFFER_ID_YEAR`）：第一版年付码手动导入。

自动补货（021）里产品文档没写死、本 PR 自己定的（拿不准，等拍板）：

- 苹果给的码值**少于要的张数**当「还没生成好」，下一轮再取，直到 6 小时超时（不先灌一部分）。
- 对账时**有好几批都对得上**（没被认领的），取 `createdDate` 最早的那批。
- 「没配置」「码环境不一致」告警在开关开着时**每天都发**，不管这时码池需不需要补；这期间不碰没收尾的那一行（不判它的 30 分钟 / 6 小时超时），低库存告警也不压。
- 收尾一条没收尾的补货（灌完或判失败）的那一轮**不接着发起新的**，下一轮（≤ 10 分钟）再判。
- 6 小时按 `started_at`（发起要码的时间）算，不是按拿到批次 ID 的时间（021 没有单独的 created 时间列）。
- 成功通知里的「N 张」= 真正新进码池的张数（另写苹果给了几个、几个库里已有被忽略）；成功通知发信失败不重发。
- 码池低库存 / 临期告警正文多了一行「自动补货见 deploy/README.md §10」（主题带环境是复审定的，见 §3.1）。
- 列批次最多跟 10 页（2,000 批）；超出的批次对账看不到（按每天最多 2 批，十年内到不了）。
- 结果判定：返回 2xx 但读不出批次 ID 算「不确定」；3xx（不跟随重定向）算明确失败；不是 `ASCError` 的错误一律当「不确定」（宁可多对账 30 分钟，不重复生成）。
- 开关关时没收尾的那一行**原样留着**（不判超时、不调苹果），只每天记 WARN；开回开关后再收尾，那时多半已过 30 分钟 / 6 小时，会直接走超时（6 小时那条先最后试一次）。


- **第 3 道用 `subscription_key` 代替 `original_transaction_id`**（§2.4；2026-10-20 api#126 已定接受）：RevenueCat REST 拿不到原始交易号，
  用 `<store>:<original_purchase_date>` 代替。**已知代价：两个不同的人在同一秒开始订阅，后认定的那条会被误作废为 `duplicate_apple_id`**
  （推荐人少一个月、名下的钱一起作废）。运营从 `referrals.subscription_key` / `void_reason` 能看到；出现了再考虑接 App Store Server API
  （另一种 In-App Purchase 密钥 + Issuer ID，用 `store_transaction_id` 查 `originalTransactionId`）换成真的原始交易号。
- `log` 档里「苹果超时 / 5xx」也只记日志放行（文档「503 不放行」那句没分档；按「log = 只记日志不拦」理解，api#126 已定保留）。
- `enforce` 下 `device_check_env` 缺省也当没带 token（文档原写「缺了当 production」；api#126 定为只认显式 `production`，Release 包本来就一律带）。
- 速度闸按理论成功时间、**只往前看**（017）：正常结算按 `redeemed_at` 升序判，口径就是「第 6 个」。若一批里较早兑换的那条这一轮 RC 查失败、
  下一轮才判，它的窗口里只有比它早的——比它晚、已经判过的 5 条不算，这一批可能 6 条都放行。要关这个口子就改成「前后 24 小时」双向数
  （代价是极少数排列下多拦一条进 held）；本 PR 照评审原话「过去 24 小时」做。
- 旧二进制（016 之前）用的两参数 `referral_claim` 没改：回滚到那么老的二进制时，「已兑换后重领」仍会换码（017 只换了三参数版）。
- 第 1 道「置位和回读都失败」时仍回滚（苹果可能其实已置上 → 这台设备之后 `device_used`）；要再保守就得在库里记一笔「待核实」下次 claim 前再查，本 PR 不做。

- 推荐人正在付费时的「覆盖」把家人共享也算进去（赠了也用不上）；要不要反过来算「没付费」直接发。
- `ListClaimed` 200 天截止之后那条 referral 永远停在 claimed；要不要定期 void。
- RevenueCat 对一个 customer 多次 `end_time_ms` 赠送是「替换到期日」还是「叠加」—— 文档只说 2h 内算重复；
  我们按「替换为更晚的到期日」理解，并把 RC 现有到期日也算进基准。prod 第一笔赠送后看一眼 customer 页核实。
- 再 claim 换码（老码不足 3 天宽限）时：如果 B 其实已经在苹果那边用旧码兑了、结算还没跑到（≤ 10 分钟）、
  App 也没报 `/redeemed`，旧码在库里被划 expired、新码白发一张（结算 ① 照样按第一次填码对上 RC 订阅，A 的奖励不受影响；
  `MarkRedeemed` 会把新码记成 redeemed，库存账少一张）。窗口极窄（老码只剩 ≤ 3 天 × 结算 10 分钟内），先记着。
