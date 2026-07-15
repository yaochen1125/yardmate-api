#!/usr/bin/env bash
# 全库参考索引构建监督(可续 + 幂等)。由 systemd(yardmate-vision-build.service)托管:
# 开机自启 + 失败自动重试。pull→embed 一条龙。部署布局见 README(/root/yardmate-vision)。
set -uo pipefail
cd /root/yardmate-vision
LOG=pipeline.log
echo "PIPELINE_START $(date -u)" >> "$LOG"

# 1) 拉图直到全部完成(pull_reference 可续;瞬时失败它内部已 sleep 重试)。
#    M1: 连续失败上限 —— 坏配置(如 plants_index 路径错)不无限空转锤 iNat,到上限 exit 1
#    让 systemd Restart=on-failure 以更长 RestartSec 退避重试并在 status 里暴露。
fails=0
while :; do
  if nice -n 15 python3 -u pull_reference.py plants_index.json /root/yardmate-vision-ref 25 >> pull.log 2>&1; then
    echo "PULL_DONE $(date -u)" >> "$LOG"; break
  fi
  fails=$((fails + 1))
  echo "PULL_RETRY #$fails $(date -u)" >> "$LOG"
  if [ "$fails" -ge 5 ]; then echo "PULL_GIVEUP after $fails fails $(date -u)" >> "$LOG"; exit 1; fi
  sleep 60
done

# 2) 等 venv/torch 就绪。M2: 有限次 —— venv 若"损坏"(非缺失,如 torch/torchvision 不匹配)
#    不会无限空转;到上限 exit 1,让故障以 systemd 失败的形式暴露而不是静默卡住。
tries=0
until ./venv/bin/python -c "import torch,open_clip,hnswlib" 2>/dev/null; do
  tries=$((tries + 1))
  if [ "$tries" -ge 20 ]; then echo "VENV_BROKEN giveup after $tries $(date -u)" >> "$LOG"; exit 1; fi
  sleep 30
done
echo "VENV_READY $(date -u)" >> "$LOG"

# 3) 建 iNat 纯真照索引 → INAT_DIR(幂等:meta.json 最后写,存在=已建成 → 跳;半成品无 meta →
#    干净重建)。H1: 传播失败码 —— build_index 挂了就 exit 1 让 Restart=on-failure 真生效。
#    ★注意: 输出改到 INAT_DIR(不是 serve 目录) —— serve 目录由 step 4 折入后产出。
INAT_DIR=/root/yardmate-vision-index-inat
SERVE_DIR=/root/yardmate-vision-index
if [ -f "$INAT_DIR/meta.json" ]; then
  echo "INDEX_EXISTS_SKIP $(date -u)" >> "$LOG"
else
  if OMP_NUM_THREADS=4 nice -n 10 ./venv/bin/python build_index.py /root/yardmate-vision-ref "$INAT_DIR" >> build_index.log 2>&1; then
    echo "BUILD_INDEX_DONE $(date -u)" >> "$LOG"
  else
    echo "BUILD_INDEX_FAILED $(date -u)" >> "$LOG"; exit 1
  fi
fi

# 4) 折入 catalog 真实 external gallery 图(P2b): 欠覆盖株补真图 + 视觉独特品种删母种误标向量
#    换正确 external(见 fold_external.py)。INAT_DIR → SERVE_DIR。serve 指向 SERVE_DIR。
#    幂等: SERVE 的 meta 比 INAT 新 = 已按最新 iNat 折过 → 跳(避免重复折入/在已折索引上再折)。
#    plants_index.json = 当前权威 catalog(做 catalog 门 + 品种判定), 与 pull_reference 同一份。
if [ -f "$SERVE_DIR/meta.json" ] && [ "$SERVE_DIR/meta.json" -nt "$INAT_DIR/meta.json" ]; then
  echo "FOLD_EXISTS_SKIP $(date -u)" >> "$LOG"
else
  if nice -n 10 ./venv/bin/python fold_external.py "$INAT_DIR" plants_index.json "$SERVE_DIR" >> fold_external.log 2>&1; then
    echo "FOLD_DONE $(date -u)" >> "$LOG"
  else
    echo "FOLD_FAILED $(date -u)" >> "$LOG"; exit 1
  fi
fi
echo "PIPELINE_COMPLETE $(date -u)" >> "$LOG"
