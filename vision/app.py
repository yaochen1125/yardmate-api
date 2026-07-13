"""L1 视觉 kNN 微服务(落 yardmate-api/vision/, 同机 localhost, Go 经 HTTP 调)。
POST /v1/vision/identify (multipart image) -> top-k catalog 候选 + 库内外置信。
GET /health。env: VISION_INDEX_DIR(索引目录) /
VISION_IN_OUT_THRESHOLD(可选, 覆盖 meta 阈值, staging 标定免重建) /
VISION_MAX_CONCURRENT_EMBED(可选, 默认 1, 限并发嵌入护 CPU/内存)。
"""
import os, sys, json, asyncio
from contextlib import asynccontextmanager
import numpy as np, hnswlib
from fastapi import FastAPI, UploadFile, File, HTTPException
sys.path.insert(0, os.path.dirname(__file__))
import vision_embed

INDEX_DIR = os.environ.get("VISION_INDEX_DIR", "")
_THRESHOLD_ENV = os.environ.get("VISION_IN_OUT_THRESHOLD", "")           # M4: 运行时可调
_MAX_CONCURRENT = max(1, int(os.environ.get("VISION_MAX_CONCURRENT_EMBED", "1")))  # H3
S = {}


@asynccontextmanager
async def lifespan(_app: FastAPI):
    # M5: 启动尽力加载索引 + 预热模型;缺失/损坏 → 记 load_error、服务仍存活
    # (/health loaded:false、identify 返 503 可重试),而不是抛异常 crash-loop
    # (serve 与 build 无 systemd 顺序依赖;索引建好后重启 serve 加载即可)。
    try:
        vision_embed._ensure()  # 预热模型(释放文本塔);失败即降级
        meta = json.load(open(f"{INDEX_DIR}/meta.json"))
        mapping = json.load(open(f"{INDEX_DIR}/mapping.json"))
        idx = hnswlib.Index(space="cosine", dim=meta["dim"])
        idx.load_index(f"{INDEX_DIR}/index.bin")
        idx.set_ef(64)
        thr = float(_THRESHOLD_ENV) if _THRESHOLD_ENV else float(meta.get("in_out_threshold", 0.8))
        # S.update 放最后:全部就绪才算 loaded,任一步失败都留 "idx" 不在 S(降级)。
        S.update(meta=meta, mapping=mapping, idx=idx, threshold=thr,
                 sem=asyncio.Semaphore(_MAX_CONCURRENT))
        print(f"loaded index: {meta['count']} vecs, {len(meta['catalog_ids'])} ids, thr={thr}, maxconc={_MAX_CONCURRENT}")
    except Exception as e:
        S["load_error"] = str(e)
        print(f"index load FAILED (serving degraded, identify->503): {e}")
    yield


app = FastAPI(title="YardMate L1 Vision kNN", lifespan=lifespan)


@app.get("/health")
def health():
    return {"ok": True, "loaded": "idx" in S,
            "count": S.get("meta", {}).get("count"), "error": S.get("load_error")}


def _embed_and_query(data: bytes, k: int):
    # 阻塞 CPU 活(embed + kNN)——由 identify 经 asyncio.to_thread 调,不占事件循环(H3)。
    v = vision_embed.embed_bytes(data).astype(np.float32)
    dim = S["meta"]["dim"]
    if v.shape[0] != dim:  # M8: 维度漂移(模型/索引不匹配)→ 明确报错, 不裸 500
        raise ValueError(f"embed dim {v.shape[0]} != index dim {dim}")
    return S["idx"].knn_query(v, k=k)


@app.post("/v1/vision/identify")
async def identify(image: UploadFile = File(...), k: int = 20, top: int = 5):
    if "idx" not in S:  # M5: 索引没加载 → 503(可重试),不是 500
        raise HTTPException(503, f"index not loaded: {S.get('load_error', 'building?')}")
    data = await image.read()
    k = max(1, min(k, S["meta"]["count"]))  # L8: k∈[1,count]
    top = max(1, top)
    # H3: 有界并发 + 线程池跑阻塞活,不阻塞事件循环(/health 不被饿死)、不并发爆内存。M8: query 也在 try 内。
    async with S["sem"]:
        try:
            labels, dists = await asyncio.to_thread(_embed_and_query, data, k)
        except ValueError as e:
            raise HTTPException(400, str(e))
        except Exception as e:
            raise HTTPException(400, f"vision failed: {e}")
    best = {}
    for lab, d in zip(labels[0], dists[0]):
        cid = S["mapping"][int(lab)]["catalog_id"]
        sim = 1.0 - float(d)
        if cid not in best or sim > best[cid]:
            best[cid] = sim
    ranked = sorted(best.items(), key=lambda x: -x[1])[:top]
    top_sim = ranked[0][1] if ranked else 0.0
    thr = S["threshold"]
    denom = max(1e-6, 1.0 - thr)  # L7: 防 thr==1.0 除零
    return {
        "candidates": [{"catalog_id": c, "vision_sim": round(s, 4)} for c, s in ranked],
        "nn_sim": round(top_sim, 4),
        "in_catalog": top_sim >= thr,
        "in_catalog_confidence": round(min(1.0, max(0.0, (top_sim - thr) / denom * 0.5 + 0.5)), 3),
        "model": S["meta"]["model"],
    }
