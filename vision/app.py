"""L1 视觉 kNN 微服务(未来落 yardmate-api/vision/, 同机 localhost, Go 经 HTTP 调)。
POST /v1/vision/identify (multipart image) -> top-k catalog 候选 + 库内外置信。
GET /health。
env: VISION_INDEX_DIR (索引目录)。
"""
import os, sys, json
import numpy as np, hnswlib
from fastapi import FastAPI, UploadFile, File, HTTPException
sys.path.insert(0, os.path.dirname(__file__))
import vision_embed

INDEX_DIR = os.environ.get("VISION_INDEX_DIR", "")
app = FastAPI(title="YardMate L1 Vision kNN")
S = {}

@app.on_event("startup")
def _load():
    meta = json.load(open(f"{INDEX_DIR}/meta.json"))
    mapping = json.load(open(f"{INDEX_DIR}/mapping.json"))
    idx = hnswlib.Index(space="cosine", dim=meta["dim"])
    idx.load_index(f"{INDEX_DIR}/index.bin")
    idx.set_ef(64)
    S.update(meta=meta, mapping=mapping, idx=idx)
    vision_embed._ensure()  # 预热模型
    print(f"loaded index: {meta['count']} vecs, {len(meta['catalog_ids'])} ids")

@app.get("/health")
def health():
    return {"ok": True, "loaded": bool(S), "count": S.get("meta", {}).get("count")}

@app.post("/v1/vision/identify")
async def identify(image: UploadFile = File(...), k: int = 20, top: int = 5):
    if not S:
        raise HTTPException(503, "index not loaded")
    try:
        v = vision_embed.embed_bytes(await image.read()).astype(np.float32)
    except Exception as e:
        raise HTTPException(400, f"embed failed: {e}")
    labels, dists = S["idx"].knn_query(v, k=min(k, S["meta"]["count"]))
    # cosine sim = 1 - dist; 按 catalog_id 聚合取最大 sim
    best = {}
    for lab, d in zip(labels[0], dists[0]):
        cid = S["mapping"][int(lab)]["catalog_id"]; sim = 1.0 - float(d)
        if cid not in best or sim > best[cid]:
            best[cid] = sim
    ranked = sorted(best.items(), key=lambda x: -x[1])[:top]
    top_sim = ranked[0][1] if ranked else 0.0
    thr = S["meta"]["in_out_threshold"]
    return {
        "candidates": [{"catalog_id": c, "vision_sim": round(s, 4)} for c, s in ranked],
        "nn_sim": round(top_sim, 4),
        "in_catalog": top_sim >= thr,
        "in_catalog_confidence": round(min(1.0, max(0.0, (top_sim - thr) / (1 - thr) * 0.5 + 0.5)), 3),
        "model": S["meta"]["model"],
    }
