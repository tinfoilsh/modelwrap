# Download Benchmark

Compares two ways to fetch a Hugging Face model, which is the question
behind dropping the `hf` CLI from modelwrap's supply chain:

1. **hf-cli** — `hf download` from `huggingface_hub[hf_xet]`. This is what
   modelwrap does today (`wrap/wrap.go`). Pulls in Python + huggingface_hub
   + the hf_xet plugin and their full transitive dependency tree.
2. **naive** — `bench/naive/main.go`, a stdlib-only Go program. Lists the
   repo tree via the Hub API and GETs each `resolve` URL over plain HTTPS.
   Zero external dependencies, no Python.

Each iteration downloads to a fresh directory with a clean cache, so both
methods measure raw network transfer (no Xet chunk dedup across runs).

## Run

```bash
# on a box with Go + python3, downloads land on /mnt/large
ITERATIONS=2 WORKERS=8 bash bench/run.sh
```

Results are written to `$OUT_BASE/results.tsv` (tab-separated):
`method iter seconds bytes gib mib_per_s`.

## Notes

- **naive** uses one TCP connection per file with bounded concurrency over
  files (default 8). It does not do byte-range chunking of individual large
  shards, which is Xet's main throughput lever. If naive is close, the Xet
  stack isn't worth its supply-chain cost; if not, Go could add range
  requests without the Python dependency.
- **naive** does not verify SHA256 of LFS blobs (the hf CLI does). That is
  less work, but also less safe — a tradeoff to call out.
- Model: `Qwen/Qwen2.5-72B-Instruct` (~145 GiB, open, Xet-backed).
