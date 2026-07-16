#!/usr/bin/env python3
"""Phase B 纯函数守卫(无网络/无 torch): python3 vision/test_supplemental.py

守的命门:
  1. classify_license —— 铁律"只 CC0/BY/SA, 绝不 NC/ND"。错判会让 NC 图混进商用训练 → 违许可。
  2. select_targets —— 优先级(0-iNat→cultivar→稀薄种)+ 品种一律无锚点 + 别狂抓已够覆盖的种。
  3. cluster_consensus —— 主簇识别 + 无共识退化(numpy 在则测, 不在则跳)。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import supp_sources as src
import pull_supplemental as ps


def test_license_allows_cc0_by_sa():
    assert src.classify_license("cc0") == "CC0"
    assert src.classify_license("CC0_1_0") == "CC0"                     # GBIF 枚举
    assert src.classify_license("cc-by") == "CC-BY"
    assert src.classify_license("cc-by-sa") == "CC-BY-SA"
    assert src.classify_license("cc-by-sa-4.0") == "CC-BY-SA"           # Wikimedia 版本码
    assert src.classify_license("CC_BY_SA_4_0") == "CC-BY-SA"
    assert src.classify_license("http://creativecommons.org/licenses/by/4.0/") == "CC-BY"
    assert src.classify_license("http://creativecommons.org/licenses/by-sa/3.0/") == "CC-BY-SA"
    assert src.classify_license("http://creativecommons.org/publicdomain/zero/1.0/") == "CC0"
    assert src.classify_license("http://creativecommons.org/publicdomain/mark/1.0/") == "PD"


def test_license_rejects_nc_nd_and_unknown():
    # NC/ND 是铁律红线 —— 任一命中即拒, 优先级高于任何"允许"。
    assert src.classify_license("cc-by-nc") is None
    assert src.classify_license("cc-by-nc-sa") is None                  # 带 sa 也拒(有 nc)
    assert src.classify_license("cc-by-nc-nd") is None
    assert src.classify_license("cc-by-nd") is None
    assert src.classify_license("CC_BY_NC_4_0") is None                 # GBIF 枚举
    assert src.classify_license("http://creativecommons.org/licenses/by-nc/4.0/") is None  # 探针实测这条
    assert src.classify_license("") is None
    assert src.classify_license("ARR") is None
    assert src.classify_license("GFDL") is None                         # 只 GFDL → 拒
    assert src.classify_license("", "Public domain") == "PD"            # 老文件只有短名
    # ★回归: 机读 code 为空、NC/ND 只写在 short_name(老 Commons 文件)—— 曾把这些误判成 CC-BY 折进
    #   商用索引(违铁律)。NC/ND veto 必须对 code+短名 一并查。
    assert src.classify_license("", "CC BY-NC 2.0") is None
    assert src.classify_license("", "CC BY-NC-SA 3.0") is None
    assert src.classify_license("", "CC BY-ND 4.0") is None
    assert src.classify_license("", "CC BY-NC-ND 4.0") is None
    assert src.classify_license("cc-by", "CC BY-NC-SA 3.0") is None     # code 没 nc 但短名有 → 仍拒
    assert src.classify_license("", "CC BY-SA 3.0") == "CC-BY-SA"       # 干净短名仍正常放行
    assert src.classify_license("", "CC BY 4.0") == "CC-BY"


def test_gbif_interpret_synonym_and_genus():
    gi = src._gbif_interpret
    # EXACT 同义词 → 换接受名 + 接受 taxon key(实测响应)
    d = {"rank": "SPECIES", "status": "SYNONYM", "matchType": "EXACT",
         "usageKey": 2883038, "acceptedUsageKey": 2883036,
         "species": "Rhododendron calendulaceum", "speciesKey": 2883036}
    key, acc, spec = gi(d, "Azalea calendulacea")
    assert acc == "Rhododendron calendulaceum" and key == 2883036 and spec is True
    # 属级/泛指(HIGHERRANK)→ specific False(非品种时调用方会跳)
    _, _, spec = gi({"rank": "GENUS", "status": "ACCEPTED", "matchType": "HIGHERRANK"}, "Petunia spp.")
    assert spec is False
    # 正常种(ACCEPTED, 非同义词)→ 名字不变, specific True(别把 Lavandula x intermedia 误跳)
    key, acc, spec = gi({"rank": "SPECIES", "status": "ACCEPTED", "matchType": "EXACT",
                         "usageKey": 9, "species": "Lavandula intermedia"}, "Lavandula x intermedia")
    assert acc == "Lavandula x intermedia" and spec is True and key == 9
    # FUZZY 同义词 → 保守不换名(防误配 flammea→flava)
    key, acc, spec = gi({"rank": "SPECIES", "status": "SYNONYM", "matchType": "FUZZY",
                         "usageKey": 4164196, "species": "Rhododendron luteum"}, "Azalea flammea")
    assert acc == "Azalea flammea" and key == 4164196 and spec is True
    # 无匹配 / 空 → None
    assert gi({"matchType": "NONE"}, "x") is None
    assert gi({}, "x") is None


def test_is_cultivar_and_species_of():
    assert ps.is_cultivar("Juncus effusus 'Spiralis'")
    assert ps.is_cultivar("Brassica oleracea var. acephala")
    assert ps.is_cultivar("Hosta subsp. something")
    assert not ps.is_cultivar("Rosa chinensis")
    assert ps.species_of("Juncus effusus 'Spiralis'") == "Juncus effusus"
    assert ps.species_of("Rosa chinensis") == "Rosa chinensis"


def test_select_targets_priority_and_mode():
    catalog = {
        "AAA1": "Rosa chinensis",                 # 种, n=0  → anchorfree(zero)
        "AAA2": "Juncus effusus 'Spiralis'",      # 品种     → anchorfree(cult)
        "AAA3": "Brassica oleracea var. acephala",# 变种     → anchorfree(cult)
        "AAA4": "Acer palmatum",                  # 种, n=30 → 跳过(够覆盖)
        "AAA5": "Thin speciosa",                  # 种, n=4  → anchored(thin)
        "AAA6": "Thinner speciosa",               # 种, n=1  → anchored(thin, 更缺 → 更靠前)
    }
    cov = {"AAA1": 0, "AAA2": 5, "AAA3": 0, "AAA4": 30, "AAA5": 4, "AAA6": 1}
    targets = ps.select_targets(catalog, cov)
    ids = [t[0] for t in targets]
    modes = {t[0]: t[2] for t in targets}

    assert "AAA4" not in ids, "够覆盖的非品种种不该被深挖(别狂抓常见种)"
    assert ids[0] == "AAA1", "0-iNat 最优先"
    assert set(ids[1:3]) == {"AAA2", "AAA3"}, "品种紧随 0-iNat"
    assert ids[3:] == ["AAA6", "AAA5"], "稀薄种最后, 且越缺越先(1 < 4)"
    assert modes["AAA1"] == "anchorfree"
    assert modes["AAA2"] == "anchorfree" and modes["AAA3"] == "anchorfree"
    assert modes["AAA5"] == "anchored" and modes["AAA6"] == "anchored"


def test_cluster_consensus():
    try:
        import numpy as np
    except ImportError:
        print("SKIP test_cluster_consensus (no numpy)")
        return
    import supp_verify as verify

    def unit(x):
        x = np.array(x, dtype=np.float32)
        return x / np.linalg.norm(x)

    # 4 张几乎相同的图(紧簇 sim~1) + 1 张离群。
    base = unit([1, 0.02, 0, 0])
    vecs = [base, unit([1, 0.01, 0.01, 0]), unit([0.99, 0.03, 0, 0.01]),
            unit([1, 0.0, 0.02, 0]), unit([0, 1, 0, 0])]  # 最后一张正交 = 离群
    cluster, medoid, outliers, med_sims = verify.cluster_consensus(vecs)
    assert medoid is not None and medoid in cluster
    assert cluster == {0, 1, 2, 3}, f"主簇应是前 4 张, got {cluster}"
    assert outliers == [4], f"第 5 张应离群, got {outliers}"
    assert med_sims is not None and len(med_sims) == 5, "medoid_sims 供 verify 挑边界图"

    # 全互不相似(< min_size)→ 无共识, medoid=None, 全体逐张判, med_sims=None。
    singl = [unit([1, 0, 0, 0]), unit([0, 1, 0, 0]), unit([0, 0, 1, 0])]
    c2, m2, o2, s2 = verify.cluster_consensus(singl)
    assert m2 is None and c2 == set() and o2 == [0, 1, 2] and s2 is None


def test_verify_anchorfree_gate():
    try:
        import numpy as np
    except ImportError:
        print("SKIP test_verify_anchorfree_gate (no numpy)")
        return
    import supp_verify as verify

    def unit(x):
        x = np.array(x, dtype=np.float32)
        return x / np.linalg.norm(x)

    # 4 张紧簇 + 1 张离群; img_paths 用占位串, gpt_verify 打桩(不联网)。
    vecs = [unit([1, 0.02, 0, 0]), unit([1, 0.01, 0.01, 0]), unit([0.99, 0.03, 0, 0.01]),
            unit([1, 0.0, 0.02, 0]), unit([0, 1, 0, 0])]
    paths = [f"p{i}" for i in range(5)]
    orig = verify.gpt_verify
    try:
        # 全过 → medoid+边界过 → 整簇收 + 离群也逐张过 → 全收; incomplete False。
        verify.gpt_verify = lambda path, *a, **k: (True, {})
        keep, inc = verify.verify_anchorfree(vecs, paths, "X", "", "key", verify.VLMBudget(100), 12)
        assert set(keep) == {0, 1, 2, 3, 4} and inc is False, (keep, inc)

        # 离群 p4 判否 → 簇仍收(medoid+边界都在簇内且过), p4 被拒。
        verify.gpt_verify = lambda path, *a, **k: (path != "p4", {})
        keep, inc = verify.verify_anchorfree(vecs, paths, "X", "", "key", verify.VLMBudget(100), 12)
        assert set(keep) == {0, 1, 2, 3} and inc is False, (keep, inc)

        # medoid 判否 → 不信主簇, 退化逐张; 这里逐张也否 → 一张不收。
        verify.gpt_verify = lambda path, *a, **k: (False, {})
        keep, inc = verify.verify_anchorfree(vecs, paths, "X", "", "key", verify.VLMBudget(100), 12)
        assert keep == [] and inc is False, (keep, inc)

        # 全部 skip(预算耗尽/报错)→ incomplete=True(调用方据此不记 done)。
        verify.gpt_verify = lambda path, *a, **k: (False, {"skip": "budget"})
        keep, inc = verify.verify_anchorfree(vecs, paths, "X", "", "key", verify.VLMBudget(100), 12)
        assert keep == [] and inc is True, (keep, inc)
    finally:
        verify.gpt_verify = orig


if __name__ == "__main__":
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn()
            print(f"PASS {name}")
    print("ALL PASS")
