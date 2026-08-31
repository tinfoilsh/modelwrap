#!/usr/bin/env bash
# diskwrite: raw disk write throughput (no network). Writes arbitrary data,
# fsyncs, reports write vs write+sync MiB/s. Results -> $OUT_BASE/diskwrite.tsv
set -euo pipefail
OUT_BASE="${OUT_BASE:-/mnt/large/modelwrap-bench}"
SIZE="${SIZE:-10GiB}"
BS="${BS:-1MiB}"
mkdir -p "$OUT_BASE"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
( cd "$SCRIPT_DIR/.." && go build -o "$OUT_BASE/diskwrite" ./diskwrite )
"$OUT_BASE/diskwrite" --out "$OUT_BASE/diskwrite.data" --size "$SIZE" --bs "$BS" --results "$OUT_BASE/diskwrite.tsv"
echo "results: $OUT_BASE/diskwrite.tsv"
