# 零停机 KNN 飞轮 SPEC（晋升 → 自动 KNN 覆盖）

目标：晋升新植物后，KNN 索引在**几分钟内**自动覆盖该株，**全程零停机**（identify 不中断、api 不重启）。
不是 MVP，是完整版。解决三痛点：①晋升后 KNN 不自动覆盖（要手动全库重跑）②每次更新停机几分钟（用户不接受）③要自动化。

## 现状拓扑（2026-07-16 实测）

- **晋升工具**在 admin/本地：`~/Documents/Yardmate_all/catalog-promote-tool`（`promote.py` 写 R2 + 本地 content）。
- **KNN 索引 + 服务**在服务器 `root@5.78.183.252`：
  - `/root/yardmate-vision/` 代码（app.py serve + build_index/fold_external/pull_* + venv）
  - `/root/yardmate-vision-index-inat` = INAT_DIR（纯 iNat 真照索引）
  - `/root/yardmate-vision-index` = **SERVE_DIR**（fold 后的服务索引，count=37295，覆盖到 AAA1634）
  - `/root/yardmate-vision-ref`、`-supp` = iNat / Phase B 库外图源
  - systemd：`yardmate-vision.service`（active，uvicorn `app:app` **绑 127.0.0.1:8099**，VISION_INDEX_DIR=SERVE_DIR，MemoryMax=2600M，模型常驻 ~1.6G）；`yardmate-vision-build.service`（inactive，跑 build_pipeline.sh 全库重建）
- **一个 vision 服务同时供 staging + prod 两个 yardmate-api**（都走 localhost:8099）。没有独立 staging vision。
- box：7.6G RAM（~5.4G free），rclone `r2:` 已配。
- **prod KNN 已开**（VISION_KNN_ENABLED=true），KNN 做辅助（印证 PlantNet + 温和 boost，raise-only，从不改返回的植物，fail-open）。

## 铁律（不可破）

1. **主图 Pencil 生成图绝不进判别索引**（P0 域差铁律：生成图 vs 真照余弦差 ~0.18 >> 近种形态差 ~0.04）。增量只从 R2 `{id}/external/` 拉真照。
2. **零停机**：identify 对 vision 完全 fail-open（vision_knn.go 6s 硬顶 + handlers.go 1.5s 等待预算，超时 SKIPPED，raise-only boost，从不改返回植物）。即使普通 `systemctl restart` 对 identify 也零停机。热重载是优化非必需，但要做（完整版）。
3. **原子写**：三文件（index.bin / mapping.json / meta.json）都 tmp + os.replace；meta 最后写（幂等约定：无 meta = 半成品）。

## 三块工程 + 设计决策

### Piece 2 — 增量建索引（命门，关键路径）

**核心简化（关键洞察）**：新株晋升完成时，R2 里只有它的 `external/` **真照**（imageingest 按学名匹配、promote 已验证），**没有 iNat 参考**（iNat 靠全库 pull_reference，增量不跑）。所以增量 add = fold_external 的 **「0-iNat 可信 external」** 情形：

- fold 的两大重过滤（**对增量都不适用**，安全）：
  - **CULTIVAR_MISLABEL**（删误标母种向量）：只在 pull_reference 误拉了母种 iNat 图时才需要；增量从不拉 iNat，无母种图可误标 → 无风险。
  - **NN_FLOOR 相对 NN 过滤**：防欠覆盖株的 external 里混入非本株/非植物图；增量的 external 是该株自己的已验证真照，无歧义 → 不需要。
- 增量**必须保留**的安全：
  - **只 external/**（绝不碰 `{id}/{slot}.png` 生成主图）。
  - **DUP_SIM 自去近重复**（同株 external 内部）。
  - 可信源 = promote 的 `fetch_supplementary`（imageingest 学名匹配 + web 审核已验证）。
- **catalog 门去掉**：fold 的 catalog 门是全库重建时剔 R2 残留已删 id；增量是 **targeted 单株**（cid 由可信 promote 钩子显式给），不需要、也不能用服务器上**过时的** plants_index.json（那份在 admin 机才刚更新）。增量只加这一个 cid 的 external，源真在于「刚晋升」这一事实。

**幂等**：
- cid 不在索引 → `resize_index(count+n)` + `add_items`（快路径 <1s）。
- cid 已在索引（re-trigger/重试）→ drop 该 cid 现有向量 + 重建（保留其余）→ 刷新（~秒级到几十秒，罕见事件可接受）。

**并发/锁**：增量写 SERVE_DIR 与全库 build 的 fold 写 SERVE_DIR 可能撞。用 **flock `{SERVE_DIR}/.write.lock`**：增量核心写时持锁；fold_external 最终写也持同锁。全库 build 罕见，且即使增量基于稍旧 base 被 fold 覆盖也不丢东西（fold 也从 R2 external 复现该株，最终一致）。原子写保证任何时刻无半截索引。

**实现**：`incremental.py`（纯磁盘核心 + 注入 embed_fn，无 torch 依赖）+ `incremental_add.py`（standalone CLI，自带模型，服务器可手测）。**同一核心**避免漂移；serve 的 admin 端点也调这个核心但注入**已加载的模型**（省第二次 torch load）。

### Piece 3 — vision 热重载

`app.py` 加 `POST /admin/knn/reload`：从 SERVE_DIR 重读 meta/mapping/index.bin 构新对象，**原子 rebind 全局 `S`**（不重载模型 BioCLIP-2）。
**并发正确性（必须）**：identify 当前多次读全局 `S["idx"]/S["mapping"]/...`，reload 中途 rebind 会 idx/mapping 错配。修法：identify **开头快照 `st = S`**（GIL 下单次全局读原子），全程用 `st`；reload 构好新 dict 再 `S = newdict`（原子 rebind）；in-flight 请求持旧快照一致。sem 与 reload 锁做**持久模块级**（不随 swap 重建），保证并发上限跨 reload 生效、reload 之间串行。

### Piece 1 — 晋升钩子 + 跨机触发

- `promote.py._promote_plant` 末尾（`plan["applied"]=True` 后，promote.py:219）加 `_trigger_knn_build(pid, sci)`，仿 `_refresh_distribution`/`_refresh_shards` 的 **best-effort fire、warn 不 block**。
- **跨机认证 = SSH（复用现成）**：admin 机已有 root 免密 SSH 到服务器（7788 quota 面板已在用）。触发 = `ssh {host} 'curl -sS -m N -X POST -H "X-Vision-Admin-Token: T" http://127.0.0.1:8099/admin/knn/add -d cid'`。**vision 端点保持 localhost-only**（不新开公网口 = 最小攻击面）；SSH key 是认证边界，token 是纵深防御。
- **默认 off / 保守**：
  - vision admin 端点由 env `VISION_ADMIN_TOKEN` 门控：**未设 = 端点 404（功能关）**；设了才启用并校验 token。
  - promote 钩子由 `cfg["knn_flywheel"]`（`enabled`/`ssh_host`/`token`/`endpoint`）门控：未配置/`enabled:false` → skip（返回 note，不 block 晋升）。
- 端点 `POST /admin/knn/add {catalog_id}`：in-process 走 incremental 核心（注入已加载模型，to_thread + sem）→ 原子写 SERVE_DIR → in-process reload（rebind S）。返回 `{added, count}`。

## 端到端流

晋升 apply → `_trigger_knn_build(pid)` → ssh + curl localhost:8099/admin/knn/add → 服务器 in-process：拉 `{pid}/external/*.png` → embed（复用模型）→ flock + resize/add_items（或 drop+rebuild）→ 原子写 SERVE_DIR → rebind S → 该株即刻可被 KNN 认出。全程 identify 不停、api 不重启。下次全库 build 经 fold_external 0-iNat 路径复现同结果（无分叉）。

## 部署 / 验证纪律

- vision 代码部署 = **rsync `vision/` → `/root/yardmate-vision/` + restart 服务**（不走 deploy.sh，那是 Go API 的）。没有 staging/prod vision 之分（一个服务）。
- **验证顺序**：① standalone `incremental_add.py` 对 SERVE_DIR 的**副本**跑，validate 输出正确（向量数 +n、mapping/meta 一致、identify 该株命中）②再上线 admin 端点、对真株/测试株走一遍、确认 identify 命中且其余未坏（fail-open + raise-only + 原子兜底）。
- 每仓库独立 PR（yardmate-api = vision 三块；catalog-promote-tool = 钩子）→ review skill + Codex → 修 → merge → prod。
- 完成后更新记忆 `l1_vision_knn.md` 记飞轮上线状态。
