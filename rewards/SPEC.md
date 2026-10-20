# `rewards` package — 推荐奖励（朋友真扣费的 30% → Amazon.com 礼品卡）

> 产品文档（源真）：yardmate-swiftui `docs/releases/v1/main-navigation/more/invite-friends/rewards/rewards.md`。
> 接口路径、字段、错误码、记账规则按那份文档原样；本文只写后端怎么实现。
> 与 `referral/`（双方各一个月）**叠加**、互不影响：那边送月份，这边记钱。

## 1. 五个问题（`AI_ENGINEERING_STANDARD.md` §6）

### 1.1 负责什么

- `POST /v1/revenuecat/webhook`：收 RevenueCat 事件 → 朋友的真实扣费记一笔 **pending**、苹果退款把 pending 置 **voided**（`webhook.go`）。
- 入账定时器（每 10 分钟，自己的循环，与 referral 结算同一个 `REFERRAL_SETTLE_INTERVAL`）：`available_at <= now()` 的 pending → **available**（`rewards.go`）。
- `GET /v1/referral/me` 的 `rewards` 字段：经 `referral.RewardsReader` 注入（`Rewards`）；本包不碰 `/me` 别的字段。
- `POST /v1/referral/payout`：可兑余额 ≥ $100 → 建一条 `requested` 申请（固定 $100）→ 给运营发邮件（`payout.go`）。
- 运营侧：`deploy/referral-payout.sh list | mark-sent <id> | cancel <id>`（PostgREST + service role）。

### 1.2 不负责什么

- 送月份、推荐状态机（`referral/`）；本包只**读** `referrals`（找朋友、看是否已兑换）。
- 验 JWT 的实现 —— 复用 `account_delete.go` 的 `verifySupabaseToken`，经 `Authenticator` 注入（`rewards_wiring.go`）。
- 设备限流桶 —— 借 `referral.Service.ClaimBucket()`（`/claim` 那一个，5 / 24h），由 referral 结算轮清扫。
- 买卡、发卡、报税（运营手动）；`referral_rewards_enabled` 开关（只管 App 展示，服务端记账不看它）。
- 删账号 —— 表外键挂在 `referral_users` 上，`referral.DeleteUser` 删那行时级联（见 §2.4）。

### 1.3 输入

| 来源 | 内容 |
|---|---|
| webhook 头 | `Authorization`：与 `REVENUECAT_WEBHOOK_SECRET` 常量时间比较，两边带不带 `Bearer ` 都行 |
| webhook body（≤ 64 KB）| `{"event": {...}, "api_version": "1.0"}`；读 `id, type, environment, app_user_id, product_id, period_type, price, takehome_percentage, transaction_id, original_transaction_id, purchased_at_ms, cancel_reason`，另读 `is_family_share` |
| `/payout` 头 | 同 `/claim`：`Authorization: Bearer <Supabase access token>`、`X-Device-Install-Id`、`X-App-Version` |
| `/payout` body | `{"email": "you@example.com"}` |
| vault | `REVENUECAT_WEBHOOK_SECRET`；`SUPABASE_URL` / `SUPABASE_DB_URL`（与 referral 共用）；`FEEDBACK_SMTP_*` / `FEEDBACK_EMAIL_TO`（运营邮件，缺了只记日志）；`REFERRAL_SETTLE_INTERVAL`（可选）|
| 库 | migration `015_referral_rewards.sql` 的 `reward_ledger` / `reward_payouts`（依赖 013 的 `referral_users` / `referrals`）|

### 1.4 输出

`/me` 的 `rewards` → `{available_cents, pending_cents, lifetime_cents, payout}`，`payout` = 最近一条（任何状态）或 `null`，
时间字段 RFC 3339、UTC、整秒（`"2026-10-20T12:00:00Z"`），`sent_at` 没发时 `null`。
`/payout` 201 → `{"payout": {...}}`（同上 object，`state == "requested"`）。webhook 200 → `{"status": "<outcome>"}`
（`recorded` / `duplicate` / `voided` / `nothing_to_void` / `not_counted` / `not_referee` / `not_redeemed` / `ignored_type` / `ignored_environment`，
RevenueCat 后台的投递记录里看得到）。错误体 `{"error":"<code>"}`：

| 接口 | HTTP | code | 说明 |
|---|---|---|---|
| payout | 400 | `missing_device_id` / `missing_app_version` / `bad_json` | 请求形状（同 referral）|
| payout | 400 | `invalid_email` | `net/mail` 解析不成裸地址（不许显示名 / 尖括号）、无 `@`、> 254 字符 |
| payout | 401 | `unauthorized` | token 验签失败 |
| payout | 409 | `payout_in_progress` | 已有 `requested`（**先于**余额判断：$100 申请中、余额 0 时也答这个）|
| payout | 409 | `below_threshold` | 可兑余额 < $100（没当过推荐人也是这个）|
| payout | 429 | `rate_limit_device` | `/claim` 的设备桶（共用次数）|
| payout | 502 | `store_failed` | 库不可达 |
| payout | 503 | `not_configured` | 缺库或缺 `SUPABASE_URL` |
| webhook | 400 | `bad_json` / `bad_event` | body 不是 JSON；要记账的事件缺 `id` / `transaction_id` 或价格大到放不进 int4 |
| webhook | 401 | `unauthorized` | 头对不上 |
| webhook | 413 | `body_too_large` | > 64 KB |
| webhook | 500 | `store_failed` | 库出错 → RevenueCat 重试 |
| webhook | 503 | `not_configured` | 没配 `REVENUECAT_WEBHOOK_SECRET`（不看头，直接 503）或没库（过了鉴权之后）|

### 1.5 外部依赖

`github.com/jackc/pgx/v5`（共享池）、`math/big`（金额精确十进制）、`net/mail`、本仓库 `ratelimit`。
发信走 `*enrichment.FeedbackMailer.Alert`（经 `Mailer` 接口注入）。不引 `chi`、不 import `referral`（钩子方向是 referral 定义接口、这边实现）。

## 2. 规则

### 2.1 记账（webhook）

```
environment != PRODUCTION                                   → 200 ignored_environment
type ∈ INITIAL_PURCHASE / RENEWAL:
    period_type ∉ {NORMAL, INTRO} / is_family_share / price ≤ 0 或缺 → 200 not_counted
    lower(app_user_id) 不是 uuid / 不是任何 referrals.referee   → 200 not_referee
    该推荐不算已兑换                                          → 200 not_redeemed
    缺 event.id / transaction_id                              → 400 bad_event
    insert pending（rc_event_id 撞了 → 200 duplicate）        → 200 recorded
type == CANCELLATION 且 cancel_reason == CUSTOMER_SUPPORT     → 同 transaction_id 的 pending → voided
其他（TRANSFER / EXPIRATION / PRODUCT_CHANGE / TEST / 别的 CANCELLATION …）→ 200 ignored_type
```

- **已兑换**：`referrals.state` ∈ `redeemed / qualified / saved / rewarded`；另外 `claimed` 且这笔 `purchased_at` 晚于 `claimed_at` 也算
  —— 那正是 referral 结算第 ① 步十分钟内会认定为 redeemed 的情形，朋友填码后直接付费的第一笔不能被这十分钟竞态吃掉。`void` 不算。
- **金额**：`credit_cents = round(price × takehome_percentage × 0.30 × 100)`，`price_usd_cents = round(price × 100)`，
  在 JSON 原文的十进制上用 `big.Rat` 算、半数进位（float64 会把 4.5 算成 4.499…）。`takehome_percentage` 缺 / null → 0.85，
  不在 (0, 1] → 0.85 并记日志。算出来 0 分（几美分的价格）→ 不记。典型值：$39.99 → 1020、$6.99 → 178、$0.29 intro → 7。
- **时间**：`purchased_at = purchased_at_ms`（缺 → 收到时间），`available_at = purchased_at + 90 天`。
- **app_user_id**：RevenueCat 里是 Supabase uuid **大写**，库里是小写 → 统一 `ToLower` 再比。`$RCAnonymousID:…` 直接 not_referee。
- **退款**只按 `transaction_id` 找 pending，不再看 app_user_id（账本里本来只有朋友的扣费；TRANSFER 之后来的退款也能找到）。
  已 available 的不追回。`environment != PRODUCTION` 的退款同样忽略。

### 2.2 余额口径（`Summarize`）

```
lifetime_cents  = sum(available 账本)
available_cents = max(0, lifetime_cents − sum(requested + sent 申请))
pending_cents   = sum(pending 账本)
```

`cancelled` 不在扣减里，所以运营 `cancel` 之后 $100 自然回到余额，不用回写任何东西。voided 永远不算。

### 2.3 兑换（`RequestPayout`，一个事务）

1. `select … from referral_users where user_id = $1 for no key update` —— 同一人的并发申请在这里排队（no key update 不挡别的表插 FK）。没这行 → `below_threshold`。
2. 有 `requested` → `payout_in_progress`；可兑余额 < 10000 → `below_threshold`。
3. `insert reward_payouts (user_id, amount_cents = 10000, email)`；partial unique index `(user_id) where state = 'requested'` 兜底，撞了也答 `payout_in_progress`。
4. 同事务读该用户**本自然年（UTC）**的 `sent`（按 `sent_at`）+ `requested`（含这一张）合计 → 进运营邮件。

邮件异步发（不拖 201，panic 也兜住）：主题 `[YardMate] Referral gift card request #<id>`（纯 ASCII），
正文 user_id、收卡邮箱、$100、本年已发 / 申请中 / 合计、mark-sent / cancel 命令；**合计 ≥ $500**（含这一张）时加一行
`W-9 FIRST (先收 W-9 再发)`。邮件丢了不影响记录，`deploy/referral-payout.sh list` 兜底。

### 2.4 删账号

- 推荐人删：`referral_users` 行没了 → `reward_ledger.referrer` / `reward_payouts.user_id` **级联删**（余额作废；requested 的照发，运营看邮件，脚本 mark-sent 会报「不存在」）。
- 朋友删：`referrals` 那行级联没了 → `reward_ledger.referee` / `referral_id` **置空**，推荐人已记的钱不动；之后该 app_user_id 的事件 not_referee。

### 2.5 幂等 / 并发

- RevenueCat 重发同一事件（同 `event.id`）→ `on conflict (rc_event_id) do nothing` → 200 duplicate。
- 退款重复到达 → 第二次 nothing_to_void。入账 update 只动 pending，重复跑无副作用。
- 我们出错一律 500（RevenueCat 指数退避重试）；永远不会接受的请求是 4xx；看过但不记的一律 200（否则 RevenueCat 会一直重发）。

## 3. 配置与运维

- 迁移：`proxy/enrichment/migrations/015_referral_rewards.sql`（依赖 013，可重复跑），**先 staging 后 prod，且先于二进制**。
  表不存在时：webhook 记账答 500（RevenueCat 重试，迁移补上后自愈）、`/me` 省略 `rewards` 字段并记日志、`/payout` 502。
- `REVENUECAT_WEBHOOK_SECRET`：`openssl rand -hex 32`，同一个值填 secrets.env 与 RevenueCat 后台 Webhooks 的 Authorization header。
- 路由：`server.go` 注册 `/v1/revenuecat/webhook`（`/v1` 作用域的按 IP 限流 600/h 照常生效，RevenueCat 被 429 会重试）、
  `/v1/referral/payout`（挂在 referral 路由组里）；服务总是构建，缺配置答 503 不是 404。nginx `location /v1/` 已覆盖。
- 发卡流程与脚本：`deploy/README.md` §8。
- 日志只有类型、金额、计数、payout id 和错误文本，没有 user id、邮箱、transaction id、token。

## 4. 测试

`go test ./rewards/`（fakes 照 `referral/*_test.go` 风格，`memStore` 复刻 SQL 规则）：
webhook 鉴权十种头 / 未配置 / 坏 body / 64 KB；文档每条记与不记（NORMAL / INTRO 记，TRIAL / PROMOTIONAL / $0 / null / 负价 / 家庭共享 / SANDBOX / 非朋友 / 匿名 id 不记，大小写）；
各推荐状态（含 claimed 前后、void）；金额四舍五入表（含正好 .5）；90 天；去重；退款只 void pending；库错误 500；
入账只动到期 pending；余额口径（cancelled 回余额、不为负）；时间 RFC 3339 无小数秒；
payout 三个错误码 + 201、头 / 鉴权 / 未配置、邮箱黑白名单、共享设备桶（邮箱错不扣次数）、20 路并发只成一张、库错误 502、无邮件 / 邮件失败照 201、
邮件正文与 W-9 阈值（$400 无、$500 有、去年的不算）、主题纯 ASCII。
`go test .`：两条路由已注册、未配置答 503 / 401。`go test ./referral/`：`/me` 有 / 无 / 失败三种、共享桶。

SQL（013 + 015 两遍）在真库上的测试：`REWARDS_PG_TEST_URL=postgres://…/scratch go test ./rewards/ -run PG -v`
（临时 schema，跑完即删；**绝不要指向 staging / prod**）。**本 PR 写的时候本机没有 Postgres，这组还没实跑过。**

## 5. 待确认

- `claimed` 状态的朋友付费也算（§2.1）是对文档「已兑换」的实现解释，文档没写到这个竞态。
- `is_family_share` 不算是按「真扣费」推的，文档字段表里没有这个字段。
- `payout_in_progress` 先于 `below_threshold` 判断；文档两条 409 没定先后。
- W-9 提醒按「本年已发 + 申请中（含这一张）≥ $500」算，比「之前已到 $500 的下一次」早一张提醒。
- 乱序：退款先于购买到达（RevenueCat 很少见）时那笔会留成 pending、90 天后入账。
- 朋友删账号后推荐人已记的钱保留（文档只写了推荐人删账号）。
