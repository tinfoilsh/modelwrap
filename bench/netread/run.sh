#!/usr/bin/env bash
# netread: raw network download throughput (no disk). Streams every file in a
# Hugging Face repo to io.Discard, reports per-file and total MiB/s.
# Results -> $OUT_BASE/netread.tsv
set -euo pipefail
OUT_BASE="${OUT_BASE:-/mnt/large/modelwrap-bench}"
MODEL="${MODEL:-Qwen/Qwen2.5-72B-Instruct}"
REVISION="${REVISION:-main}"
mkdir -p "$OUT_BASE"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
( cd "$SCRIPT_DIR/.." && go build -o "$OUT_BASE/netread" ./netread )
"$OUT_BASE/netread" --repo "$MODEL" --revision "$REVISION" --results "$OUT_BASE/netread.tsv"
echo "results: $OUT_BASE/netread.tsv"
