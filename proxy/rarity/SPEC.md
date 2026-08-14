# proxy/rarity — Plantdex 稀有度聚合与发布

服务端把「库内物种识别成功次数」聚合成静态 CDN 文件 `dex/rarity.json`，供 iOS
Plantdex（图鉴）只读消费。消费方契约真源：
`yardmate-swiftui/docs/releases/v1/main-navigation/home/plantdex/plantdex.md`
§数据契约 §rarity.json。

## 1. 数据流

```
/v1/identify 成功（top suggestion 命中库内 AAA id，排除 AAA0000 哨兵）
  → best-effort detached goroutine（proxy/dex.go recordIdentifyScan）
  → dex_identify_daily (plant_id, day, count) 日桶 +1     [migration 012]
  → Publisher 周期聚合（默认 24h，进程内 ticker，sweep 同款双层 recover）
  → SUM(count) per plant_id → 分位数 tier + oneIn → Manifest JSON
  → R2 Put {RARITY_R2_PREFIX}/dex/rarity.json（public, max-age=60）
  → iOS 冷启动直拉（flags.json 模式，见 §4）
```

计数是 **best-effort**：goroutine 内 `context.Background()` + 5s 超时 +
panic recover，任何失败只 log，识别响应永不受影响、永不变慢。

## 2. 表（migration 012_dex_identify_daily.sql）

`dex_identify_daily(plant_id text, day date, count bigint, PK(plant_id, day))`，
UTC 日桶，`ON CONFLICT DO UPDATE count+1`。**匿名 by design**：无 device_id /
user id → 无同意门控问题、无删号义务（DeleteUserRows 刻意不碰）。

与 `catalog_signals`（010）刻意分表：那张是设备去重的 engagement 信号
（客户端上报、受 analytics 同意门控、同设备重复识别不计），喂 catalog admin
工具；本表是服务端权威的 **scan 次数**（契约口径 `1 in {n} scans`，重复也计）。
两者并存，语义互不污染。2026-08-13 拍板：新表方案（vs 扩展/复用
catalog_signals）；不用 catalog_signals 做冷启动种子（单位有偏，宁可自然冷启动）。

## 3. 聚合算法（rarity.go，纯函数可单测）

- `totalScans` = Σ 全部物种计数（**含**低于阈值的物种——oneIn 的分母必须是真实
  总量），AAA0000 与非正数按垃圾丢弃。
- 物种入选：count ≥ `MinSample`（默认 50）。入选数 < `MinSpecies`（默认 100）→
  species map 发空 {}：冷启动早期只有头部（必然常见）物种达标，对这种「只有
  头部」的总体做分位数会把其中最不常见的错标成 legendary——**宁可隐藏不可错标**
  （客户端对缺席物种本来就隐藏稀有度，这是契约设计好的降级）。
- tier：入选物种按 count 降序（同 count 按 id 升序，保证输出确定性），物种的
  percentile = 其**同分 run 起点**下标 / 入选总数 → 按 `TierCuts`
  （默认 0.40/0.70/0.85/0.95）切五档。同分必同档；run 跨界整体归**更常见**档
  （保守，不虚标稀有）。
- `oneIn` = round(totalScans / count)，下限 1。
- `version`：发布前读 R2 现值 +1；对象不存在 → 1；现值损坏 → WARN + 1
  （自愈优先于单调性）；R2 读失败 → 本轮中止（宁可跳过一轮也不回退版本号）。
- `generatedAt`：RFC3339 UTC。

Wire 形状（字段名/档名是契约常量，勿改）：

```json
{ "version": 3, "generatedAt": "2026-08-13T00:00:00Z", "totalScans": 1840000,
  "species": { "AAA0876": { "tier": "rare", "oneIn": 2400 } } }
```

空 species 必须序列化为 `{}`（非 null）。五档：`common` / `uncommon` / `rare`
/ `very-rare` / `legendary`。

## 4. 发布闸门决策（2026-08-13 拍板：flags.json 模式）

rarity.json **不进 version.txt 闸门**、发布时**不 bump version.txt**：

- version.txt 的唯一写者是 `catalog-promote-tool/deploy_shards.sh`（先传文件
  后翻版本的 commit-point 语义）。第二个独立写者会与它竞态——可能把别人半份
  发布提前 commit。
- 一次 bump = 全体客户端强制重拉整个内容集（~9MB+）。稀有度日更 ≠ 内容日更，
  为一个 ~70KB 文件天天强刷不成立。
- 先例：`flags.json` 与 version.txt 同级、不进闸门、每次冷启动直拉，理由
  （「改动不 bump 内容版本」）与本文件完全同构。

短新鲜度靠对象自身 `Cache-Control: public, max-age=60`（发布脚本对 content/
平铺文件的同款值），日更粒度下无需 CF purge。**iOS 侧落地**（独立 PR）：照
RemoteFlags 模式冷启动拉 `{content-base}/dex/rarity.json`（ETag 304 时几乎
免费），勿加进 ContentResource.refreshSet；plantdex.md 契约中「走 RemoteContent
version 闸门」一行需对应更新。

注意 `yardmate-content/publish.sh` 的白名单不含 `dex/`（各脚本都是 `rclone
copy` 不删除多余对象），本文件唯一写者是本服务——保持如此。

## 5. 配置（全部走 vault / secrets.env，Codex #23）

| Key | 默认 | 说明 |
|---|---|---|
| `RARITY_COUNT_ENABLED` | `false` | 识别成功计数写入总闸（先跑 012 SQL 再开） |
| `RARITY_PUBLISH_ENABLED` | `false` | 聚合发布总闸（复用 imageingest 的 R2_* 凭据） |
| `RARITY_R2_PREFIX` | `content` | staging **必须**设 `content-staging`（桶共享，前缀是唯一隔离） |
| `RARITY_MIN_SAMPLE` | 50 | 物种入选的最小 scan 数 |
| `RARITY_MIN_SPECIES` | 100 | 发布 tier 的最少入选物种数（低于则发空 species） |
| `RARITY_TIER_CUTS` | `0.40,0.70,0.85,0.95` | 五档分位切点（升序、(0,1)；坏值 WARN+默认） |
| `RARITY_PUBLISH_INTERVAL` | `24h` | 发布节奏（下限 10m） |
| `RARITY_ADMIN_TOKEN` | — | `/internal/rarity/rebuild` 门票；未设则路由不注册 |

计数与发布独立开关：预期 prod 先开 COUNT 攒数据，PUBLISH 后开（或同开，靠
MinSpecies 下限自动发空直到样本够）。

## 6. 手动触发（staging 验证用）

`POST /internal/rarity/rebuild`，header `X-Rarity-Admin-Token`（constant-time
比较；空配置 = 全拒）。与 `/internal/imageingest/run` 同姿态：/v1 之外、无公网
限流中间件、nginx 白名单外公网 404——**只在服务器本机 curl**，无需改 nginx。
同步返回 `{status, key, version, generatedAt, totalScans, speciesPublished}`。

## 7. Ops runbook

### 7.1 上线顺序（staging 先行）

1. **SQL**：Supabase Dashboard SQL Editor 跑
   `proxy/enrichment/migrations/012_dex_identify_daily.sql` —— **staging 项目
   （egnmwvyafckanioumrdi）先，prod（bbdvibmoqpupdkoqwndn）后**。幂等可重跑。
2. **staging secrets**（`secrets-staging.env`）：
   `RARITY_COUNT_ENABLED=true`、`RARITY_PUBLISH_ENABLED=true`、
   `RARITY_R2_PREFIX=content-staging`、`RARITY_ADMIN_TOKEN=<新 token>`，
   验证期可加 `RARITY_MIN_SAMPLE=1`、`RARITY_MIN_SPECIES=0`、
   `RARITY_PUBLISH_INTERVAL=10m`。
3. 部署 staging（`deploy-staging.sh`），对 staging（:8081）打几发真实
   /v1/identify，然后服务器本机：
   `curl -X POST -H "X-Rarity-Admin-Token: $TOKEN" http://127.0.0.1:8081/internal/rarity/rebuild`
4. 验收：`curl https://images.yardmate.ai/content-staging/dex/rarity.json` ——
   version/totalScans/species 与打点数一致；再触发一次 rebuild 确认 version+1。
   查 staging 日志 `rarity publish ok` 与 `identify dex-count` 无错。
5. **prod**：跑完 012（prod 项目）后，prod secrets 先只开
   `RARITY_COUNT_ENABLED=true` 攒数据；`RARITY_PUBLISH_ENABLED=true` 可同步开
   （MinSpecies 下限期间发布的是合法空 species 文件）或数据够后再开。阈值用
   默认（50/100），**不要**把 staging 的放宽值带进 prod。

### 7.2 已知边界

- staging 验证时若要对照其它内容文件，记得 `content-staging/` 是死快照，先跑
  staging-runbook.md:84 的 rclone 对齐；rarity.json 本身不受影响（本服务直写）。
- 计数从开关打开当天起算，无历史回填（拍板：不种子）。头部物种最先达标浮现，
  长尾逐步解锁——渐进显现即契约设计的冷启动降级。
- 反滥用依赖 identify 端点既有的 per-device 限流 + spend gate（计数只发生在
  真实识别成功之后，无独立攻击面）。
- 账号删除不涉及本表（无任何用户/设备标识）。
