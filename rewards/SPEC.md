# `rewards` package — 推荐奖励（朋友真扣费的 30% → Amazon.com 礼品卡）

> 产品文档（源真）：yardmate-swiftui `docs/releases/v1/main-navigation/more/invite-friends/rewards/rewards.md`。
> 接口路径、字段、错误码、记账规则按那份文档原样；本文只写后端怎么实现。
> 与 `referral/`（双方各一个月）**叠加**、互不影响：那边送月份，这边记钱。

## 1. 五个问题（`AI_ENGINEERING_STANDARD.md` §6）

### 1.1 负责什么

- `POST /v1/revenuecat/webhook`：收 RevenueCat 事件 → 朋友的真实扣费记一笔 **pending**、苹果退款把 pending 置 **voided**；
  退款先于扣费到达时记下交易号，那笔扣费晚到也不入账（`webhook.go`）。
- 入账定时器（每 10 分钟，自己的循环，与 referral 结算同一个 `REFERRAL_SETTLE_INTERVAL`）：`available_at <= now()` 的 pending → **available**（`rewards.go`）。
- `GET /v1/referral/me` 的 `rewards` 字段：经 `referral.RewardsReader` 注入（`Rewards`）；本包不碰 `/me` 别的字段。
- `POST /v1/referral/payout`：可兑余额 ≥ $100 → 建一条 `requested` 申请（固定 $100）→ 给运营发邮件（`payout.go`）；
  邮件就是防刷第 5 道的人工审核，附这个推荐人的朋友数 / 朋友设备去重数 / 退款笔数 / 作废与待审核推荐数（`ReviewStats`，§2.3）。
- 运营侧：`deploy/referral-payout.sh list | mark-sent <id> | undo <id> | cancel <id>`（PostgREST + service role；`mark-sent` 两次确认、标记后 1 小时内可 `undo`，§2.7）。

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
| vault | `REVENUECAT_WEBHOOK_SECRET`；`SUPABASE_URL` / `SUPABASE_DB_URL`（与 referral 共用）；`FEEDBACK_SMTP_*` / `FEEDBACK_EMAIL_TO`（运营邮件，main 经 `mail_wiring.go` 建一次、与 feedback / 码池告警共用，缺了只记日志）；`REFERRAL_SETTLE_INTERVAL`（可选）|
| 库 | migration `015_referral_rewards.sql` 的 `reward_ledger` / `reward_voided_txns` / `reward_payouts`（依赖 013 的 `referral_users` / `referrals`）；`016_referral_antiabuse.sql` 的 `reward_ledger.void_reason`、`referrals.claim_install_id` |

### 1.4 输出

`/me` 的 `rewards` → `{available_cents, pending_cents, lifetime_cents, payout, counts, referrals}`，`payout` = 最近一条（任何状态）或 `null`，
时间字段 RFC 3339、UTC、整秒（`"2026-10-20T12:00:00Z"`），`sent_at` 没发时 `null`。
`counts` / `referrals`（2026-10-21 加，§2.6）只加不改：读进度出错时两个都不出（只记日志），前四个照常；`referrals` 为空时也不出。
`/payout` 201 → `{"payout": {...}}`（同上 object，`state == "requested"`）。webhook 200 → `{"status": "<outcome>"}`
（`recorded` / `duplicate` / `voided` / `nothing_to_void` / `not_counted` / `not_referee` / `not_redeemed` / `ignored_type` / `ignored_environment`，
RevenueCat 后台的投递记录里看得到；`voided` 既指退款作废了 pending，也指扣费晚于自己的退款到达、没入账）。错误体 `{"error":"<code>"}`：

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
| webhook | 429 | `rate_limit_ip` | 同一 IP 每分钟 > 600 次（自己的桶，不占 `/v1` 的 600 / h）|
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
    这笔交易已在 reward_voided_txns（退款先到了）             → 200 voided（不写）
    insert pending（rc_event_id 或 transaction_id 撞了 → 200 duplicate）→ 200 recorded
type == CANCELLATION 且 cancel_reason == CUSTOMER_SUPPORT:
    同 transaction_id 的 pending → voided                     → 200 voided
    没有 pending → 交易号记进 reward_voided_txns              → 200 nothing_to_void
其他（TRANSFER / EXPIRATION / PRODUCT_CHANGE / TEST / 别的 CANCELLATION …）→ 200 ignored_type
```

- **已兑换**：`referrals.state` ∈ `redeemed / qualified / held / saved / rewarded`（`held` = referral 防刷第 4 道的待审核，朋友已兑换，钱照记、照常过 90 天；
  reject 时由 SQL `referral_void` 连带作废）；另外 `claimed` 且这笔是 **`INITIAL_PURCHASE`**、`purchased_at` 晚于 `claimed_at` 也算
  —— 那正是 referral 结算第 ① 步十分钟内会认定为 redeemed 的情形，朋友填码后直接付费的第一笔不能被这十分钟竞态吃掉。
  `claimed` 下的 `RENEWAL` **不算**（`not_redeemed`）：续费说明订阅早于填码，典型是小号领码后「恢复购买」把老订阅转过来，
  算了就会每期都给推荐人记钱。真朋友的续费不受影响：referral 结算 ① 每 10 分钟一轮、批次轮转（referral 017，积压再多新来的也轮得到），
  首购之后很快就认定 redeemed，第一次续费（最早一个月后）到的时候早已不是 `claimed`（2026-10-20 定：后端保持现状，产品文档跟着改）。`void` 不算。
- **价格缺 / null**：不记，并打一行日志（只有事件类型和 period_type，不带任何 id）—— 真扣费没价格会悄悄少记推荐人一笔，留个痕。
- **金额**：`credit_cents = round(price × takehome_percentage × 0.30 × 100)`，`price_usd_cents = round(price × 100)`，
  在 JSON 原文的十进制上用 `big.Rat` 算、半数进位（float64 会把 4.5 算成 4.499…）。`takehome_percentage` 缺 / null → 0.85，
  不在 (0, 1] → 0.85 并记日志。算出来 0 分（几美分的价格）→ 不记。典型值：$39.99 → 1020、$6.99 → 178、$0.29 intro → 7。
- **时间**：`purchased_at = purchased_at_ms`（缺 → 收到时间），`available_at = purchased_at + 90 天`。
- **app_user_id**：RevenueCat 里是 Supabase uuid **大写**，库里是小写 → 统一 `ToLower` 再比。`$RCAnonymousID:…` 直接 not_referee。
- **退款**只按 `transaction_id` 找 pending，不再看 app_user_id（账本里本来只有朋友的扣费；TRANSFER 之后来的退款也能找到）。
  已 available 的不追回。`environment != PRODUCTION` 的退款同样忽略。作废时记 `void_reason = 'refunded'`（016）。
- **推荐整条作废**（referral 防刷第 3 道 `duplicate_apple_id` / 第 4 道或礼品卡审核的 `rejected`）：不在本包——SQL `referral_void`（016）在同一事务里
  把这条推荐名下 pending + available 的账本一起标 `voided`，`void_reason` 记推荐的作废原因；之后的事件因为推荐是 void 答 `not_redeemed`。
  原因是退款 / 没订阅（`refunded` / `no_subscription`）时账本不动（退款那笔 webhook 自己会作废）。voided 的 available 不再算余额，
  已申请过的礼品卡照常扣，所以余额可能被压到 0（`Summarize` 不显示负数）。
- **退款先于扣费到达**（扣费那次投递 500、RevenueCat 稍后重试，退款反而先到）：作废时没命中 pending → 交易号记进
  `reward_voided_txns`；扣费事件到了，insert 带 `where not exists (… reward_voided_txns …)` 被挡下 → 200 `voided`。
  没命中 pending 的退款一律记（已 available / 不是朋友的也记，无害：前者账本里已有这笔、transaction_id 唯一；后者永远不会来扣费）。

### 2.2 余额口径（`Summarize`）

```
lifetime_cents  = sum(available 账本)
available_cents = max(0, lifetime_cents − sum(requested + sent 申请))
pending_cents   = sum(pending 账本)
```

`cancelled` 不在扣减里，所以运营 `cancel` 之后 $100 自然回到余额，不用回写任何东西。voided 永远不算。

### 2.3 兑换（`RequestPayout`，一个事务）

1. `select … from referral_users where user_id = $1 for no key update` —— 同一人的并发申请在这里排队（no key update 不挡别的表插 FK）。没这行 → `below_threshold`。
2. 有 `requested`，或有还在 1 小时缓冲里的 `sent`（`undo_until > now()`，§2.7）→ `payout_in_progress`；可兑余额 < 10000 → `below_threshold`。
3. `insert reward_payouts (user_id, amount_cents = 10000, email)`；partial unique index `(user_id) where state = 'requested'` 兜底，撞了也答 `payout_in_progress`。
4. 同事务读该用户**本自然年（UTC）**的 `sent`（按 `sent_at`，含缓冲中的）+ `requested`（含这一张）合计 → 进运营邮件。

邮件异步发（不拖 201，panic 也兜住）：主题 `[YardMate] Referral gift card request #<id>`（纯 ASCII），
正文 user_id、收卡邮箱、$100、本年已发 / 申请中 / 合计、mark-sent（附「两次确认、1 小时内可 undo」）/ undo / cancel 命令；**合计 ≥ 门槛 − $100**（含这一张；门槛 =
`referral_settings.w9_threshold_cents`，migration 019，默认 200000 = $2,000，2026 年起的 1099 门槛；在发信的 goroutine 里读，读不到按 $2,000 记日志）时加
`W-9 FIRST (先收 W-9 再发；已经收过……就照发)`，写出门槛金额、要求加密收 W-9、模板在 rewards.md `### 税务：W-9`；不写 1099 表名（NEC / MISC 待会计确认）。邮件丢了不影响记录，`deploy/referral-payout.sh list` 兜底。

**审核那几行**（防刷第 5 道，rewards.md `### 入账与兑换`；`ReviewStats` 在发信的 goroutine 里读，读失败邮件照发、写「numbers unavailable」）：

| 行 | 口径 |
|---|---|
| `Friends (not void): N, of whom M brought in money` | 这个推荐人没作废的推荐条数；其中账本里有没作废记账的推荐数 |
| `Friend devices (distinct): D of R with a recorded device → all different / NOT all different` | 没作废推荐的 `claim_install_id` 去重数 / 记了设备的条数（016 之前填的码没记设备，另行注明）|
| `Refunded charges: K` | 名下 `voided` 且 `void_reason` 为 `refunded`（016 之前的作废只可能是退款，迁移回填）的账本笔数 |
| `Void referrals: duplicate_apple_id a, rejected b, refunded c[, 其他原因…]` | 按 `referrals.void_reason` 分别数，三个主要原因 0 也列 |
| `Held (awaiting review): H` | 现在 `held` 的推荐数 |

后面跟一句：看着不对就 cancel 这张申请，再 `deploy/referral-review.sh reject <referral id>` 把刷的推荐作废（钱一起作废）。

### 2.4 删账号

- 推荐人删：`referral_users` 行没了 → `reward_ledger.referrer` / `reward_payouts.user_id` **级联删**（余额作废；requested 的照发，运营看邮件，脚本 mark-sent 会报「不存在」）。
- 朋友删：`referrals` 那行级联没了 → `reward_ledger.referee` / `referral_id` **置空**，推荐人已记的钱不动；之后该 app_user_id 的事件 not_referee。

### 2.7 标记已发的 1 小时缓冲（migration 018，rewards.md `### 标记已发`）

- `reward_payouts.undo_until`：`reward_payout_mark_sent(p_id, p_email)` 置 `state = sent, sent_at = now(), undo_until = now() + 1 hour`，只在 `state = requested` 且 `email = p_email`（脚本列给运营看的那个）时改；`reward_payout_undo(p_id)` 只在 `state = sent and undo_until > now()` 时改回 `requested`、清 `sent_at` / `undo_until`。两个都是 SQL 函数，脚本经 PostgREST `/rest/v1/rpc/` 调，只 grant 给 service_role。
- 缓冲期对外一律是 `requested`：`payoutColumns`（`Balance.Latest`、`RequestPayout` 的返回）在 SQL 里把 `state = sent and undo_until > now()` 读成 `requested`、`sent_at` 读成 null；`RequestPayout` 的「有没有在路上」也算上它。全部用数据库 `now()`，不用 Go 的 `s.now()`。
- 余额口径不变（`requested` / `sent` 都扣）；年度合计按库里的真实 `state`，缓冲中算「已发」、年份按 `sent_at`（标记时间）。
- 撤回回 `requested` 不会撞 partial unique index：缓冲期里同一人建不出第二张 `requested`。
- `cancel` 只认 `requested`（PATCH 带 `state=eq.requested`）：缓冲中的先 `undo`。

### 2.5 幂等 / 并发

- RevenueCat 重发同一事件（同 `event.id`）或同一笔交易换了 `event.id` 又来 → `rc_event_id`、`transaction_id` 两个唯一索引，
  insert 用**不带冲突目标**的 `on conflict do nothing` 让两个都生效 → 200 duplicate（账本里已有这笔，哪怕已 voided）。
- 退款重复到达 → 第二次 nothing_to_void（再记一次 reward_voided_txns 也是 no-op）。入账 update 只动 pending，重复跑无副作用。
- 同一交易号的记账和作废各自一个事务，开头 `pg_advisory_xact_lock(hashtextextended('reward_txn:' || transaction_id, 0))`
  串行化：扣费和退款同一瞬间处理时，不会出现「退款没看到 pending、扣费也没看到退款记录」两边都漏。
- 我们出错一律 500（RevenueCat 指数退避重试）；永远不会接受的请求是 4xx；看过但不记的一律 200（否则 RevenueCat 会一直重发）。

### 2.6 每条推荐的进度（`Progress`，progress.go）

奖励卡的三格人数和「ON THE WAY」清单（产品文档 `### 每条推荐的进度`）。`Store.Progress` 在一个只读 repeatable read 事务里读两条
（同一快照：两条之间朋友删号 / 推荐作废不会让同一个人出现两次）——推荐人没作废的 `referrals`（`id, state, claimed_at, redeemed_at,
coalesce(reissued_at, claimed_at)`，走 `referrals_referrer_idx`）和他的账本（全部 `pending` + 每条推荐**最近一笔** `available`，
`available` 只增不减、只用来判「付过钱」和套餐，所以 `distinct on` 只取一行；走 `reward_ledger_referrer_state_idx`）——
其余在 Go 里算（纯函数，单测全覆盖）。**不加迁移、不改 `referral_me`。** `Balance` 是另一条语句，清单合计偶尔与 `pending_cents` 差一笔刚到的钱，下次 `/me` 对齐。

```
有一笔 pending / available 账本            → paying     计 Paying；还有 pending 才进清单
否则 state = claimed                       → unredeemed 领码 / 换码 200 天内进清单（同结算 ① 的窗口），之后只计 Joined
否则 now < FreeMonthEnd(redeemed_at)       → free_month 计 Free month；进清单
否则（免费月过了、没记账）                    → 只计 Joined
账本 referral_id 为空（朋友删号）            → 合成一行 joined_at = null、paying，不计人数
账本 referral_id 不在没作废的推荐里（void）   → 跳过
```

- `joined = len(没作废的推荐)`，与 `referral_me` 的 `invited` 同口径；`joined_at = claimed_at`。
- `FreeMonthEnd` = UTC 下 +1 个日历月，下个月没有这一天取月底（1-31 → 2-28 / 29；`time.AddDate` 会滚到 3 月，不能直接用）。
- `plan` 取最近一笔（`purchased_at` 最大）的 `product_id`：`premium.yearly.3d` / `premium.yearly` / `premium.monthly`（老产品，实为年付）→ `yearly`，`premium.month` → `monthly`，其余不给。
- `next_*` = 最早 `available_at` 的那笔 pending。
- 排序 paying（next 升序）→ free_month（结束升序）→ unredeemed（加入降序），同值按 referral id；最多 50 行。
- 用 `Service.now`（应用时钟）判免费月，不用库的 `now()`。

## 3. 配置与运维

- 迁移：`proxy/enrichment/migrations/015_referral_rewards.sql`（依赖 013，可重复跑），**先 staging 后 prod，且先于二进制**。
  `019_referral_w9_threshold.sql`（`referral_settings.w9_threshold_cents`）先后都行：没跑时邮件按默认 $2,000 算并记日志。每年 1 月按 IRS 新值改这一列。
  `018_reward_payout_undo.sql`（`undo_until` + 两个 RPC 函数）同样先于二进制：新二进制的 SQL 读 `undo_until`，没跑 018 时 `/me` 的 `rewards` 读失败被省略、`/payout` 502。
  `016_referral_antiabuse.sql` 也先于二进制：退款作废写 `void_reason`、申请邮件读 `claim_install_id`，没跑 016 时退款事件答 500（RevenueCat 重试，跑了自愈）、
  邮件的审核行写「numbers unavailable」。
  表不存在时：webhook 记账答 500（RevenueCat 重试，迁移补上后自愈）、`/me` 省略 `rewards` 字段并记日志、`/payout` 502。
- `REVENUECAT_WEBHOOK_SECRET`：`openssl rand -hex 32`，同一个值填 secrets.env 与 RevenueCat 后台 Webhooks 的 Authorization header。
- 路由：`server.go` 在**顶层**注册 `/v1/revenuecat/webhook`，**不在** `/v1` 组里，不吃 App 用的每 IP 600 / h
  （RevenueCat 从少数几个 IP 推所有用户的事件，续费扎堆或补发积压时会被 429 进退避，晚记甚至丢）；它有密钥鉴权，
  但仍是公网 URL，所以照仓库「公网路由都有按 IP 限流」的习惯单给一个高得多的桶：每 IP 每分钟 600（`Service.WebhookLimiter`，
  桶由入账轮清扫）。静态路径，chi 先于 `/v1` mount 匹配。`/v1/referral/payout` 挂在 referral 路由组里。
  服务总是构建，缺配置答 503 不是 404。nginx `location /v1/` 已覆盖。
- 发卡流程与脚本：`deploy/README.md` §8。
- 日志只有类型、金额、计数、payout id 和错误文本，没有 user id、邮箱、transaction id、token。

## 4. 测试

`go test ./rewards/`（fakes 照 `referral/*_test.go` 风格，`memStore` 复刻 SQL 规则）：
webhook 鉴权十种头 / 未配置 / 坏 body / 64 KB；文档每条记与不记（NORMAL / INTRO 记，TRIAL / PROMOTIONAL / $0 / null / 负价 / 家庭共享 / SANDBOX / 非朋友 / 匿名 id 不记，大小写）；
各推荐状态（含 claimed 下首购前后、claimed 下续费不记、void）；金额四舍五入表（含正好 .5）；90 天；按 event id 和 transaction id 去重；
退款只 void pending；退款先到 → 留记录、晚到的扣费及其重发答 voided、别的交易不受影响；库错误 500；
入账只动到期 pending；余额口径（cancelled 回余额、不为负）；时间 RFC 3339 无小数秒；
payout 三个错误码 + 201、头 / 鉴权 / 未配置、邮箱黑白名单、共享设备桶（邮箱错不扣次数）、20 路并发只成一张、库错误 502、无邮件 / 邮件失败照 201、
邮件正文与 W-9 阈值（默认门槛 $2,000：$1,800 无、$1,900 有、$2,100 有、去年的不算；门槛改 $2,100 时 $1,900 无 / $2,000 有；读设置失败按 $2,000；不出现 1099-NEC / $600）、主题纯 ASCII；
PG `TestPGW9Threshold`（默认 200000、改了即读到）。
防刷（016）：webhook `held` 状态照记（首购 / 续费）；申请邮件审核行逐项（朋友数 / 带来钱的 / 设备去重含重复与没记设备 / 退款笔数只数退款 / 各作废原因含 0 和其他 / held）、
全不同的写法、读审核数据失败邮件照发且 201；PG：退款写 `void_reason = refunded`、`ReviewStats` 各口径（016 跑两遍）。

进度（§2.6，`progress_test.go`）：对比稿那张卡（年付 5 天、月付两笔取最早、免费月、未兑换、免费月过期只算 Joined）、线上 JSON 逐字、
全部到账移出清单 / 续费回来、免费月结束前一秒与正点、claimed 却已付费算 paying、删号朋友合一行不计人数、void 与 voided 跳过、
产品映射（含老 `premium.monthly`）与升级后取最近一笔、排序与 50 行上限、`/me` 带进度 / 读进度失败只去掉两个字段、
`FreeMonthEnd` 月底表（含闰年 / 跨年 / 带时区）、未兑换 200 天窗口（含换过码）；
PG `TestPGProgress`：两条查询的口径（void 不出、删号后 referral_id 为空仍在、voided 不出）。

标记缓冲（§2.7）：PG `TestPGMarkSentBufferAndUndo`：邮箱对不上不改、requested 不能 undo、标记后 `undo_until − sent_at = 1h`、重复标记不改、缓冲中 `Balance.Latest` 读成 requested 无 `sent_at` 且余额照扣、缓冲中 `/payout` 答 in progress、缓冲内 undo 回 requested 清两列、过了缓冲读成 sent 且 undo 拒绝、之后余额够可再申请。脚本 `deploy/referral-payout.sh` 用 shellcheck 检查，在 staging 上手跑（§3）。

`go test .`：两条路由已注册、未配置答 503 / 401；webhook 不占 `/v1` 的 IP 桶、自己 600 / min 后 429。`go test ./referral/`：`/me` 有 / 无 / 失败三种、共享桶。

SQL（按线上顺序 013 → 014 → 015，015 跑两遍）在真库上的测试（含 transaction_id 去重、退款先到、新表 RLS）：`REWARDS_PG_TEST_URL=postgres://…/scratch go test ./rewards/ -run PG -v`
（临时 schema，跑完即删；**绝不要指向 staging / prod**）。**本 PR 写的时候本机没有 Postgres，这组还没实跑过。**

## 5. 待确认

- `claimed` 状态的朋友付费也算（§2.1）是对文档「已兑换」的实现解释，文档没写到这个竞态。
- `is_family_share` 不算是按「真扣费」推的，文档字段表里没有这个字段。
- `payout_in_progress` 先于 `below_threshold` 判断；文档两条 409 没定先后。
- W-9 提醒按「本年已发 + 申请中（含这一张）≥ $500」算，比「之前已到 $500 的下一次」早一张提醒。
- 乱序：退款先于购买到达 —— 已处理（§2.1，`reward_voided_txns`，产品已定：按已作废、答 200）。
- 只认 `app_user_id`，**不查** `aliases` / `original_app_user_id`：朋友若用匿名 id 付了费、之后才被 RevenueCat 合并到 Supabase id，
  那笔事件的 `app_user_id` 可能还是 `$RCAnonymousID:…` → not_referee、不记。填码必须登录（JWT），之后的扣费一般就是 Supabase id，
  影响面小；要补就在判 not_referee 前把 aliases 里的 uuid 也查一遍（注意别让一个事件命中两条推荐）。本 PR 不改。
- `takehome_percentage` 的增值税问题：RevenueCat 对它的说明是扣完商店佣金、**未扣 VAT / 数字服务税**的比例，`price` 本身也含税
  → 欧盟等含税地区按本公式算出的 30% 比苹果实际到账的 30% 偏高（大约高出该地税率那么多）。文档公式就是 price × takehome × 0.30，
  本 PR 照文档不改；要按到手算需产品定口径（可考虑事件里的税率字段，是否每条都带未核实）。
- 朋友删账号后推荐人已记的钱保留（文档只写了推荐人删账号）。
