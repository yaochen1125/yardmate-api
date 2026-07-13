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

# 3) 建索引(幂等:meta.json 最后写,存在=已建成 → 跳;半成品无 meta → 干净重建)。
#    H1: 传播失败码 —— build_index 挂了就 exit 1 让 Restart=on-failure 真生效(原来只 log rc、
#    脚本仍 exit 0,systemd 永不重试 → 无索引、serve 崩溃循环)。
if [ -f /root/yardmate-vision-index/meta.json ]; then
  echo "INDEX_EXISTS_SKIP $(date -u)" >> "$LOG"
else
  if OMP_NUM_THREADS=4 nice -n 10 ./venv/bin/python build_index.py /root/yardmate-vision-ref /root/yardmate-vision-index >> build_index.log 2>&1; then
    echo "BUILD_INDEX_DONE $(date -u)" >> "$LOG"
  else
    echo "BUILD_INDEX_FAILED $(date -u)" >> "$LOG"; exit 1
  fi
fi
echo "PIPELINE_COMPLETE $(date -u)" >> "$LOG"
