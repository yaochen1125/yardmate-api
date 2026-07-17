"""L1 视觉 kNN 微服务(落 yardmate-api/vision/, 同机 localhost, Go 经 HTTP 调)。
POST /v1/vision/identify (multipart image) -> top-k catalog 候选 + 库内外置信。
GET /health。env: VISION_INDEX_DIR(索引目录) /
VISION_IN_OUT_THRESHOLD(可选, 覆盖 meta 阈值, staging 标定免重建) /
VISION_MAX_CONCURRENT_EMBED(可选, 默认 1, 限并发嵌入护 CPU/内存)。

飞轮(KNN_FLYWHEEL_SPEC.md)admin 端点(**默认关**, 需 env VISION_ADMIN_TOKEN):
  POST /admin/knn/add?catalog_id=AAA#### -> 拉该株 external 真照增量 add + 热重载(零停机)。
  POST /admin/knn/reload -> 从盘重读索引热重载(全库 build/standalone 改盘后免 restart 生效)。
env: VISION_ADMIN_TOKEN(未设=admin 端点 404 关) / VISION_EXTERNAL_CDN(external 图前缀)。
"""
import os, sys, re, json, hmac, asyncio
from contextlib import asynccontextmanager
import numpy as np, hnswlib
from fastapi import FastAPI, UploadFile, File, HTTPException, Header
sys.path.insert(0, os.path.dirname(__file__))
import vision_embed
from incremental import add_plant_to_index, _write_lock

INDEX_DIR = os.environ.get("VISION_INDEX_DIR", "")
_THRESHOLD_ENV = os.environ.get("VISION_IN_OUT_THRESHOLD", "")           # M4: 运行时可调
_MAX_CONCURRENT = max(1, int(os.environ.get("VISION_MAX_CONCURRENT_EMBED", "1")))  # H3
_ADMIN_TOKEN = os.environ.get("VISION_ADMIN_TOKEN", "")                  # 未设=admin 端点关(404)
_CDN = os.environ.get("VISION_EXTERNAL_CDN", "https://images.yardmate.ai/plant_images")
# build/fold 用的 catalog 快照(做 catalog 门)。增量 add 后把新 id 记进来, 防后续全库 build 的 fold
# 用**过时** plants_index(该 id 未同步进来)把它当已删 id drop 掉(Codex #116)。默认 = 与 app 同目录。
_CATALOG_PATH = os.environ.get(
    "VISION_CATALOG_PATH", os.path.join(os.path.dirname(os.path.abspath(__file__)), "plants_index.json"))
_CID_RE = re.compile(r"^[A-Z]{3}\d{4,}$")  # cid 进 curl URL, 严格校验防注入

# S = 可原子 swap 的索引快照 {meta, mapping, idx, threshold}; reload/add 换新 dict 后 rebind。
# 读者(identify/health)开头 `st = S` 快照一次(GIL 下单读原子), 全程用 st → reload 中途 rebind
# 不会让 in-flight 请求看到 idx/mapping 错配。sem/锁是**持久**对象(不随 swap 重建)。
S = {}
_SEM: asyncio.Semaphore = None    # 持久: 并发嵌入上限跨 reload 生效(lifespan 内建, 需事件循环)
_WRITE_LOCK: asyncio.Lock = None  # 持久: 串行化 add/reload 的 in-process 写盘 + rebind


def _load_snapshot():
    """从 INDEX_DIR 读一份新索引快照(不含 sem)。阻塞 I/O —— 由 lifespan/端点经 to_thread 调。
    ★三文件读在 {INDEX_DIR}/.write.lock 内 —— 与增量 add / 全库 build 的 fold 写(都持同锁)互斥,
    否则 fold 的三次 os.replace 若插在这里的三次读之间, 会读到 meta(旧)+mapping(旧)+index(新) 错配
    → identify 里 st['mapping'][label] 越界(见 KNN_FLYWHEEL_SPEC.md「并发/锁」)。"""
    with _write_lock(INDEX_DIR):
        meta = json.load(open(f"{INDEX_DIR}/meta.json"))
        mapping = json.load(open(f"{INDEX_DIR}/mapping.json"))
        idx = hnswlib.Index(space="cosine", dim=meta["dim"])
        idx.load_index(f"{INDEX_DIR}/index.bin")
    idx.set_ef(64)
    thr = float(_THRESHOLD_ENV) if _THRESHOLD_ENV else float(meta.get("in_out_threshold", 0.8))
    return {"meta": meta, "mapping": mapping, "idx": idx, "threshold": thr}


@asynccontextmanager
async def lifespan(_app: FastAPI):
    # M5: 启动尽力加载索引 + 预热模型;缺失/损坏 → 记 load_error、服务仍存活
    # (/health loaded:false、identify 返 503 可重试),而不是抛异常 crash-loop。
    global S, _SEM, _WRITE_LOCK
    _SEM = asyncio.Semaphore(_MAX_CONCURRENT)
    _WRITE_LOCK = asyncio.Lock()
    try:
        vision_embed._ensure()  # 预热模型(释放文本塔);失败即降级
        S = _load_snapshot()    # 全部就绪才 rebind → 任一步失败留 load_error(降级)
        print(f"loaded index: {S['meta']['count']} vecs, {len(S['meta']['catalog_ids'])} ids, "
              f"thr={S['threshold']}, maxconc={_MAX_CONCURRENT}, admin={'on' if _ADMIN_TOKEN else 'off'}")
    except Exception as e:
        S = {"load_error": str(e)}
        print(f"index load FAILED (serving degraded, identify->503): {e}")
    yield


app = FastAPI(title="YardMate L1 Vision kNN", lifespan=lifespan)


@app.get("/health")
def health():
    st = S
    return {"ok": True, "loaded": "idx" in st,
            "count": st.get("meta", {}).get("count"), "error": st.get("load_error")}


def _embed_and_query(st: dict, data: bytes, k: int):
    # 阻塞 CPU 活(embed + kNN)——由 identify 经 asyncio.to_thread 调,不占事件循环(H3)。
    # 收 st 快照(非全局 S): reload 期间也用一致的 idx/meta。
    v = vision_embed.embed_bytes(data).astype(np.float32)
    dim = st["meta"]["dim"]
    if v.shape[0] != dim:  # M8: 维度漂移(模型/索引不匹配)→ 明确报错, 不裸 500
        raise ValueError(f"embed dim {v.shape[0]} != index dim {dim}")
    return st["idx"].knn_query(v, k=k)


@app.post("/v1/vision/identify")
async def identify(image: UploadFile = File(...), k: int = 20, top: int = 5):
    st = S  # 快照一次: 全程一致, reload rebind 只影响后续请求
    if "idx" not in st:  # M5: 索引没加载 → 503(可重试),不是 500
        raise HTTPException(503, f"index not loaded: {st.get('load_error', 'building?')}")
    data = await image.read()
    k = max(1, min(k, st["meta"]["count"]))  # L8: k∈[1,count]
    top = max(1, top)
    # H3: 有界并发 + 线程池跑阻塞活,不阻塞事件循环、不并发爆内存。M8: query 也在 try 内。
    async with _SEM:
        try:
            labels, dists = await asyncio.to_thread(_embed_and_query, st, data, k)
        except ValueError as e:
            raise HTTPException(400, str(e))
        except Exception as e:
            raise HTTPException(400, f"vision failed: {e}")
    best = {}
    for lab, d in zip(labels[0], dists[0]):
        cid = st["mapping"][int(lab)]["catalog_id"]
        sim = 1.0 - float(d)
        if cid not in best or sim > best[cid]:
            best[cid] = sim
    ranked = sorted(best.items(), key=lambda x: -x[1])[:top]
    top_sim = ranked[0][1] if ranked else 0.0
    thr = st["threshold"]
    denom = max(1e-6, 1.0 - thr)  # L7: 防 thr==1.0 除零
    return {
        "candidates": [{"catalog_id": c, "vision_sim": round(s, 4)} for c, s in ranked],
        "nn_sim": round(top_sim, 4),
        "in_catalog": top_sim >= thr,
        "in_catalog_confidence": round(min(1.0, max(0.0, (top_sim - thr) / denom * 0.5 + 0.5)), 3),
        "model": st["meta"]["model"],
    }


# ---------- 飞轮 admin 端点(默认关: 未配 VISION_ADMIN_TOKEN → 404) ----------

def _require_admin(token: str):
    if not _ADMIN_TOKEN:
        raise HTTPException(404, "not found")  # 功能关(未配 token)—— 不泄露端点存在
    if not token or not hmac.compare_digest(token, _ADMIN_TOKEN):  # 常量时间比较
        raise HTTPException(403, "forbidden")


async def _reload_locked():
    """在 _WRITE_LOCK 内从盘重读并原子 rebind S。调用方须已持 _WRITE_LOCK。"""
    global S
    snap = await asyncio.to_thread(_load_snapshot)
    S = snap  # 原子 rebind(GIL)
    return snap["meta"]["count"]


@app.post("/admin/knn/reload")
async def admin_reload(x_vision_admin_token: str = Header(None)):
    """从盘热重载索引(全库 build / standalone incremental_add 改盘后免 restart 生效)。"""
    _require_admin(x_vision_admin_token)
    async with _WRITE_LOCK:
        count = await _reload_locked()
    return {"reloaded": True, "count": count}


@app.post("/admin/knn/add")
async def admin_add(catalog_id: str, scientific_name: str = "", x_vision_admin_token: str = Header(None)):
    """增量: 拉 catalog_id 的 external 真照 add 进索引 + 热重载(零停机, 复用已加载模型)。
    幂等(re-trigger 刷新)。写盘走 incremental.add_plant_to_index(flock 与全库 build 互斥)。
    scientific_name(可选): 记进 build catalog 快照防陈旧 fold drop(_pin_catalog_id)。"""
    _require_admin(x_vision_admin_token)
    if not _CID_RE.match(catalog_id or ""):
        raise HTTPException(400, "bad catalog_id (want ^[A-Z]{3}\\d{4,}$)")
    async with _WRITE_LOCK:            # 串行化: 同时只一个 add/reload 写 SERVE_DIR + rebind
        # 不占 identify 的 _SEM: 下载 external(curl 可慢/超时到 25s×N)期间不该卡住 identify 嵌入
        # (否则 Go 侧等 vision 超时, Codex #116)。并发上界仍受控: _WRITE_LOCK 串行 add(≤1) +
        # identify _SEM=1(≤1) → 至多 2 个并发嵌入; 模型只读前向线程安全、激活内存有界(<MemoryMax)。
        # catalog_path/sci: 核心在同一 flock 内写索引后 pin 进 build catalog(原子, 防陈旧 fold 抹掉)。
        stats = await asyncio.to_thread(
            add_plant_to_index, INDEX_DIR, catalog_id, _CDN, vision_embed.embed_path,
            catalog_path=_CATALOG_PATH, sci=scientific_name)
        if stats.get("added", 0) > 0:
            count = await _reload_locked()
            return {**stats, "reloaded": True, "count": count}
    # added==0(external 缺/CDN 未同步/全部嵌入失败): 该株**没进 KNN**。返 422(非 2xx)让触发端
    # curl --fail-with-body 非零退出 → 晋升钩子 warn + operator 知道要重试(如等 CDN 传播), 不误报成功(Codex #116)。
    raise HTTPException(422, detail={**stats, "reloaded": False})
