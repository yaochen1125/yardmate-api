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
    cluster, medoid, outliers = verify.cluster_consensus(vecs)
    assert medoid is not None and medoid in cluster
    assert cluster == {0, 1, 2, 3}, f"主簇应是前 4 张, got {cluster}"
    assert outliers == [4], f"第 5 张应离群, got {outliers}"

    # 全互不相似(< min_size)→ 无共识, medoid=None, 全体逐张判。
    singl = [unit([1, 0, 0, 0]), unit([0, 1, 0, 0]), unit([0, 0, 1, 0])]
    c2, m2, o2 = verify.cluster_consensus(singl)
    assert m2 is None and c2 == set() and o2 == [0, 1, 2]


if __name__ == "__main__":
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn()
            print(f"PASS {name}")
    print("ALL PASS")
