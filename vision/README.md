# L1 Vision kNN 微服务

目录原生**参考图向量库**:给用户照片做视觉最近邻检索,输出"目录原生"相似度 + 库内外置信,
作为识别级联的新融合信号(和 PlantNet/Plant.id/GPT 并存)。同机 localhost, Go 经 HTTP 调。

模型 **BioCLIP-2**(P0 选定:种级判别 89% / 库内外 AUC 0.995 / 通用 CLIP 仅 66%)。
检索 **hnswlib** 余弦。

## ★核心不变量(P0 硬数据得出, 违反即坏)
1. **判别索引只放真实照片**(iNat/GBIF)。AI 生成图与真实照片画风差(余弦 ~0.18)远大于近种形态差(~0.04),
   混入会让最近邻按"画风"而非 botany 判 → 生成图**不进索引**。
2. 同一候选集**绝不混真实+生成参考**。
3. cultivar(螺旋灯心草/玫瑰名株)iNat 无独立分类单元 → 拿不到真实参考,L1 不自动解决,靠用户飞轮补。

## 组成
- `vision_embed.py` — BioCLIP-2 嵌入单例(建库/推理共用)
- `pull_reference.py` — 全 catalog 从 iNat 拉真实照片(resumable/限流/记零覆盖)
- `build_index.py` — 真实照片 → hnswlib 余弦索引(index.bin + mapping.json + meta.json)
- `app.py` — FastAPI: `POST /v1/vision/identify` + `GET /health`
- `yardmate-vision.service` — systemd(CPU, 限内存 2.6G)

## HTTP 契约
`POST /v1/vision/identify` (multipart `image`, 可选 `k`/`top`) →
```json
{ "candidates": [{"catalog_id": "AAA0262", "vision_sim": 0.85}, ...],
  "nn_sim": 0.85, "in_catalog": true, "in_catalog_confidence": 0.61,
  "model": "hf-hub:imageomics/bioclip-2" }
```
Go 侧 fail-open:超时/不可达/kill-switch off → 视作无信号,不影响主级联。

## 部署(server 5.78.183.252, 先 staging)
```bash
cd /root/yardmate-api/vision
python3.11 -m venv venv
venv/bin/pip install -r requirements.txt   # CPU: --index-url https://download.pytorch.org/whl/cpu 装 torch
# 1) 建库(4-8h, 一次性; 用当前权威 plants_index)
venv/bin/python pull_reference.py /path/to/plants_index.json /root/yardmate-vision-ref 40
venv/bin/python build_index.py /root/yardmate-vision-ref /root/yardmate-vision-index
# 2) 起服务
cp yardmate-vision.service /etc/systemd/system/ && systemctl enable --now yardmate-vision
curl -s localhost:8099/health
```
Go 侧开关 `VISION_KNN_ENABLED`(vault, 默认 false)。先 staging 实拍验证 → prod 灰度。

## 性能注意(server 4vCPU/8GB)
BioCLIP-2 ViT-L CPU 单图 ~1-2s、常驻 ~2G。识别级联本就并行调外部引擎 → vision 并发跑,延迟大概率被并行吸收。
若过重:P1d 评估 ONNX+INT8,或权衡换 BioCLIP-v1(ViT-B)。上线前在 server 实测延迟/内存。

## 飞轮(P2)
iOS 拍照历史(userID 隔离)accept/correct → 真实照片异步回流 → 追加进对应 catalog,越用越准 + 逐步补 cultivar 真实参考。
