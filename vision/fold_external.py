#!/usr/bin/env python3
"""建库第三步(P2b): 把 catalog 详情页的真实 external gallery 图折进 L1 判别索引。

build_pipeline 里 pull_reference → build_index(纯 iNat) → **fold_external** 顺序跑。
解决两个真实问题(2026-07-14 实测发现):
  1. 覆盖: iNat≤N 的欠覆盖株, 用 catalog 已有的 CC 真实 external 图补参考(尤其 cultivar,
     iNat 无独立 taxon)。
  2. **品种误标**: pull_reference 把视觉独特品种(如 Juncus effusus 'Spiralis' 螺旋灯心草、
     Tulipa 'Queen of Night' 黑郁金香)解析成母种、拉了母种图贴品种标签 → 判别索引里参考图
     是错的植物 → 识别 bug 根因。这里按 embedding 自动检出(external 与 iNat 差异大=独特品种=
     母种图误标)、删母种向量换正确 external。

★铁律: 只收真实图(external gallery 是 iNat/Wikimedia 的 CC 真照, 已排 NC)。绝不碰 R2 的
Pencil 生成主图({id}/1_whole.png 等) —— 本脚本只从 {id}/external/ 拉。

过滤(命门, 标定实测: 同株 sim 中位 0.916 / 异株 p95=0.813 严重重叠, 绝对阈值不可行):
  - 有 iNat 的欠覆盖株: **相对 NN** —— 候选图在现索引最近邻==本株 且 sim≥NN_FLOOR 才收
    (避开重叠, 挡掉不是这植物/不是植物的图)。
  - **品种**(带引号 / var. / subsp.)有 iNat + external: 比 external 与本株 iNat 质心相似度,
    < CULTIVAR_MISLABEL_SIM → 独特品种、iNat 是误标母种 → 删 iNat 向量 + 换 external;
    否则(≈母种的普通品种)保留 iNat + 加 external。
  - 0-iNat 株: 无自锚点, external 源头可信(imageingest 按学名匹配)+ 记入待抽查。
  - **当前 catalog 门**: 只处理现役 catalog 的 id(剔 R2 残留的已删 id, 如被删的杂交菊花)。

用法: fold_external.py <index_dir> <plants_index.json> <out_dir> [--cdn URL]
  index_dir: build_index 产出的纯 iNat 索引(index.bin/mapping.json/meta.json)。
  plants_index.json: 当前权威 catalog(id + scientific_name), 做 catalog 门 + 品种判定。
  out_dir: 输出折入后的索引(同格式)。serve 指向它(或原子换回 index_dir)。
"""
import sys, os, re, json, subprocess
import numpy as np, hnswlib

sys.path.insert(0, os.path.dirname(__file__))
import vision_embed

UNDERCOVERED_MAX = 20      # iNat 图数 ≤ 此值才补 external(其上已饱和, kNN 加图边际≈0)
NN_FLOOR = 0.72            # 相对 NN 过滤的相似度下限(同株 sim p5≈0.80, 留余量挡非植物)
CULTIVAR_MISLABEL_SIM = 0.78  # external vs iNat 质心 < 此 → 独特品种、iNat 母种图误标 → 删
                              # 标定(prod 8 个带 iNat 的品种, 2026-07-15): 真误标旗舰螺旋灯心草
                              # 0.737 / 黑郁金香 0.746, 下一簇(深色变种, 母种确实相似)≥0.829 —— 0.70
                              # 太低两旗舰漏检(母种误标被拉回). 0.78 落在 0.746→0.829 空档正中,
                              # 只删两真误标旗舰, 深色变种保留两者(见 keep_both 设计)。
DUP_SIM = 0.98            # 近重复
MAX_EXTERNAL = 8         # 每株最多探测的 external 张数

_CULTIVAR_RE = re.compile(r"'[^']+'")
_INFRA_RE = re.compile(r"\b(?:var|subsp|ssp|f|cv)\.", re.IGNORECASE)


def is_cultivar(sci):
    """catalog 名是品种/变种/亚种(比母种更专一)。"""
    return bool(sci) and bool(_CULTIVAR_RE.search(sci) or _INFRA_RE.search(sci))


def dl_img(url, path):
    """下载并按 magic bytes 校验是 PNG/JPEG(挡掉 CDN 404 的 HTML 错误页)。"""
    subprocess.run(["curl", "-s", "-A", "Mozilla/5.0", "--max-time", "25", "-o", path, url],
                   check=False)
    if not os.path.exists(path) or os.path.getsize(path) < 500:
        return False
    with open(path, "rb") as f:
        sig = f.read(4)
    return sig == b"\x89PNG" or sig[:3] == b"\xff\xd8\xff"


def external_vecs(cid, cdn, tmp):
    """下载 {id}/external/ 的真实图并嵌入, 去近重复。返回 [vec]。"""
    out = []
    for i in range(1, MAX_EXTERNAL + 1):
        p = f"{tmp}/{cid}_{i}.png"
        if not dl_img(f"{cdn}/{cid}/external/{i}.png", p):
            continue
        try:
            v = vision_embed.embed_path(p).astype(np.float32)
        except Exception as e:
            print("  embed err", cid, i, e); continue
        if any(float(v @ k) > DUP_SIM for k in out):
            continue
        out.append(v)
    return out


def _fold_supp(supp_dir, current, add, st):
    """折入 pull_supplemental 深挖并**已验证**的库外图(Phase B)。★以 _manifest.json 为准 —— 只折
    manifest 里记的图, 不扫目录: 重跑失败/删条目后残留的孤儿图不会被误折进 serve 索引(Codex P2)。
    正确性已在 pull_supplemental 判过, 这里只嵌入 + 自去近重复 + 加向量; catalog 门仍生效。"""
    mpath = os.path.join(supp_dir or "", "_manifest.json")
    if not supp_dir or not os.path.isfile(mpath):
        return
    try:
        manifest = json.load(open(mpath))
    except Exception as e:
        print("  supp manifest 读失败, 跳过 Phase B 折入:", e); return
    for cid, rec in sorted(manifest.items()):
        if not isinstance(rec, dict) or cid not in current:   # catalog 门(+ 天然跳过非 dict 的元数据键)
            continue
        added = []
        for img in rec.get("images", []):
            p = os.path.join(supp_dir, cid, img.get("file", ""))
            if not os.path.isfile(p):
                continue
            try:
                v = vision_embed.embed_path(p).astype(np.float32)
            except Exception as e:
                print("  supp embed err", p, e); continue
            if any(float(v @ k) > DUP_SIM for k in added):
                continue
            added.append(v); add.append((v, cid))
        if added:
            st["supp_added"] += len(added)


def main(index_dir, catalog_path, out_dir, cdn, supp_dir=None):
    os.makedirs(out_dir, exist_ok=True)
    tmp = "/tmp/fold_external"; os.makedirs(tmp, exist_ok=True)

    meta = json.load(open(f"{index_dir}/meta.json"))
    mapping = json.load(open(f"{index_dir}/mapping.json"))
    dim, base = meta["dim"], meta["count"]
    catalog = {p["id"]: p["scientific_name"] for p in json.load(open(catalog_path))}
    current = set(catalog)

    live = hnswlib.Index(space="cosine", dim=dim)
    live.load_index(f"{index_dir}/index.bin"); live.set_ef(64)
    base_vecs = np.array(live.get_items(list(range(base))), dtype=np.float32)

    # 按 catalog_id 分组现有 iNat 向量下标
    groups = {}
    for i, m in enumerate(mapping):
        groups.setdefault(m["catalog_id"], []).append(i)

    drop = set()          # 要删的现有向量下标(误标母种图)
    add = []              # [(vec, cid)] 要加的 external
    st = {"undercov_kept": 0, "undercov_rej": 0, "cultivar_replaced": 0,
          "cultivar_kept_both": 0, "zero_added": 0, "stale_skipped": 0, "supp_added": 0}
    spot = []             # 0-iNat 折入的 id → 待抽查

    ext_ids = _list_external_ids(cdn, current)  # 现役 catalog 里哪些有 external
    for cid in sorted(ext_ids):
        if cid not in current:
            st["stale_skipped"] += 1; continue   # 当前 catalog 门: 剔残留 id
        sci = catalog.get(cid, "")
        idxs = groups.get(cid, [])
        n_inat = len(idxs)
        if n_inat > UNDERCOVERED_MAX and not is_cultivar(sci):
            continue   # 已饱和的非品种株: 跳过(加图边际≈0)

        evs = external_vecs(cid, cdn, tmp)
        if not evs:
            continue

        if is_cultivar(sci) and n_inat > 0:
            centroid = base_vecs[idxs].mean(axis=0)
            centroid /= (np.linalg.norm(centroid) + 1e-9)
            sim = float(np.mean([max(float(e @ centroid), 0.0) for e in evs]))
            if sim < CULTIVAR_MISLABEL_SIM:
                # 独特品种: iNat 是误标母种图 → 删, 换 external
                drop.update(idxs); st["cultivar_replaced"] += 1
                for e in evs: add.append((e, cid))
                continue
            st["cultivar_kept_both"] += 1
            for e in evs: add.append((e, cid))
            continue

        if n_inat == 0:
            # 0-iNat: 无自锚点, external 源头可信 + 抽查
            for e in evs: add.append((e, cid))
            spot.append(cid); st["zero_added"] += 1
            continue

        # 有 iNat 的欠覆盖株: 相对 NN 过滤
        for e in evs:
            lab, dist = live.knn_query(e.reshape(1, -1), k=1)
            nn = mapping[int(lab[0][0])]["catalog_id"]; s = 1 - float(dist[0][0])
            if nn == cid and s >= NN_FLOOR:
                add.append((e, cid)); st["undercov_kept"] += 1
            else:
                st["undercov_rej"] += 1

    _fold_supp(supp_dir, current, add, st)   # Phase B: 折入深挖并已验证的库外图

    print("stats:", st, flush=True)
    print(f"删误标向量 {len(drop)} | 加向量 {len(add)}(含 supp {st['supp_added']}) | 0-iNat待抽查 {len(spot)}", flush=True)
    json.dump(sorted(spot), open(f"{out_dir}/_spotcheck.json", "w"))

    # 重建: 原始(去掉 drop 的误标 + 过时已删 id) + external add。
    # ★当前 catalog 门也要作用于 base iNat 向量(Codex #109): pull_reference 从不删旧 ref 目录、
    #   build_index 索引每个 AAA* 目录, 所以从 catalog 删掉的株其 iNat 向量仍在 base + meta.catalog_ids
    #   → 会被 /identify 返回。这里按 current 过滤 base, 保证 catalog 门对 base+external 都生效。
    stale_base = sum(1 for i in range(base)
                     if i not in drop and mapping[i]["catalog_id"] not in current)
    if stale_base:
        print(f"剔除已删 id 的过时 base 向量: {stale_base}", flush=True)
    keep = [(base_vecs[i], mapping[i]) for i in range(base)
            if i not in drop and mapping[i]["catalog_id"] in current]
    new_vecs = [v for v, _ in keep] + [v for v, _ in add]
    new_map = [m for _, m in keep] + [{"catalog_id": c, "src": "ext"} for _, c in add]
    N = len(new_vecs)
    idx = hnswlib.Index(space="cosine", dim=dim)
    idx.init_index(max_elements=N + 10, ef_construction=200, M=32); idx.set_ef(64)
    idx.add_items(np.vstack(new_vecs), np.arange(N))
    nm = dict(meta); nm["count"] = N
    nm["catalog_ids"] = sorted(set(m["catalog_id"] for m in new_map))
    # 原子写, meta 最后(build_index 同幂等约定)。★三文件写在 {out_dir}/.write.lock 内 —— 与飞轮
    #   增量 add(incremental.add_plant_to_index 持同锁)互斥, 防全库 build 的 fold 写与增量 add 交错
    #   写导致 index.bin 与 mapping.json 向量数错配(见 KNN_FLYWHEEL_SPEC.md「Piece 2 并发/锁」)。
    #   惰性导入避免 incremental<->fold_external 循环导入。
    from incremental import _write_lock
    with _write_lock(out_dir):
        idx.save_index(f"{out_dir}/index.bin.tmp"); os.replace(f"{out_dir}/index.bin.tmp", f"{out_dir}/index.bin")
        _atomic_json(new_map, f"{out_dir}/mapping.json")
        _atomic_json(nm, f"{out_dir}/meta.json")
    print(f"final: {N} vecs / {len(nm['catalog_ids'])} ids (原 {base}/{len(set(meta['catalog_ids']))})", flush=True)


R2_EXTERNAL_PREFIX = "r2:yardmate-static/plant_images/"


def _list_external_ids(cdn, current):
    """现役 catalog 里哪些 id 有 external。优先 rclone 一次列 R2(秒级); 失败退化为逐个 CDN
    探测(慢, ~O(catalog) 次 curl)。当前 catalog 门在此已应用(只留 current 的 id)。"""
    try:
        out = subprocess.run(
            ["rclone", "lsf", "-R", "--include", "*/external/index.json", R2_EXTERNAL_PREFIX],
            capture_output=True, text=True, timeout=180, check=True).stdout
        ids = {ln.split("/")[0] for ln in out.splitlines() if ln.strip()}
        got = sorted(ids & current)
        if got:
            return got
        print("rclone 列到 0 个 external, 退化探测", flush=True)
    except Exception as e:
        print("rclone 列 R2 失败, 退化逐个探测:", e, flush=True)
    ids = []
    tmp = "/tmp/fold_probe"; os.makedirs(tmp, exist_ok=True)
    for cid in sorted(current):
        if dl_img(f"{cdn}/{cid}/external/1.png", f"{tmp}/{cid}.png"):
            ids.append(cid)
    return ids


def _atomic_json(obj, path):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, ensure_ascii=False)
    os.replace(tmp, path)


if __name__ == "__main__":
    # 逐个解析: --cdn/--supp **连同其值**一起吃掉, 别把 flag 的值误当位置参(否则 `--supp /dir INAT ...`
    # 会把 /dir 当 index_dir → 读错索引/崩)。位置参 = 非 -- 且不是被吃掉的 flag 值。
    argv = sys.argv[1:]
    cdn = "https://images.yardmate.ai/plant_images"
    supp_dir = None
    args = []
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--cdn":
            cdn = argv[i + 1] if i + 1 < len(argv) else cdn; i += 2; continue
        if a == "--supp":                        # Phase B: 深挖并已验证的库外图目录
            supp_dir = argv[i + 1] if i + 1 < len(argv) else None; i += 2; continue
        if a.startswith("--"):
            i += 1; continue
        args.append(a); i += 1
    if len(args) < 3:
        print(__doc__); sys.exit(2)
    main(args[0], args[1], args[2], cdn, supp_dir)
