# L1 参考图向量库 — P0 结论 (2026-07-13)

数据: 10 同属异种组(30 种)+ 2 玫瑰 cultivar 组 + 旗舰灯心草 + 6 库外; 1065 张 iNat 真实照片 + 152 张库内生成图。
模型: clip_generic / BioCLIP-2 / DINOv2-vitb14 / dinov2-reg4 (PlantCLEF HF gated 401, 兜底 reg4)。设备 MPS。

## 判决: L1 可行 ✅ — 但参考图必须域一致(重塑 P1)

### 1. 种级判别强成立 (实验 A, chance≈0.33)
| 模型 | A1 real→real | A2 gen查→real参 | A3 real查→gen参 |
|---|---|---|---|
| clip_generic | 0.655 | 0.574 | 0.544 |
| **BioCLIP-2** | **0.886** | **0.704** | **0.759** |
| DINOv2-vitb14 | 0.844 | 0.704 | 0.671 |
| dinov2-reg4 | 0.863 | 0.722 | 0.664 |

- BioCLIP-2 用真实照片能 89% 正确区分同属近种 (通用 CLIP 仅 66%, chance 33%)。这正是引擎级联最弱的一环 → L1 是真新增信号。
- **选 BioCLIP-2**: 各项最优/并列最优, 生物专精 + 带文本塔(L2 延续)。

### 2. 库内外可分极强 (实验 B)
| 模型 | in_sim均 | out_sim均 | AUC |
|---|---|---|---|
| BioCLIP-2 | 0.896 | 0.699 | **0.995** |
| DINOv2-vitb14 | 0.741 | 0.29 | 0.992 |

- 最近邻余弦天然可读地分库内/库外 → "库外置信度"信号成立, 可给全局融合。
- ⚠️ caveat: 库外样本部分形态奇特(食虫植物), AUC 偏乐观; 更严需 look-alike-but-absent 物种。阈值先验: BioCLIP-2 余弦 ~0.8 附近。

### 3. ★核心发现: 域差压倒形态差 (旗舰诊断)
- corkscrew→spiralis: mixed(真实effusus+生成spiralis) **0/20**; gen-only(都生成) 3/20。
- 真实螺旋照片→真实effusus **0.944**, →任何生成图仅 0.74–0.78; 同株跨域基线 0.774。
- **"真实 vs 生成"域差(~0.16–0.20) ≫ "普通 vs 螺旋"形态差(0.04)。** 混用真实+生成参考 → 最近邻被域轴主导, botany 被淹没。

## P1 设计约束 (由 P0 得出, 反直觉但硬性)
1. **参考向量库只用真实照片(iNat/GBIF), 生成图不进判别索引。** 生成图会把真实 query 按"画风"吸走。用户"iNat 真实优先"直觉对; "混合"需修正为: 生成图仅在某株完全无真实照片时兜底, 且与其被比较对象保持同域。
2. **绝不在同一候选集混真实+生成参考。** 否则 domain 轴主导(旗舰 0/20 实证)。
3. **cultivar 是硬骨头**: 螺旋/玫瑰等 cultivar 在 iNat 无独立分类单元 → 拿不到真实参考。L1 不自动解决 cultivar, 除非: (a) 真实 cultivar 照片来源(iNat cultivated/casual 观测 + 园艺源 + 用户飞轮), 或 (b) L2 微调学形态不学画风。生成的 cultivar 图不能替代真实参考。
4. 种级(绝大多数库内外场景)已 ready: BioCLIP-2 + 真实照片库 + 库外距离阈值。

## P1 落地方向 (据此调整)
- 建库: 全 1633 株从 iNat/GBIF 拉真实研究级照片(每种 30–100), BioCLIP-2 嵌入, hnswlib 索引。生成图不入判别库。
- 服务: 独立 Python 微服务同机 localhost (torch CPU/ONNX), Go 经 HTTP 调, kill-switch `VISION_KNN_ENABLED` 默认 off。
- 融合信号: (a) 种级 top-k 视觉相似分参与重排; (b) 最近邻距离作库内外置信。先低权重, staging 标定后调。
- cultivar: 单独立项, 靠用户飞轮(接受/纠正照片)逐步补真实 cultivar 参考。
- L2 备选: 若要根治 cultivar + 消域差, 在真实+生成混合数据上微调 BioCLIP 学"抗画风"表征。
