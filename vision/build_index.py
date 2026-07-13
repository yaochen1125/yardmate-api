"""建库: 把真实照片(每 catalog id 一目录)嵌入进 hnswlib 余弦索引。
P0 铁律: 只用真实照片, 生成图不进判别索引。
用法: build_index.py <data_root> <out_dir>  (data_root 下 AAA* 子目录=库内种; OOC_*/其他跳过)
"""
import sys, os, glob, json
import numpy as np, hnswlib
sys.path.insert(0, os.path.dirname(__file__))
import vision_embed

IN_OUT_THRESHOLD = 0.80  # P0: BioCLIP-2 库内外余弦分界先验(staging 再标定)

def main(data_root, out_dir):
    os.makedirs(out_dir, exist_ok=True)
    dirs = sorted(d for d in os.listdir(data_root)
                  if d.startswith("AAA") and "_" not in d and os.path.isdir(f"{data_root}/{d}"))
    vecs, labels, mapping = [], [], []
    for cid in dirs:
        imgs = sorted(glob.glob(f"{data_root}/{cid}/*.jpg")) + sorted(glob.glob(f"{data_root}/{cid}/*.png"))
        for p in imgs:
            try:
                v = vision_embed.embed_path(p)
            except Exception as e:
                print("skip", p, e); continue
            vecs.append(v); labels.append(len(mapping)); mapping.append({"catalog_id": cid, "photo": os.path.basename(p)})
        print(f"{cid}: {len(imgs)} imgs")
    if not vecs:
        print("NO VECTORS"); return
    X = np.vstack(vecs).astype(np.float32); dim = X.shape[1]
    idx = hnswlib.Index(space="cosine", dim=dim)
    idx.init_index(max_elements=len(X), ef_construction=200, M=32)
    idx.add_items(X, np.array(labels))
    idx.set_ef(64)
    idx.save_index(f"{out_dir}/index.bin")
    json.dump(mapping, open(f"{out_dir}/mapping.json", "w"))
    json.dump({"dim": dim, "count": len(X), "model": vision_embed.MODEL_ID,
               "space": "cosine", "in_out_threshold": IN_OUT_THRESHOLD,
               "catalog_ids": sorted(set(m["catalog_id"] for m in mapping))},
              open(f"{out_dir}/meta.json", "w"), indent=2)
    print(f"INDEX built: {len(X)} vectors, dim {dim}, {len(set(m['catalog_id'] for m in mapping))} catalog ids -> {out_dir}")

if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
