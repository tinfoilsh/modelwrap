# HF CLI vs Naive Go Download

## Question

modelwrap downloads models by shelling out to `hf download` from
`huggingface_hub[hf_xet]` (`wrap/wrap.go`). That pulls Python,
`huggingface_hub`, the `hf_xet` plugin, and their full transitive deps into
the packer container — a sizable supply-chain surface for a tool whose
point is reproducibility and trust.

Do we actually need the Xet stack for fast large-model downloads, or could
a stdlib-only Go downloader replace it?

## Benchmarks

Three small benchmarks in `bench/`, each its own program + run script, each
writing a TSV you can `rsync` off the bench host:

- `bench/diskwrite` — raw disk write throughput (no network).
- `bench/netread` — raw network download throughput to `io.Discard` (no disk).
- `bench/naive` — the real stdlib downloader, sequential, with per-file
  network and disk timing separated.

Run on `inf8.tinfoil.sh` (downloads to `/mnt/large`):

```bash
OUT_BASE=/mnt/large/modelwrap-bench bash bench/diskwrite/run.sh
OUT_BASE=/mnt/large/modelwrap-bench bash bench/netread/run.sh
OUT_BASE=/mnt/large/modelwrap-bench bash bench/naive/run.sh
```

Results land in `$OUT_BASE/{diskwrite,netread,naive}.tsv`.
