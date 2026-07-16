#!/usr/bin/env python3
"""建库第 3.5 步(Phase B): 给欠覆盖库内株**主动深挖**更多真实库外图, 带正确性过滤, 只留验证过的图
(缓存本地 + manifest 存档), 供 fold_external 折进判别索引。**只进索引、不存图到 R2**(用户定)。

Phase A(#109, fold_external)= 把 catalog 已有的 ~4 张 external gallery 图折进索引。
Phase B(这里)= 超出那 ~4 张, 从 Wikimedia(深挖)/GBIF/PlantNet 主动多抓, 加正确性过滤:
  - 有锚点的欠覆盖**种**(非品种, iNat 有图): 相对 NN(候选图最近邻==本株 且 sim≥NN_FLOOR 才收)。
  - 无锚点株(0-iNat / cultivar —— iNat 无独立 taxon 或母种图误标, 无法和自己比对):
    聚类粗筛 + GPT-4o 定边界(见 supp_verify)。品种一律走这条(母种锚点不可信, 见 SPEC §7)。

★铁律: 只收真实图 + 只收 CC0/CC-BY/CC-BY-SA(supp_sources.classify_license 把关, NC/ND 绝不用)。
★质量>数量: 每株封顶 MAX_KEEP, 优先 0-iNat + cultivar + 真正稀薄的种; 已够覆盖的常见种不狂抓。

用法: pull_supplemental.py <inat_index_dir> <catalog.json> <supp_out_dir> [--limit N]
  inat_index_dir: build_index 产出的纯 iNat 索引(index.bin/mapping.json/meta.json)—— 定覆盖 + 锚点 NN。
  catalog.json:   当前权威 catalog(id + scientific_name)。
  supp_out_dir:   输出 {cid}/NNN.jpg(验证过的图, 供 fold) + _manifest.json(可续 + 存档)。
  --limit N:      本轮最多处理 N 株(优先级序, 可续)—— 卡时间窗分批跑 / 先小批验证时用。
env: OPENAI_API_KEY(GPT 判据, 无则无锚点株跳过—— 绝不折未验证图)。
     PLANTNET_API_KEY(可选图源, 仅非品种种级用); PLANTNET_MAX_IDENTIFY(默认 60, =0 关)。
"""
import sys
import os
import re
import json
import time
import shutil

sys.path.insert(0, os.path.dirname(__file__))
import supp_sources as src
import supp_verify as verify
# numpy / hnswlib / vision_embed(torch)是重依赖, 只在建库路径需要 → 懒加载(见 _embed_candidates /
# main), 好让 select_targets / is_cultivar 等纯函数在没装 torch 的环境里也能被 test_supplemental 导入。

UNDERCOVERED_MAX = 20     # iNat 图数 > 此的非品种株已饱和, 不补(kNN 加图边际≈0)
SPECIES_MIN = 10          # 有锚点的**种**: n_inat ≥ 此视作"够覆盖", 不深挖(只补真正稀薄的)
NN_FLOOR = 0.72           # 相对 NN 过滤下限(与 fold_external 一致)
DUP_SIM = 0.98            # 近重复
MAX_FETCH = 40            # 每株最多下载+嵌入的候选数(护建库时长)
MAX_KEEP = 12             # 每株最多收下的验证图数(饱和即止)
MAX_VLM_CALLS = int(os.environ.get("VISION_SUPP_MAX_VLM", "12000"))  # GPT 全局硬上限
PLANTNET_MAX_IDENTIFY = int(os.environ.get("PLANTNET_MAX_IDENTIFY", "60"))  # PlantNet 图源配额封顶

# 品种/变种/亚种判定(镜像 fold_external.is_cultivar)。
_CULTIVAR_RE = re.compile(r"'[^']+'")
_INFRA_RE = re.compile(r"\b(?:var|subsp|ssp|f|cv)\.", re.IGNORECASE)


def is_cultivar(sci):
    return bool(sci) and bool(_CULTIVAR_RE.search(sci) or _INFRA_RE.search(sci))


def species_of(sci):
    """去 cultivar 引号得种级名(Wikimedia 深挖/GBIF/PlantNet 的种级查询用)。"""
    return re.sub(r"\s*'[^']+'", "", sci).strip()


def select_targets(catalog, coverage, species_min=SPECIES_MIN, undercovered_max=UNDERCOVERED_MAX):
    """纯函数(可单测): 定哪些株要深挖 + 走哪条判据。按优先级排序: 0-iNat → cultivar → 稀薄种。
    catalog: {cid: sci}; coverage: {cid: n_inat}。
    返回 [(cid, sci, mode)], mode ∈ {'anchorfree','anchored'}, 已按优先级排好。"""
    zero, cult, thin = [], [], []
    for cid, sci in catalog.items():
        n = coverage.get(cid, 0)
        if is_cultivar(sci):
            # 品种: 一律无锚点判据(母种锚点不可信, 会误拒真品种图 / 误收母种图)。
            cult.append((cid, sci, "anchorfree"))
        elif n == 0:
            zero.append((cid, sci, "anchorfree"))
        elif n < species_min:
            # 有锚点的稀薄种: 相对 NN 免费又准。
            thin.append((cid, sci, "anchored"))
        # n ≥ species_min 的非品种种: 够覆盖, 跳过(别狂抓常见种)。
    # 优先级: 0-iNat 最缺 → 品种(硬骨头) → 稀薄种。稀薄种内按覆盖升序(越缺越先)。
    thin.sort(key=lambda t: coverage.get(t[0], 0))
    return zero + cult + thin


def _atomic_json(obj, path):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, ensure_ascii=False, indent=2)
    os.replace(tmp, path)


def _embed_candidates(cands, tmp):
    """下载 + 嵌入 + 自去近重复。返回对齐的 (evs, paths, kept_cands)。"""
    import numpy as np
    import vision_embed
    os.makedirs(tmp, exist_ok=True)   # download 直接 open(path) 写, 目录须先在(PlantNet 用子目录)
    evs, paths, kept = [], [], []
    for i, c in enumerate(cands[:MAX_FETCH]):
        p = f"{tmp}/{i:03d}.jpg"
        if not src.download(c.url, p):
            continue
        try:
            v = vision_embed.embed_path(p).astype(np.float32)
        except Exception as e:
            print("  embed err", c.url, e)
            continue
        if any(float(v @ e) > DUP_SIM for e in evs):
            continue   # 跨源近重复(GBIF 常和 iNat 重叠)
        evs.append(v)
        paths.append(p)
        kept.append(c)
    return evs, paths, kept


def _gather(sci, is_cult):
    """从三源收候选(已 license 门)。品种用**全名**偏向品种图 + 种级兜底; 非品种用种级。
    GBIF 一次解析: 同义词→接受名(Azalea→Rhododendron 也搜, 救学名过时的收 0 株)+ 属级/泛指
    ('Petunia spp.'/'x hybrida')判定(非品种时跳, 免拉泛属噪声)+ 复用 taxon key(免二次解析)。"""
    sp = species_of(sci)
    gbif_err = False
    try:
        r = src.gbif_resolve(sp)                   # None=真·无匹配; raise=瞬时网络失败
    except Exception as e:
        print("  [gbif] resolve err", sp, e); r = None; gbif_err = True
    if (not is_cult) and r is not None and not r[2]:
        return [], False                           # 非品种但名字是属级/泛指 → 不深挖(真·跳过, 非瞬时失败)
    accepted = r[1] if r else sp
    key = r[0] if r else None

    cands, seen = [], set()
    n_err = [0]    # 源报错计数(源出错返回 None; 任一报错 → 无候选时别记 n:0, 见下)

    def add(lst):
        if lst is None:         # 源瞬时失败(网络/5xx)—— 与"正常返回空"区分
            n_err[0] += 1
            return
        for c in lst:
            if c.dedup_key not in seen:
                seen.add(c.dedup_key)
                cands.append(c)

    if is_cult:
        add(src.wikimedia_search(sci, limit=40))      # 全品种名, 命中真品种图
        time.sleep(0.6)
        add(src.wikimedia_search(sp, limit=20))       # 种级兜底(GPT 会滤掉母种噪声)
    else:
        add(src.wikimedia_search(sp, limit=50))
    if accepted and accepted.lower() != sp.lower():   # 同义词的接受名(如 Rhododendron calendulaceum)也搜
        time.sleep(0.6)
        add(src.wikimedia_search(accepted, limit=30))
    time.sleep(0.6)
    # GBIF media: 有 key 按 key 取; 无 key 时 —— 解析**瞬时失败**(gbif_err)记一次源错误(别用 by-name
    # 掩盖: 那样会返回 [] 被当"成功空源" → 漏建, Codex P2); 解析成功但真·无 taxon 才记一次成功空源。
    if key:
        add(src.gbif_media_by_key(key, 60))
    elif gbif_err:
        n_err[0] += 1
    # else: 解析成功但真·无 taxon → 无 GBIF 图可取, 不算源(不影响 any_err)
    # any_err = **任一**源报错 → 无候选时别记 n:0: 那个挂掉的源可能才有图, 记 n:0 会漏建。
    # 只有全源**干净**返回空(n_err==0)才是真·无图(Codex: 部分失败也不能当真空, 不能要求全源都挂)。
    any_err = n_err[0] > 0
    return cands, any_err


def main(index_dir, catalog_path, out_dir, limit=None):
    import numpy as np
    import hnswlib
    os.makedirs(out_dir, exist_ok=True)
    api_key = os.environ.get("OPENAI_API_KEY", "").strip()
    pn_key = os.environ.get("PLANTNET_API_KEY", "").strip()
    if not api_key:
        print("WARN: 无 OPENAI_API_KEY → 无锚点株(0-iNat/cultivar)无 GPT 判据, 将跳过(绝不折未验证图)。")

    plants = json.load(open(catalog_path))
    catalog = {p["id"]: p["scientific_name"] for p in plants}
    commons = {p["id"]: p.get("common_name", "") for p in plants}  # 传给 GPT 判据, 助判品种(如"黑郁金香")

    meta = json.load(open(f"{index_dir}/meta.json"))
    mapping = json.load(open(f"{index_dir}/mapping.json"))
    dim, base = meta["dim"], meta["count"]
    live = hnswlib.Index(space="cosine", dim=dim)
    live.load_index(f"{index_dir}/index.bin"); live.set_ef(64)

    groups = {}
    for i, m in enumerate(mapping):
        groups.setdefault(m["catalog_id"], []).append(i)
    coverage = {cid: len(groups.get(cid, [])) for cid in catalog}

    targets = select_targets(catalog, coverage)
    print(f"targets: {len(targets)} 株 (0-iNat/cultivar 无锚点 + 稀薄种), 全 catalog {len(catalog)}", flush=True)

    manifest_path = f"{out_dir}/_manifest.json"
    manifest = json.load(open(manifest_path)) if os.path.exists(manifest_path) else {}
    budget = verify.VLMBudget(MAX_VLM_CALLS)
    pn_budget = verify.VLMBudget(PLANTNET_MAX_IDENTIFY)   # 复用计数器当 PlantNet 配额
    st = {"anchored_kept": 0, "anchorfree_kept": 0, "skipped_done": 0,
          "no_key_skip": 0, "empty": 0, "plants_covered": 0}
    spot = []
    processed = 0   # 本轮实际动手处理的株数(不含 manifest 跳过); --limit 用它封顶

    for cid, sci, mode in targets:
        if limit and processed >= limit:
            break   # --limit: 本轮到量收工(可续: 下轮从 manifest 之后接着跑)
        if budget.mostly_failing():
            # GPT 连续几乎全失败(疑 key 失效/OpenAI 挂)→ 中止。★放循环顶: 即使某株走 continue
            # (incomplete/预算耗尽)也拦得住, 不会绕过这道闸把后面成百上千株全白试一遍(Codex P2)。
            raise RuntimeError(f"GPT 判据几乎全失败 ({budget.errors}/{budget.spent}) —— 疑 OPENAI_API_KEY "
                               f"失效或 OpenAI 不可用; 中止(已处理株已存 manifest, 修好后删对应条目重跑)")
        if cid in manifest:
            st["skipped_done"] += 1; continue
        if mode == "anchorfree" and not api_key:
            st["no_key_skip"] += 1; continue   # 无 GPT → 不碰无锚点株
        if mode == "anchorfree" and not budget.ok():
            continue   # GPT 预算耗尽: 不记 done, 留给下轮新预算重试(绝不记成 done-with-zero 永久跳过)
        processed += 1

        tmp = f"/tmp/supp/{cid}"
        cands, any_err = _gather(sci, is_cultivar(sci))
        evs, paths, kept_cands = _embed_candidates(cands, tmp)

        # PlantNet 图源(可选, 仅非品种种级, seed=已有第一张, 消耗线上配额)。
        if (mode == "anchored" and pn_key and pn_budget.ok() and paths):
            pn_budget.take()
            pn_cands = src.plantnet_related(paths[0], species_of(sci), pn_key)
            if pn_cands:
                import numpy as np
                pev, ppath, pk = _embed_candidates(pn_cands, tmp + "/pn")
                for v, pth, c in zip(pev, ppath, pk):   # 并进候选池, 但去掉与已抓图的近重复(PlantNet 常和 GBIF/iNat 撞图)
                    if not any(float(v @ e) > DUP_SIM for e in evs):
                        evs.append(v); paths.append(pth); kept_cands.append(c)

        if not evs:
            # 只有**全源干净返回空**才算真·无图(记 n:0, 免每轮白试)。任一源报错(它可能才有图)、
            # 或有候选但全下载失败 → 疑瞬时 → 绝不记 n:0(否则一次 API/CDN 抖动就把该株永久跳过、
            # 静默漏建, Codex 一路收窄至此: 部分失败也不能当真空)。
            if any_err or cands:
                shutil.rmtree(tmp, ignore_errors=True)
                continue   # 疑瞬时: 不记 done, 留待下轮重试
            manifest[cid] = {"sci": sci, "mode": mode, "n": 0}
            _atomic_json(manifest, manifest_path); shutil.rmtree(tmp, ignore_errors=True)
            st["empty"] += 1; continue

        # ── 正确性判据 ──
        if mode == "anchored":
            keep_idx = []
            for j, e in enumerate(evs):
                lab, dist = live.knn_query(e.reshape(1, -1), k=1)
                nn = mapping[int(lab[0][0])]["catalog_id"]; s = 1 - float(dist[0][0])
                if nn == cid and s >= NN_FLOOR:
                    keep_idx.append(j)
            keep_idx = keep_idx[:MAX_KEEP]
        else:
            keep_idx, incomplete = verify.verify_anchorfree(evs, paths, sci, commons.get(cid, ""),
                                                            api_key, budget, MAX_KEEP)
            spot.append(cid)
            if incomplete and not keep_idx:
                shutil.rmtree(tmp, ignore_errors=True)
                continue   # 预算耗尽/GPT 报错 且一张没收 → 不记 done, 下轮新预算/恢复后重试

        # ── 落盘: 验证过的图 + manifest 条目 ──
        dst = f"{out_dir}/{cid}"
        shutil.rmtree(dst, ignore_errors=True)   # 先清旧图: 重跑(如删 manifest 条目后)收更少时, 免留孤儿被 fold 重收
        entries = []
        for out_i, j in enumerate(keep_idx, 1):
            os.makedirs(dst, exist_ok=True)
            fn = f"{dst}/{out_i:03d}.jpg"
            shutil.move(paths[j], fn)   # 跨文件系统安全: tmp 常是 tmpfs, os.replace 会 EXDEV(Invalid cross-device link)
            e = kept_cands[j].as_manifest(); e["file"] = f"{out_i:03d}.jpg"
            entries.append(e)
        manifest[cid] = {"sci": sci, "mode": mode, "n": len(entries), "images": entries}
        _atomic_json(manifest, manifest_path)
        shutil.rmtree(tmp, ignore_errors=True)   # 清未收下的候选 tmp(已收的早 os.replace 出去了)
        if entries:
            st["plants_covered"] += 1
            st["anchored_kept" if mode == "anchored" else "anchorfree_kept"] += len(entries)
        print(f"  {cid} {mode} {sci[:40]}: 候选 {len(evs)} → 收 {len(entries)} "
              f"[GPT {budget.spent}/{budget.max}]", flush=True)

    _atomic_json(manifest, manifest_path)
    _atomic_json(sorted(spot), f"{out_dir}/_spotcheck.json")
    print(f"DONE {st} | GPT calls {budget.spent}/{budget.max} | PlantNet {pn_budget.spent}/{pn_budget.max}", flush=True)


if __name__ == "__main__":
    # --limit N: 本轮最多动手 N 株(优先级序: 0-iNat → cultivar → 稀薄种); 可续。用于卡时间窗的分批跑。
    argv = sys.argv[1:]
    limit = None
    args = []
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--limit":
            limit = int(argv[i + 1]); i += 2; continue
        if a.startswith("--"):
            i += 1; continue
        args.append(a); i += 1
    if len(args) < 3:
        print(__doc__); sys.exit(2)
    main(args[0], args[1], args[2], limit)
