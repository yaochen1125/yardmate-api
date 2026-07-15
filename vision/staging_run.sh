#!/usr/bin/env bash
# Phase B(深挖库外图)staging 验证一键跑。★非破坏: 只写 staging 目录(SUPP_DIR + STAGING_SERVE),
# 绝不动 live serve(/root/yardmate-vision-index)—— 验证满意后再另跑全量 + 原子换 live。
#
# 用法(在 box 上): export OPENAI_API_KEY=...; [LIMIT=50] ./staging_run.sh
# 可调 env(都有默认): VISION_DIR INAT_DIR CATALOG SUPP_DIR STAGING_SERVE LIMIT PLANTNET_MAX_IDENTIFY
#   LIMIT               —— 本轮最多处理几株(优先级序: 0-iNat→cultivar→稀薄种)。卡时间窗用; 可续。
#   PLANTNET_MAX_IDENTIFY —— 默认 0(验证阶段不碰线上识别配额); 想测 PlantNet 图源再设 >0。
# 可续: pull_supplemental 按 manifest 跳过已处理株; 中断后重跑接着来。
set -uo pipefail

VISION_DIR=${VISION_DIR:-/root/yardmate-vision}
cd "$VISION_DIR" || { echo "ERR: 进不去 VISION_DIR=$VISION_DIR"; exit 1; }

INAT_DIR=${INAT_DIR:-/root/yardmate-vision-index-inat}      # 纯 iNat 索引(定覆盖 + 锚点 NN)
CATALOG=${CATALOG:-plants_index.json}                       # 当前权威 catalog
SUPP_DIR=${SUPP_DIR:-/root/yardmate-vision-supp}            # 深挖验证图 + manifest 落这
STAGING_SERVE=${STAGING_SERVE:-/root/yardmate-vision-index-staging}  # 折入后的 staging 索引(不是 live)
LIMIT=${LIMIT:-50}
export PLANTNET_MAX_IDENTIFY=${PLANTNET_MAX_IDENTIFY:-0}

PY=./venv/bin/python
ts() { date -u +%H:%M:%S; }

# ── 前置检查(catastrophic 情况早失败, 别白跑) ──
[ -n "${OPENAI_API_KEY:-}" ] || { echo "ERR: 需先 export OPENAI_API_KEY(无锚点株的 GPT 判据); 从 proxy 的 secrets.env.prod 取"; exit 1; }
[ -f "$INAT_DIR/meta.json" ] || { echo "ERR: 找不到纯 iNat 索引 $INAT_DIR/meta.json"; exit 1; }
[ -f "$CATALOG" ]           || { echo "ERR: 找不到 catalog $CATALOG(在 $VISION_DIR 下)"; exit 1; }
$PY -c "import torch,open_clip,hnswlib" 2>/dev/null || { echo "ERR: venv 缺 torch/open_clip/hnswlib"; exit 1; }
if [ "$STAGING_SERVE" = "/root/yardmate-vision-index" ]; then
  echo "ERR: STAGING_SERVE 指到了 live serve 目录 —— 拒绝(本脚本只写 staging)"; exit 1
fi

echo "[$(ts)] Phase B staging  LIMIT=$LIMIT  SUPP=$SUPP_DIR  SERVE=$STAGING_SERVE  PLANTNET_MAX=$PLANTNET_MAX_IDENTIFY"

echo "[$(ts)] 1/3 深挖 + 正确性过滤 (pull_supplemental --limit $LIMIT) — 头 15 分钟看节奏决定够不够 ..."
nice -n 15 $PY pull_supplemental.py "$INAT_DIR" "$CATALOG" "$SUPP_DIR" --limit "$LIMIT" 2>&1 | tee supp.log

echo "[$(ts)] 2/3 折入 staging 索引 (fold_external --supp; 阈值 0.78; 不动 live serve) ..."
nice -n 10 $PY fold_external.py "$INAT_DIR" "$CATALOG" "$STAGING_SERVE" --supp "$SUPP_DIR" 2>&1 | tee fold_staging.log

echo "[$(ts)] 3/3 量增益 (eval_holdout: BEFORE 纯 iNat vs AFTER staging, LOO top-1) ..."
$PY eval_holdout.py "$INAT_DIR" "$STAGING_SERVE" "$SUPP_DIR/_manifest.json" 2>&1 | tee eval.log

echo "[$(ts)] 完成。"
echo "  ▸ 增益看上面 eval_holdout: AFTER 明显高于 BEFORE = Phase B 生效(0-iNat/品种株 BEFORE 应接近 0)。"
echo "  ▸ 人工抽查: 翻 $SUPP_DIR/_spotcheck.json 列的株, 眼看收下的图对不对(品种没混母种、无地图/图解)。"
echo "  ▸ 满意 → 另找整块时间去掉 --limit 跑全量 + 原子换 live; 不满意 → 看 eval.log 最弱株 / 调阈值。"
