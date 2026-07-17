#!/usr/bin/env python3
"""增量建索引核心(Piece 2): 把**单株**刚晋升的真实 external 图加进 L1 判别索引, 免全库重建。

飞轮命门。设计依据(见 KNN_FLYWHEEL_SPEC.md「Piece 2」):
  新株晋升完成时 R2 只有它的 {id}/external/ **真照**(imageingest 按学名匹配 + web 审核已验证),
  **没有 iNat 参考**(iNat 靠全库 pull_reference, 增量不跑)。所以增量 add = fold_external 的
  「0-iNat 可信 external」情形 —— fold 的两大重过滤对增量**都不适用**:
    - CULTIVAR_MISLABEL(删误标母种向量): 只在 pull 误拉母种 iNat 图时需要; 增量从不拉 iNat → 无风险。
    - NN_FLOOR 相对 NN 过滤: 防欠覆盖株 external 混入非本株图; 增量的 external 是该株自己已验证真照 → 不需要。
  增量**必须保留**的安全: ①只 external/(绝不碰生成主图, P0 铁律)②DUP_SIM 自去近重复 ③可信源=promote 已验证。
  catalog 门去掉: fold 的 catalog 门是全库重建剔 R2 残留已删 id; 增量是 targeted 单株(cid 由可信
  promote 钩子显式给), 不能用服务器上过时的 plants_index.json。

铁律: 只从 {cdn}/{cid}/external/{i}.png 拉真照。原子写三文件(index.bin/mapping.json/meta.json,
meta 最后 = 幂等约定)。flock {index_dir}/.write.lock 与全库 build 的 fold 写互斥。

纯磁盘核心 + 注入 embed_fn(无 torch 依赖) —— standalone CLI(incremental_add.py)注入自带模型;
serve 的 admin 端点注入**已加载**的模型(省第二次 torch load)。同一核心避免漂移。
"""
import os
import json
import fcntl
from contextlib import contextmanager

import numpy as np
import hnswlib

from fold_external import dl_img, DUP_SIM, MAX_EXTERNAL  # 复用已验证的下载(magic bytes)+ 常量


@contextmanager
def _write_lock(index_dir):
    """对 index_dir 的写加进程级排他锁 —— 与全库 build 的 fold_external 最终写互斥(同一 .write.lock),
    也串行化并发增量 add。锁挂在独立 .write.lock(索引文件走 os.replace 换 inode, 锁不能挂被替换的文件)。"""
    os.makedirs(index_dir, exist_ok=True)
    lf = open(os.path.join(index_dir, ".write.lock"), "a+")
    try:
        fcntl.flock(lf.fileno(), fcntl.LOCK_EX)
        yield
    finally:
        lf.close()  # close 释放 flock(异常路径也走到)


def _atomic_json(obj, path, **kw):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, **kw)
    os.replace(tmp, path)


def _save_index(index_dir, idx, mapping, meta):
    """原子写三文件, meta 最后(与 build_index/fold_external 同幂等约定: 无 meta = 半成品)。"""
    idx.save_index(f"{index_dir}/index.bin.tmp")
    os.replace(f"{index_dir}/index.bin.tmp", f"{index_dir}/index.bin")
    _atomic_json(mapping, f"{index_dir}/mapping.json", ensure_ascii=False)
    _atomic_json(meta, f"{index_dir}/meta.json", indent=2)


def _external_vecs(cid, cdn, embed_fn, tmp_dir, max_external=MAX_EXTERNAL):
    """下载 {cdn}/{cid}/external/{i}.png 真照并嵌入, 同株内去近重复。返回 [np.float32 vec]。
    只 external/(P0: 绝不碰 {id}/{slot}.png 生成主图)。"""
    os.makedirs(tmp_dir, exist_ok=True)
    out = []
    for i in range(1, max_external + 1):
        p = os.path.join(tmp_dir, f"{cid}_{i}.png")
        if not dl_img(f"{cdn}/{cid}/external/{i}.png", p):
            continue  # 缺图/CDN 404(非 PNG/JPEG magic) → 跳过
        try:
            v = embed_fn(p).astype(np.float32)
        except Exception as e:  # noqa: BLE001 —— 单张坏图不该毁整株
            print("  embed err", cid, i, e, flush=True)
            continue
        if any(float(v @ k) > DUP_SIM for k in out):
            continue  # 同株近重复
        out.append(v)
    return out


def add_plant_to_index(index_dir, cid, cdn, embed_fn, *, tmp_dir="/tmp/incremental_add",
                       max_external=MAX_EXTERNAL):
    """把 cid 的 external 真照加进 index_dir 的索引(原地, 原子写)。幂等:
      - cid 不在索引 → resize_index + add_items(快路径)。
      - cid 已在索引(re-trigger/重试) → drop 该 cid 旧向量 + 重建(保留其余) → 刷新。
    返回 stats dict。整个 load→写在 flock 内, 与全库 fold 写互斥。"""
    with _write_lock(index_dir):
        meta = json.load(open(f"{index_dir}/meta.json"))
        mapping = json.load(open(f"{index_dir}/mapping.json"))
        dim = meta["dim"]

        evs = _external_vecs(cid, cdn, embed_fn, tmp_dir, max_external)
        if not evs:
            return {"cid": cid, "added": 0, "count": meta["count"],
                    "note": "无 external 真照可加(R2 无 {id}/external/ 或全部下载/嵌入失败)"}

        live = hnswlib.Index(space="cosine", dim=dim)
        live.load_index(f"{index_dir}/index.bin")
        live.set_ef(64)
        base = live.get_current_count()

        # 幂等键 = 该 cid **之前增量加过的 external** 向量(src=="ext")。re-trigger 时只刷新这些,
        # **保留 iNat 向量**(build_index 的 {"catalog_id","photo"} 无 src) —— 避免误对 iNat 覆盖株
        # 调用时把它的 iNat 参考也一起 drop 掉(降覆盖)。飞轮只加 0-iNat 新株, 但这样对任意 cid 都安全。
        def _is_this_cid_ext(m):
            return m.get("catalog_id") == cid and m.get("src") == "ext"
        cid_ext_present = any(_is_this_cid_ext(m) for m in mapping)
        # 防御: mapping 长度必须与索引向量数一致(否则 label→catalog_id 错位) → 不一致走重建修复。
        consistent = base == len(mapping)

        if cid_ext_present or not consistent:
            # 重建路径(幂等刷新 / 修 mapping 错位): drop 该 cid 旧 external + 保留其余(含其 iNat) + 加新 external。
            keep = [i for i in range(min(base, len(mapping))) if not _is_this_cid_ext(mapping[i])]
            base_vecs = np.array(live.get_items(list(range(base))), dtype=np.float32)
            new_vecs = [base_vecs[i] for i in keep] + list(evs)
            new_map = [mapping[i] for i in keep] + [{"catalog_id": cid, "src": "ext"} for _ in evs]
            N = len(new_vecs)
            idx = hnswlib.Index(space="cosine", dim=dim)
            idx.init_index(max_elements=N + 10, ef_construction=200, M=32)
            idx.set_ef(64)
            idx.add_items(np.vstack(new_vecs), np.arange(N))
            mode = "rebuild"
        else:
            # 快路径: 就地 resize + add_items(<1s)。labels 接在现有 [0, base) 之后。
            n = len(evs)
            live.resize_index(base + n)
            live.add_items(np.vstack(evs), np.arange(base, base + n))
            idx = live
            new_map = mapping + [{"catalog_id": cid, "src": "ext"} for _ in evs]
            N = base + n
            mode = "add_items"

        new_meta = dict(meta)
        new_meta["count"] = N
        new_meta["catalog_ids"] = sorted(set(m["catalog_id"] for m in new_map))
        _save_index(index_dir, idx, new_map, new_meta)
        return {"cid": cid, "added": len(evs), "count": N, "mode": mode,
                "in_catalog_ids": cid in set(new_meta["catalog_ids"])}
