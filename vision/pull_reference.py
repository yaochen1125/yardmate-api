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
        outdir = f"{out_dir}/{cid}"
        have = len([f for f in os.listdir(outdir)]) if os.path.isdir(outdir) else 0
        if have >= min(per, 15):  # 已够 → 跳(resumable)
            skipped += 1; continue
        q = species_query(sci)
        tx = resolve(q)
        if not tx:
            zero += 1; zero_log.write(f"{cid}\t{sci}\tNO_TAXON\n"); zero_log.flush()
            manifest[cid] = {"sci": sci, "resolved": None, "n": 0}; continue
        tid, tname, trank = tx
        photos = pull(tid, outdir, per)
        if len(photos) == 0:
            zero += 1; zero_log.write(f"{cid}\t{sci}\t{tname}\tNO_PHOTOS\n"); zero_log.flush()
        manifest[cid] = {"sci": sci, "resolved": {"id": tid, "name": tname, "rank": trank}, "n": len(photos)}
        done += 1
        if done % 20 == 0:
            json.dump(manifest, open(manifest_path, "w"), ensure_ascii=False, indent=2)
            print(f"progress: done={done} skipped={skipped} zero={zero} ({cid} {tname}:{len(photos)})")
    json.dump(manifest, open(manifest_path, "w"), ensure_ascii=False, indent=2)
    print(f"DONE done={done} skipped={skipped} zero_coverage={zero} total={len(plants)}")

if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], int(sys.argv[3]) if len(sys.argv) > 3 else 40)
