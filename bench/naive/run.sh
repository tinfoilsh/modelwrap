#!/usr/bin/env bash
# naive: sequential stdlib-only model download to disk, with per-file network
# and disk timing separated. Compare against netread (no disk) and diskwrite
# (no network) to see where time goes. Results -> $OUT_BASE/naive.tsv
#
# SYNC=1 adds an fsync per file (measures real disk, not page cache).
set -euo pipefail
OUT_BASE="${OUT_BASE:-/mnt/large/modelwrap-bench}"
MODEL="${MODEL:-Qwen/Qwen2.5-72B-Instruct}"
REVISION="${REVISION:-main}"
SYNC="${SYNC:-0}"
mkdir -p "$OUT_BASE"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
( cd "$SCRIPT_DIR/.." && go build -o "$OUT_BASE/naive" ./naive )
OUT="$OUT_BASE/naive-out"
rm -rf "$OUT"
args=( --repo "$MODEL" --revision "$REVISION" --out "$OUT" --results "$OUT_BASE/naive.tsv" )
if [ "${SYNC}" = "1" ]; then args+=( --sync ); fi
"$OUT_BASE/naive" "${args[@]}"
echo "results: $OUT_BASE/naive.tsv"
