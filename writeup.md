# HF CLI vs Naive Go Download Benchmark

## Question

modelwrap downloads models today by shelling out to `hf download` from
`huggingface_hub[hf_xet]` (`wrap/wrap.go`). That pulls Python,
`huggingface_hub`, the `hf_xet` plugin, and their full transitive
dependency tree into the packer container — a sizable supply-chain surface
for a tool whose whole point is reproducibility and trust.

The question: how much download throughput do we actually get from the Xet
stack, and could a stdlib-only Go downloader replace it without giving up
speed?

## Setup

- **Host:** `inf8.tinfoil.sh` — 2.0 TiB RAM, no GPU, Go 1.24.4, downloads
  written to `/mnt/large` (25 TB RAID, 3.2 TB free)
- **Model:** `Qwen/Qwen2.5-72B-Instruct` (revision `main`) — open, Xet-backed,
  47 files, **135.44 GiB** total. Large enough that Xet's chunking/dedup
  has room to help.
- **hf-cli:** `huggingface_hub[hf_xet]` v1.20.1 (`hf-xet` 1.5.1), installed
  in an isolated venv. Xet confirmed active.
- **naive:** `bench/naive/main.go` — Go standard library only (no external
  dependencies, no Python). Lists the repo file tree via the Hub API, then
  GETs each `resolve` URL concurrently (8 workers), one TCP connection per
  file, no byte-range chunking.

## Methodology

Each iteration downloaded the full model to a **fresh directory with a
clean cache**, so both methods measured raw network transfer — not Xet
chunk dedup of already-present data. After each iteration the download was
deleted before the next run.

- 2 iterations per method.
- Throughput = total bytes downloaded / wall time, measured from process
  start to completion.
- Both methods ran unauthenticated (no `HF_TOKEN`), since the model is open.

Harness: `bench/run.sh` (builds the naive binary, creates the HF venv on
first run, loops iterations, records results to `results.tsv`).

## Results

| method | iter | seconds | GiB    | MiB/s  |
| ------ | ---- | ------- | ------ | ------ |
| hf-cli | 1    | 130.8   | 135.44 | 1060.7 |
| hf-cli | 2    | 119.7   | 135.44 | 1158.4 |
| naive  | 1    | 102.9   | 135.44 | 1347.3 |
| naive  | 2    | 75.7    | 135.44 | 1832.1 |

**Average throughput:**

- hf-cli (Xet): **~1110 MiB/s**
- naive Go: **~1590 MiB/s**

The naive Go downloader was faster on **every iteration** — roughly 27–58%
faster depending on the comparison, and ~43% faster on average.

## Notes and caveats

- **Xet was active.** `hf-xet` 1.5.1 was installed in the venv; the HF CLI
  ran with its default Xet-backed transfer path. The naive path used plain
  HTTPS `resolve` redirects to the CDN.
- **Network variance.** The two naive runs (1347 vs 1832 MiB/s) and the two
  hf-cli runs (1061 vs 1158 MiB/s) both show real variance, consistent with
  shared-internet conditions. The ordering (naive > hf-cli) held across all
  runs.
- **naive does less work.** It does not verify SHA256 of LFS blobs (the
  hf CLI does), and it does not do byte-range chunking of large shards.
  Less work, but also less safe on integrity — a tradeoff to call out. If
  integrity matters for the production path, Go could add SHA256 checks
  cheaply without the Python stack.
- **naive uses one connection per file** with bounded concurrency over
  files (8). Xet's main throughput lever is parallel byte-range chunking of
  individual large shards. Despite not doing that, naive still won — likely
  because the CDN serves `resolve` URLs fast enough that per-file
  parallelism saturates the link on a 2 TiB-RAM box with no other contention.
- **Pagination was unused.** The naive downloader includes RFC-8288
  `Link`-header pagination for the tree API, but Qwen 72B has 47 files
  (under the ~1000-file page limit), so it ran a single page. Pagination
  would matter for repos with thousands of files.

## Conclusion

For this model and host, the stdlib-only Go downloader was consistently
faster than the Xet-backed HF CLI while carrying none of its supply-chain
weight (no Python, no `huggingface_hub`, no `hf_xet`, no transitive deps).

That inverts the usual assumption that you need Xet for fast large-model
downloads: here, plain HTTPS `resolve` fetches with file-level
concurrency saturated the available bandwidth more effectively than the
Xet stack did. Dropping the `hf` CLI from modelwrap's packer would shrink
the supply chain without costing throughput — the remaining work would be
adding SHA256 verification (and possibly range requests for very large
single shards) in Go.

## Reproducing

```bash
# on inf8 (or any box with Go + python3 and a writable /mnt/large)
ITERATIONS=2 WORKERS=8 bash bench/run.sh
```

Code lives in `bench/`:

- `bench/naive/main.go` — the naive stdlib-only downloader
- `bench/run.sh` — the benchmark harness
- `bench/README.md` — quick reference
