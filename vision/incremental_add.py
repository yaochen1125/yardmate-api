#!/usr/bin/env python3
"""增量建索引 standalone CLI(Piece 2): 手动给一株跑通增量 add(服务器可独立测)。
自带 BioCLIP-2 模型(torch), 生产触发路径走 serve 的 /admin/knn/add 复用已加载模型省 torch load。

用法: incremental_add.py <index_dir> <catalog_id> [--cdn URL]
  index_dir: serve 索引目录(index.bin/mapping.json/meta.json), 就地原子更新。
  catalog_id: 刚晋升的株 AAA id。
  --cdn: external 图 CDN 前缀, 默认 https://images.yardmate.ai/plant_images。
"""
import sys
import json

sys.path.insert(0, __import__("os").path.dirname(__file__))
import vision_embed
from incremental import add_plant_to_index

DEFAULT_CDN = "https://images.yardmate.ai/plant_images"


def main(argv):
    args, cdn = [], DEFAULT_CDN
    i = 0
    while i < len(argv):
        if argv[i] == "--cdn":
            cdn = argv[i + 1] if i + 1 < len(argv) else cdn
            i += 2
            continue
        args.append(argv[i])
        i += 1
    if len(args) < 2:
        print(__doc__)
        sys.exit(2)
    index_dir, cid = args[0], args[1]
    stats = add_plant_to_index(index_dir, cid, cdn, vision_embed.embed_path)
    print(json.dumps(stats, ensure_ascii=False))
    # added==0 视为软失败(无图可加): 非零退出让调用方(全库 build/触发脚本)能感知。
    sys.exit(0 if stats.get("added", 0) > 0 else 1)


if __name__ == "__main__":
    main(sys.argv[1:])
