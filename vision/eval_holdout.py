#!/usr/bin/env python3
"""Phase B 效果验证(staging/副本先跑, 再决定上 prod)。留一法(LOO)量化"深挖库外图"带来的
识别可分性提升 —— 不需要外部用户照片测试集, 用索引里各株自己的参考向量互验。

思路: 对每株, 逐个把它的某条参考向量"临时删除"(hnswlib mark_deleted), 拿这条向量当 query 查索引,
看 top-1 命中的是不是本株。命中率 = 该株参考图能否把"自己的另一张照片"拉到自己名下(真识别的代理)。
  - serve 索引(已折入 external+supp): covered 株(尤其原 0-iNat/cultivar)现在有 ≥1 真实向量 → 有 LOO 分。
  - inat 索引(纯 iNat, Phase A/B 前): 同一批 query 向量拿去查 → 0-iNat/cultivar 株**根本不在库里**,
    top-1 必落到别的植物(before = 不可识别)。两相对比即为提升。

用法: eval_holdout.py <inat_index_dir> <serve_index_dir> [supp_manifest.json]
  给 supp_manifest.json → 只聚焦 Phase B 深挖过的株(精确报 Phase B 增益); 不给 → 报所有折入株。
"""
import sys
import os
import json

import numpy as np
import hnswlib


def _load(index_dir):
    meta = json.load(open(f"{index_dir}/meta.json"))
    mapping = json.load(open(f"{index_dir}/mapping.json"))
    idx = hnswlib.Index(space="cosine", dim=meta["dim"])
    idx.load_index(f"{index_dir}/index.bin"); idx.set_ef(96)
    return idx, mapping, meta


def _top1(idx, vec, k=1):
    lab, dist = idx.knn_query(vec.reshape(1, -1), k=k)
    return int(lab[0][0]), 1 - float(dist[0][0])


def _top1_excl_self(idx, mapping, vec, thresh=0.9999):
    """top-1 catalog_id, 但跳过 sim≥thresh 的"同图副本"。BEFORE 用: serve 的 iNat 向量是 iNat 库的
    逐字拷贝, 不排掉的话 anchored 株的 query 会自匹配到 sim~1 的副本 → 虚高 before、低估增益。"""
    lab, dist = idx.knn_query(vec.reshape(1, -1), k=2)
    for j in range(len(lab[0])):
        if 1 - float(dist[0][j]) < thresh:
            return mapping[int(lab[0][j])]["catalog_id"]
    return mapping[int(lab[0][0])]["catalog_id"]   # 全是自副本(极罕见)→ 退回 top-1


def main(inat_dir, serve_dir, supp_manifest=None):
    serve, smap, smeta = _load(serve_dir)
    inat, imap, imeta = _load(inat_dir)

    # serve 里各 cid 的向量下标(全体, 用于 LOO)。
    groups = {}
    for i, m in enumerate(smap):
        groups.setdefault(m["catalog_id"], []).append(i)

    # 聚焦集: 给了 supp manifest 就只看 Phase B 深挖过且真收到图的株; 否则看所有"折入过"(src=ext)的株。
    if supp_manifest and os.path.exists(supp_manifest):
        man = json.load(open(supp_manifest))
        focus = sorted(c for c, v in man.items() if isinstance(v, dict) and v.get("n", 0) > 0)
        label = "Phase B 深挖株"
    else:
        focus = sorted({m["catalog_id"] for m in smap if m.get("src") == "ext"})
        label = "折入株(external+supp)"

    inat_cids = {m["catalog_id"] for m in imap}
    after_hit = after_tot = 0
    before_absent = before_hit = before_tot = 0
    per_plant = []

    for cid in focus:
        idxs = groups.get(cid, [])
        if not idxs:
            continue
        hit = tot = 0
        for li in idxs:
            v = np.array(serve.get_items([li])[0], dtype=np.float32)
            # AFTER: 临时删本条 → query → top-1 是否本株(留一)。
            serve.mark_deleted(li)
            try:
                top_cid_i, _ = _top1(serve, v)
                ok = smap[top_cid_i]["catalog_id"] == cid
            finally:
                serve.unmark_deleted(li)
            hit += int(ok); tot += 1; after_hit += int(ok); after_tot += 1
            # BEFORE: 同一 query 查纯 iNat 索引, 排掉同图副本(否则 anchored 株自匹配 sim~1 虚高)。
            b_ok = _top1_excl_self(inat, imap, v) == cid
            before_hit += int(b_ok); before_tot += 1
            if cid not in inat_cids:
                before_absent += 1
        per_plant.append((cid, hit, tot))

    print(f"聚焦: {label} —— {len(per_plant)} 株有可验证向量", flush=True)
    print(f"AFTER  (serve, LOO top-1):  {after_hit}/{after_tot} = "
          f"{after_hit / max(after_tot, 1):.1%}", flush=True)
    print(f"BEFORE (纯 iNat, 同 query):  {before_hit}/{before_tot} = "
          f"{before_hit / max(before_tot, 1):.1%}  "
          f"(其中 {before_absent}/{before_tot} 条的株根本不在 iNat 库 = 之前不可识别)", flush=True)
    # 最差的几株(收了图但 LOO 仍低)= 值得人工抽查的疑似残留噪声。
    weak = sorted((h / max(t, 1), c, h, t) for c, h, t in per_plant)[:15]
    print("LOO 最弱 15 株(疑残留噪声, 建议抽查 _spotcheck.json 里的图):", flush=True)
    for r, c, h, t in weak:
        print(f"  {c}: {h}/{t} = {r:.0%}", flush=True)


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print(__doc__); sys.exit(2)
    main(sys.argv[1], sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else None)
