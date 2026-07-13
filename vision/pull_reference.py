#!/usr/bin/env python3
"""建库第一步: 给全 catalog 每株从 iNaturalist 拉 research-grade 真实照片。
P0 铁律: 判别索引只用真实照片(生成图会按画风吸走真实 query)。
Resumable / skip-existing / 限流(≤1 req/s) / 记录零覆盖株。只存图算向量, 不再分发。

用法: pull_reference.py <plants_index.json> <out_dir> [per_taxon=40]
plants_index.json: 当前权威 catalog(yardmate-content 或 app Resources), 每项含 id + scientific_name。
"""
import sys, os, re, json, time, urllib.parse, urllib.request

UA = "YardMate-L1-vision/1.0 (reference index build; contact emanon.me@gmail.com)"

def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)

def species_query(sci):
    return re.sub(r"\s*'[^']+'", "", sci).strip()  # 去 cultivar, 种级查询

def resolve(q):
    u = "https://api.inaturalist.org/v1/taxa?" + urllib.parse.urlencode({"q": q, "per_page": 5})
    try: res = get(u)
    except Exception as e: print("  taxa err", q, e); return None
    time.sleep(1.1)
    results = res.get("results", [])
    for want in (("species", "form", "variety", "subspecies"), None):
        for r in results:
            if want is None or r.get("rank") in want:
                return r["id"], r.get("name"), r.get("rank")
    return None

def pull(taxon_id, outdir, per):
    # 干净重拉(Codex #100): 清掉该 taxon 目录已有文件, 避免中断后重跑从 000.jpg
    # 覆盖旧文件 / 残留孤儿。只有未在 manifest 记录的 taxon 才会进到这里(见 main),
    # 故清空是安全的(不会误删已完成株的参考图)。
    if os.path.isdir(outdir):
        for f in os.listdir(outdir):
            try:
                os.remove(os.path.join(outdir, f))
            except OSError:
                pass
    os.makedirs(outdir, exist_ok=True)
    u = "https://api.inaturalist.org/v1/observations?" + urllib.parse.urlencode({
        "taxon_id": taxon_id, "photos": "true", "quality_grade": "research",
        "per_page": per, "order_by": "votes", "order": "desc"})
    try: res = get(u)
    except Exception as e: print("  obs err", e); return []
    time.sleep(1.1)
    man = []
    for obs in res.get("results", []):
        for p in obs.get("photos", [])[:1]:
            purl = p.get("url", "").replace("square", "medium")
            if not purl: continue
            fn = f"{outdir}/{len(man):03d}.jpg"
            try:
                req = urllib.request.Request(purl, headers={"User-Agent": UA})
                with urllib.request.urlopen(req, timeout=30) as r, open(fn, "wb") as f:
                    f.write(r.read())
                man.append({"file": os.path.basename(fn), "photo_id": p.get("id"),
                            "attribution": p.get("attribution"), "license": p.get("license_code")})
                time.sleep(0.25)
            except Exception as e: print("  dl err", e)
            break
    return man

def main(idx_path, out_dir, per=40):
    plants = json.load(open(idx_path))
    os.makedirs(out_dir, exist_ok=True)
    zero_log = open(f"{out_dir}/_zero_coverage.txt", "a")
    manifest_path = f"{out_dir}/_manifest.json"
    manifest = json.load(open(manifest_path)) if os.path.exists(manifest_path) else {}
    done = skipped = zero = 0
    for pl in plants:
        cid, sci = pl["id"], pl["scientific_name"]
        # Resume (Codex #100): manifest 是"已处理"真源 —— 处理过的株(含无 taxon /
        # 零图)都记入并每株持久化,重跑直接跳过,不重拉、不覆盖。文件数是二级兜底
        # (老 manifest 缺失但目录已够时仍跳)。
        if cid in manifest:
            skipped += 1; continue
        outdir = f"{out_dir}/{cid}"
        have = len(os.listdir(outdir)) if os.path.isdir(outdir) else 0
        if have >= min(per, 15):
            skipped += 1; continue
        q = species_query(sci)
        tx = resolve(q)
        if not tx:
            zero += 1; zero_log.write(f"{cid}\t{sci}\tNO_TAXON\n"); zero_log.flush()
            manifest[cid] = {"sci": sci, "resolved": None, "n": 0}
        else:
            tid, tname, trank = tx
            photos = pull(tid, outdir, per)  # pull() 会先清目录, 干净重拉
            if len(photos) == 0:
                zero += 1; zero_log.write(f"{cid}\t{sci}\t{tname}\tNO_PHOTOS\n"); zero_log.flush()
            manifest[cid] = {"sci": sci, "resolved": {"id": tid, "name": tname, "rank": trank}, "n": len(photos)}
        done += 1
        json.dump(manifest, open(manifest_path, "w"), ensure_ascii=False, indent=2)  # 每株持久化 → 断点最多丢 1 株
        if done % 20 == 0:
            print(f"progress: done={done} skipped={skipped} zero={zero} (last {cid})")
    json.dump(manifest, open(manifest_path, "w"), ensure_ascii=False, indent=2)
    print(f"DONE done={done} skipped={skipped} zero_coverage={zero} total={len(plants)}")

if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], int(sys.argv[3]) if len(sys.argv) > 3 else 40)
