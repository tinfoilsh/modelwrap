#!/usr/bin/env bash
# Benchmark: HF CLI (Xet) vs naive Go HTTP download of a Hugging Face model.
#
# Measures raw download throughput for the two approaches modelwrap could use:
#   1. hf download from huggingface_hub[hf_xet] — the current approach
#   2. ./naive — a stdlib-only Go HTTP downloader (no Python, no Xet)
#
# Each iteration downloads to a fresh directory with a clean cache, so we
# measure network transfer, not Xet dedup of already-present chunks.
set -euo pipefail

MODEL="${MODEL:-Qwen/Qwen2.5-72B-Instruct}"
REVISION="${REVISION:-main}"
ITERATIONS="${ITERATIONS:-2}"
WORKERS="${WORKERS:-8}"
OUT_BASE="${OUT_BASE:-/mnt/large/modelwrap-bench}"
HF_VENV="${HF_VENV:-$HOME/.hf-venv}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mkdir -p "$OUT_BASE"
RESULTS="$OUT_BASE/results.tsv"
printf "method\titer\tseconds\tbytes\tgib\tmib_per_s\n" > "$RESULTS"

now()   { date +%s.%N; }
delta() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.3f", b-a}'; }

record() { # method iter start end dir
	local secs; secs=$(delta "$3" "$4")
	local bytes; bytes=$(du -sb "$5" | cut -f1)
	local gib; gib=$(awk -v b="$bytes" 'BEGIN{printf "%.2f", b/1073741824}')
	local mibps; mibps=$(awk -v b="$bytes" -v s="$secs" 'BEGIN{printf "%.1f", b/1048576/s}')
	printf "%s\t%s\t%s\t%s\t%s\t%s\n" "$1" "$2" "$secs" "$bytes" "$gib" "$mibps" | tee -a "$RESULTS"
}

echo "Building naive downloader..."
( cd "$SCRIPT_DIR" && go build -o "$OUT_BASE/naive" ./naive )

if [ ! -x "$HF_VENV/bin/hf" ]; then
	echo "Creating HF venv at $HF_VENV ..."
	python3 -m venv "$HF_VENV"
	"$HF_VENV/bin/pip" install --upgrade pip
	"$HF_VENV/bin/pip" install "huggingface_hub[hf_xet]"
fi
echo "HF CLI version: $("$HF_VENV/bin/hf" --version)"

for i in $(seq 1 "$ITERATIONS"); do
	out="$OUT_BASE/hf-$i"
	cache="$OUT_BASE/hf-cache-$i"
	rm -rf "$out" "$cache"
	echo -e "\n=== hf download (Xet) iter $i ==="
	s=$(now)
	HF_HOME="$cache" "$HF_VENV/bin/hf" download "$MODEL" --revision "$REVISION" --local-dir "$out"
	e=$(now)
	record hf-cli "$i" "$s" "$e" "$out"
	rm -rf "$out" "$cache"
done

for i in $(seq 1 "$ITERATIONS"); do
	out="$OUT_BASE/naive-$i"
	rm -rf "$out"
	echo -e "\n=== naive Go iter $i ==="
	s=$(now)
	"$OUT_BASE/naive" --repo "$MODEL" --revision "$REVISION" --out "$out" --workers "$WORKERS"
	e=$(now)
	record naive "$i" "$s" "$e" "$out"
	rm -rf "$out"
done

echo -e "\n=== RESULTS ==="
cat "$RESULTS"
