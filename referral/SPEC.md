# `referral` package — 推荐好友（/v1/referral/*）

> 产品文档（源真）：yardmate-swiftui `docs/releases/v1/main-navigation/more/invite-friends/invite-friends.md`。
> 接口路径、字段、错误码按那份文档原样；本文只写后端怎么实现。
> 照搬 Atta Kid 后端 `referral/`，按 YardMate 三处拍板改：推荐人奖励**不限量叠加**、
> **正在付费的推荐人先攒着**、身份**验 Supabase JWT**（不是 App 自报）。

## 1. 五个问题（`AI_ENGINEERING_STANDARD.md` §6）

### 1.1 负责什么

- `GET /v1/referral/me`：给调用者造邀请码（第一次调才造）+ 回战绩。
- `POST /v1/referral/claim`：B 填 A 的码 → 记 A→B → 从码池发一张 App Store 一次性兑换码。
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
| `claim` body | `{"code": "ym 7kq2", "subscribed_before": false}` |
| `redeemed` body | `{"offer_code": "XK7P2M9Q"}` |
| vault | `REVENUECAT_SECRET_KEY`（v1 secret）、`REVENUECAT_ENTITLEMENT_ID`、`REFERRAL_SETTLE_INTERVAL`（可选，默认 10m）|
| 库 | migration `013_referral.sql` 的五张表 + 两个函数，`014_referral_pool_margin.sql` 的四列设置 + 改过的 `referral_claim`（经共享 pgx 池 `enrichment.DB.Pool()`）|
| 告警邮件 | `FEEDBACK_SMTP_FROM/PASS` + `FEEDBACK_EMAIL_TO`（与意见反馈同一个账号、同一个收件箱）；缺任一 → 只打 WARN 日志 |
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
| 503 | `pool_empty` | 码池没有**留够余量**的 free 码（`expires_at > now() + issue_margin_days`；补货后自愈） |
| 503 | `not_configured` | 四个依赖缺任一个 |

### 1.5 外部依赖

`github.com/jackc/pgx/v5`（共享池）、`net/http`（RevenueCat）、本仓库 `ratelimit`。
不引 `chi`（路由在 `server.go` 挂）。

## 2. 一次推荐怎么走完

```
A /me ──造码 YM-XXXX──▶ A 分享
B /claim ──RC 查 B 无订阅史──▶ referral_claim(): referrals(A→B, claimed) + code_pool 一张 issued
                                （只发 expires_at > now + issue_margin_days 的，先到期的先发）
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
- `referral_settings` 改库即生效，不用发版：

  | 列 | 默认 | 用处 |
  |---|---|---|
  | `hold_days` | 7 | B 兑换后满几天 A 才拿得到 |
  | `issue_margin_days` | 14 | 发码余量：**新发**只发到期日在 `now + 这么多天` 之后的码 |
  | `swap_grace_days` | 3 | 再 claim 时手上那张 issued 码不足这么多天才换新；必须 < `issue_margin_days` |
  | `low_stock_threshold` | 100 | 可发码少于这个数 → 告警；0 = 两种告警都关（临期是投影的低库存） |
  | `expiry_warn_days` | 30 | 最早一批可发码在这么多天内到期、**且它死后可发数会低于阈值** → 告警；0 = 关；必须 > `issue_margin_days` 才可达 |

  `014_referral_pool_margin.sql` 加的四列 + 替换 `referral_claim`；**先于二进制**（新二进制每轮读这四列，
  列不存在只是每轮记一条 `pool status` 错误 + `rep.Errors`，结算其它步照跑；旧 `referral_claim` 也能用，只是没余量）。
  组合不可达（`expiry_warn_days <= issue_margin_days` / `swap_grace_days > issue_margin_days`）→ 巡检打一条
  `WARN referral pool settings: …`，改好之前不重复、改好后再错再打。
- 码池告警（`pool.go`）：结算每轮算一次可发数 / 最早到期 / 它死后还剩多少 / 各批剩余；`low_stock`（可发 < 阈值）
  或 `expiring`（最早可发批 `expiry_warn_days` 内到期 **且** 死后可发 < 阈值——投影口径，后面有新批顶着就不吵）
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
- nginx：`location /v1/` 前缀转发已覆盖 `/v1/referral/*`，**不需要改 vhost**。
- 日志只有计数和错误文本，没有 user id、token、码。

## 4. 测试

`go test ./referral/`：码归一化；三个接口每个错误码分支（缺头 / 503 / 401 / 400 系 / 429 / 502 / 503 pool_empty）；
RevenueCat 对象解码与五个判断（订阅史 / 填码后开始 / 退款 / 生效中 / 赠送到期）；Client 的路径、鉴权头、`end_time_ms`；
结算：直接发 / 攒着 → 付费结束补发 / 多个 saved 一次发 / 第 11 个照样 +30d / 退款不算 /
取消不退款照算 / 填码前的订阅不算 / hold_days 读设置 / 账本写失败重发 / RC 挂了跳过；
码池巡检（`pool_test.go`）：余量口径（恰在 cutoff 上不算）/ 最早到期取可发批次里最早的（同日期的批一起算死后余量）/
投影口径（后面有 ≥ 阈值顶着不报、不够才报、恰好等于阈值不报）/ 阈值与 0 = 关 / 不可达组合的 `SettingsProblem` /
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

- 推荐人正在付费时的「覆盖」把家人共享也算进去（赠了也用不上）；要不要反过来算「没付费」直接发。
- `ListClaimed` 200 天截止之后那条 referral 永远停在 claimed；要不要定期 void。
- RevenueCat 对一个 customer 多次 `end_time_ms` 赠送是「替换到期日」还是「叠加」—— 文档只说 2h 内算重复；
  我们按「替换为更晚的到期日」理解，并把 RC 现有到期日也算进基准。prod 第一笔赠送后看一眼 customer 页核实。
- 再 claim 换码（老码不足 3 天宽限）时：如果 B 其实已经在苹果那边用旧码兑了、结算还没跑到（≤ 10 分钟）、
  App 也没报 `/redeemed`，旧码在库里被划 expired、新码白发一张（结算 ① 照样按第一次填码对上 RC 订阅，A 的奖励不受影响；
  `MarkRedeemed` 会把新码记成 redeemed，库存账少一张）。窗口极窄（老码只剩 ≤ 3 天 × 结算 10 分钟内），先记着。
