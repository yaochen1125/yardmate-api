#!/usr/bin/env python3
"""pull_reference 可续性/瞬时失败守卫(Codex #100/#101)。
独立跑,无网络:python3 vision/test_pull_reference.py

核心不变量:瞬时失败(taxa/obs 网络抖动)绝不删已有参考图、绝不把该株标记完成
(否则会永久跳过 + 丢参考集)。真查无 taxon / 真零图 才记入 manifest。
"""
import os, sys, json, tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import pull_reference as pr


def _fresh():
    d = tempfile.mkdtemp()
    idx = os.path.join(d, "idx.json")
    json.dump([{"id": "AAA0001", "scientific_name": "Test one"}], open(idx, "w"))
    out = os.path.join(d, "ref")
    os.makedirs(out + "/AAA0001", exist_ok=True)
    open(out + "/AAA0001/000.jpg", "w").write("x")   # 已有部分参考图(<15, 未在 manifest)
    open(out + "/AAA0001/001.jpg", "w").write("y")
    return idx, out


def _manifest(out):
    p = out + "/_manifest.json"
    return json.load(open(p)) if os.path.exists(p) else {}


def test_transient_resolve_preserves():
    idx, out = _fresh()
    pr.resolve = lambda q: False  # 瞬时 taxa API 失败
    pr.pull = lambda *a: (_ for _ in ()).throw(AssertionError("pull must not run on transient resolve"))
    pr.main(idx, out, 25)
    assert "AAA0001" not in _manifest(out), "transient resolve must not record (retry next run)"
    assert os.path.exists(out + "/AAA0001/000.jpg"), "existing references must be preserved"


def test_transient_obs_preserves():
    idx, out = _fresh()
    pr.resolve = lambda q: (123, "Test", "species")
    pr.pull = lambda *a: None  # obs 成功前的瞬时失败 → None
    pr.main(idx, out, 25)
    assert "AAA0001" not in _manifest(out), "transient obs must not record"
    assert os.path.exists(out + "/AAA0001/000.jpg"), "existing references preserved on transient obs"


def test_success_records():
    idx, out = _fresh()
    pr.resolve = lambda q: (123, "Test", "species")
    pr.pull = lambda tid, od, per: [{"file": "000.jpg"}]
    pr.main(idx, out, 25)
    assert _manifest(out).get("AAA0001", {}).get("n") == 1


def test_genuine_no_taxon_records_zero():
    idx, out = _fresh()
    pr.resolve = lambda q: None  # 真查无 taxon(区别于瞬时 False)
    pr.main(idx, out, 25)
    m = _manifest(out).get("AAA0001", {})
    assert m.get("n") == 0 and m.get("resolved") is None


if __name__ == "__main__":
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            fn()
            print(f"PASS {name}")
    print("ALL PASS")
