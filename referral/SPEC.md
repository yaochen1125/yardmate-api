# `referral` package — 推荐好友（/v1/referral/*）

> 产品文档（源真）：yardmate-swiftui `docs/releases/v1/main-navigation/more/invite-friends/invite-friends.md`。
> 接口路径、字段、错误码按那份文档原样；本文只写后端怎么实现。
> 照搬 Atta Kid 后端 `referral/`，按 YardMate 三处拍板改：推荐人奖励**不限量叠加**、
> **正在付费的推荐人先攒着**、身份**验 Supabase JWT**（不是 App 自报）。
> 推荐奖励（朋友真扣费的 30% → Amazon 礼品卡）在 `rewards/`，见 `rewards/SPEC.md`；本包只给 `/me` 留 `RewardsReader` 钩子（`rewards` 字段）、把 `/claim` 的设备桶借给 `/payout`。

## 1. 五个问题（`AI_ENGINEERING_STANDARD.md` §6）

### 1.1 负责什么

- `GET /v1/referral/me`：给调用者造邀请码（第一次调才造）+ 回战绩。
- `POST /v1/referral/claim`：B 填 A 的码 → 记 A→B → 从码池发一张 App Store 一次性兑换码。
- `POST /v1/referral/redeemed`：App 回报「兑了」→ 只是让结算早点去查，不作凭据。
- 结算定时器（每 10 分钟）：查 RevenueCat 推进状态，给推荐人赠会员（`settle.go`）。
- 删账号时删掉这个人的邀请码、推荐记录（两侧）、奖励账本（`DeleteUser`，由 `account_delete.go` 调）。
- 三个接口各自的按设备限流桶（claim 5/24h，me + redeemed 30/h），自己扫。

### 1.2 不负责什么

- 验 JWT 的实现 —— 复用 `account_delete.go` 的 `verifySupabaseToken` + JWKS 缓存，
  经 `Authenticator` 注入（`referral_wiring.go`）；本包不认识 `SUPABASE_URL`。
- 按 IP 限流 —— `/v1` 作用域的 `PerIPMiddleware` 已覆盖。
- 码池补货 —— `deploy/supabase/import-codes.sh`（CSV → `code_pool`）。
- 苹果兑换本身、App 端判「从没订阅过」（`Transaction.all`）、App Store Connect 的 offer 配置。
- 收回已发出的 RevenueCat 赠送（删账号也不收）。

### 1.3 输入

| 来源 | 内容 |
|---|---|
| 请求头 | `Authorization: Bearer <Supabase access token>`、`X-Device-Install-Id`（UUID）、`X-App-Version` |
| `claim` body | `{"code": "ym 7kq2", "subscribed_before": false}` |
| `redeemed` body | `{"offer_code": "XK7P2M9Q"}` |
| vault | `REVENUECAT_SECRET_KEY`（v1 secret）、`REVENUECAT_ENTITLEMENT_ID`、`REFERRAL_SETTLE_INTERVAL`（可选，默认 10m）|
| 库 | migration `013_referral.sql` 的五张表 + 两个函数（经共享 pgx 池 `enrichment.DB.Pool()`）|
| RevenueCat | `GET /v1/subscribers/{id}`（**不存在会创建**，只对验过签的 user id 调）、`POST .../entitlements/{ent}/promotional {"end_time_ms"}` |

### 1.4 输出

`/me` → `{code, invited, months_earned, months_pending, months_saved, can_claim}`；
`claim` → `{offer_code}`；`redeemed` → `{status:"ok"}`。错误体 `{"error":"<code>"}`：

| HTTP | code | 谁给的 |
|---|---|---|
| 400 | `missing_device_id` / `missing_app_version` / `bad_json` | 请求形状 |
| 400 | `bad_code` | `Normalise` 不认 / `offer_code` 不像码 |
| 400 | `unknown_code` / `own_code` / `already_claimed` | SQL `referral_claim` 的 `raise exception` |
| 400 | `not_new_user` | App 报 `subscribed_before`，或 RevenueCat 有本人的 store 订阅记录 |
| 401 | `unauthorized` | token 验签失败（不说哪一步） |
| 429 | `rate_limit_device` / `rate_limit_ip` | 本包的桶 / `/v1` 的 IP 桶 |
| 502 | `store_failed` | 库或 RevenueCat 不可达 |
| 503 | `pool_empty` | 码池没货（补货后自愈） |
| 503 | `not_configured` | 四个依赖缺任一个 |

### 1.5 外部依赖

`github.com/jackc/pgx/v5`（共享池）、`net/http`（RevenueCat）、本仓库 `ratelimit`。
不引 `chi`（路由在 `server.go` 挂）。

## 2. 一次推荐怎么走完

```
A /me ──造码 YM-XXXX──▶ A 分享
B /claim ──RC 查 B 无订阅史──▶ referral_claim(): referrals(A→B, claimed) + code_pool 一张 issued
B 在苹果兑换 ──▶ B /redeemed（只记 redeem_reported_at，并 nudge 结算）
结算 ①：RC 上 B 有 store 订阅且 original_purchase_date > claimed_at → redeemed（redeemed_at = 那个时间）
结算 ②：redeemed_at + hold_days ≤ now，且该订阅 refunded_at 为空 → qualified；退款 → void
结算 ③：A 的 RC 没有生效中的 store 订阅 → 赠：end = max(now, 账本 promo_until, RC 现有赠送到期) + 30d
        A 有（付费 / 试用 / intro / 朋友的免费月 / 家人共享）→ saved
结算 ④：有 saved 的 A 不再被 store 订阅覆盖 → 一次赠 n × 30d，全部 saved → rewarded
```

RevenueCat 的 app user id = Supabase user id **大写**（iOS 传 `UUID.uuidString`，RC id 区分大小写），
见 `AppUserID`。

### 2.1 状态机（`referrals.state`）

`claimed → redeemed → qualified → (rewarded | saved → rewarded)`，任一步可 `void`（退款 / 查不到订阅）。
`/me` 的数字：`invited` = 非 void 条数；`months_earned` = rewarded + saved；
`months_pending` = redeemed + qualified；`months_saved` = saved；`can_claim` = 没填过 **且** RC 无订阅史。

### 2.2 幂等 / 并发

- 造码、领码是 plpgsql 函数（`raise exception` 整体回滚；`for update skip locked` 取库存）。
- 同一人同一码再 claim → 拿回同一张；那张过期了（`expires_at` 过）→ 划 expired、换新。
- `/redeemed` 重复调无副作用；`ListClaimed` 只看 200 天内的（码最长 6 个月）。
- 赠送先 RC 后账本；账本写失败会大声记日志，下一轮**再赠一次**（宁多发不少发）。
- `end_time_ms` 与现有赠送到期相差 < 2h 会被 RC 当重复忽略；我们每次至少 +30d。

### 2.3 限流

| 接口 | 桶 | 在哪扣 |
|---|---|---|
| `/claim` | 5 / 24h / 设备 | handler 内，在 `bad_json` / `bad_code` / `subscribed_before` 之后、RC 之前（打错字不扣） |
| `/me`、`/redeemed` | 30 / 1h / 设备 | `ReadLimiter()` 中间件 |

两个桶在结算 tick 里 `Sweep`。IP 桶沿用 `/v1` 作用域。

## 3. 配置与运维

- 四个依赖：`SUPABASE_URL`、`SUPABASE_DB_URL`、`REVENUECAT_SECRET_KEY`、`REVENUECAT_ENTITLEMENT_ID`。
  缺任一 → 路由仍注册，答 `503 not_configured`；结算不启动（`StartSettler` 打日志退出）。
- `REVENUECAT_ENTITLEMENT_ID` 就是 RevenueCat 后台显示的 identifier 字符串，
  本项目实测为 `YardMate: AI Plant ID & Care Pro`（含空格冒号，写进 secrets.env 不加引号）。
- 迁移：`proxy/enrichment/migrations/013_referral.sql`，可重复跑；**先 staging 后 prod，且先于二进制**。
  删账号路径把「表不存在」当无事可删（42P01 → nil），所以顺序反了也不会把删账号打成 500。
- `referral_settings.hold_days`（默认 7）改库即生效，不用发版。
- 补货：`SUPABASE_URL=… SUPABASE_SERVICE_ROLE_KEY=… deploy/supabase/import-codes.sh codes.csv "Referral Friend Month" 2027-04-19`。
- 冒烟：`TOKEN_A=… TOKEN_B=… deploy/referral-smoke.sh`（默认打 staging）。
- nginx：`location /v1/` 前缀转发已覆盖 `/v1/referral/*`，**不需要改 vhost**。
- 日志只有计数和错误文本，没有 user id、token、码。

## 4. 测试

`go test ./referral/`：码归一化；三个接口每个错误码分支（缺头 / 503 / 401 / 400 系 / 429 / 502 / 503 pool_empty）；
RevenueCat 对象解码与五个判断（订阅史 / 填码后开始 / 退款 / 生效中 / 赠送到期）；Client 的路径、鉴权头、`end_time_ms`；
结算：直接发 / 攒着 → 付费结束补发 / 多个 saved 一次发 / 第 11 个照样 +30d / 退款不算 /
取消不退款照算 / 填码前的订阅不算 / hold_days 读设置 / 账本写失败重发 / RC 挂了跳过。
SQL 函数在 staging 用临时 id 实跑过（own_code / unknown_code / 领码 / 重领同一张 / pool_empty / already_claimed）。

## 5. 待确认

- 推荐人正在付费时的「覆盖」把家人共享也算进去（赠了也用不上）；要不要反过来算「没付费」直接发。
- `ListClaimed` 200 天截止之后那条 referral 永远停在 claimed；要不要定期 void。
- RevenueCat 对一个 customer 多次 `end_time_ms` 赠送是「替换到期日」还是「叠加」—— 文档只说 2h 内算重复；
  我们按「替换为更晚的到期日」理解，并把 RC 现有到期日也算进基准。prod 第一笔赠送后看一眼 customer 页核实。
