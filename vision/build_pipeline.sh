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

# 3.5) Phase B(深挖库外图): 给欠覆盖株(尤其 0-iNat/cultivar)主动多抓真实库外图 + 正确性过滤,
#    验证过的图落 SUPP_DIR, 供 step 4 折入。★默认关(VISION_SUPP_ENABLED=1 才自动跑)—— 这步花
#    GPT 调用($) + PlantNet 配额 + 数小时, 该由操作者先在 staging/副本手动跑并验证(见 pull_supplemental
#    + eval_holdout), 不宜挂在 systemd Restart 上反复触发。关掉时 step 4 仍会吸收 SUPP_DIR 里已有的
#    (手动跑出的)验证图 —— fold 是幂等的。pull_supplemental 自身可续(manifest 记已处理株)。
SUPP_DIR=/root/yardmate-vision-supp
if [ "${VISION_SUPP_ENABLED:-0}" = "1" ]; then
  # 需要 OPENAI_API_KEY(无锚点株的 GPT 判据); 可选 PLANTNET_API_KEY。由 EnvironmentFile 注入(见 README)。
  if nice -n 15 ./venv/bin/python pull_supplemental.py "$INAT_DIR" plants_index.json "$SUPP_DIR" >> supp.log 2>&1; then
    echo "SUPP_PULL_DONE $(date -u)" >> "$LOG"
  else
    echo "SUPP_PULL_SOFTFAIL $(date -u) (深挖是增强, 不阻断建库)" >> "$LOG"   # 软失败: 不 exit 1
  fi
fi

# 4) 折入 catalog 真实 external gallery 图(P2b): 欠覆盖株补真图 + 视觉独特品种删母种误标向量
#    换正确 external(见 fold_external.py)。INAT_DIR → SERVE_DIR。serve 指向 SERVE_DIR。
#    --supp: 额外折入 Phase B 深挖并已验证的库外图(SUPP_DIR, 见 step 3.5)。
#    plants_index.json = 当前权威 catalog(做 catalog 门 + 品种判定), 与 pull_reference 同一份。
#    ★不做 mtime 幂等跳过(Codex #109): fold 每次都从 INAT_DIR 重建(不在已折索引上再折 → 重跑不
#    重复加向量), 这样 R2 新增/变更的 external 图、catalog 门变化都能被最新一次 fold 吸收(只比
#    INAT 时间戳会漏掉这些)。build_index 的重活(数小时)仍靠 INAT_DIR/meta 幂等跳过; fold 是分钟级
#    (rclone 列 external + 只嵌欠覆盖/品种株的图), 每轮重跑代价可接受。
if nice -n 10 ./venv/bin/python fold_external.py "$INAT_DIR" plants_index.json "$SERVE_DIR" --supp "$SUPP_DIR" >> fold_external.log 2>&1; then
  echo "FOLD_DONE $(date -u)" >> "$LOG"
else
  echo "FOLD_FAILED $(date -u)" >> "$LOG"; exit 1
fi
echo "PIPELINE_COMPLETE $(date -u)" >> "$LOG"
