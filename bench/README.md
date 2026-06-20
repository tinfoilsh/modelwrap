# Download Bench

Three small benchmarks for the question behind dropping the `hf` CLI from
modelwrap: do we need the Xet stack for fast large-model downloads, or
would a stdlib-only Go downloader do?

Each is a standalone Go program with its own run script, and each writes a
TSV of results you can `rsync` off the bench host.

- `diskwrite/` — raw disk write throughput (no network). Writes arbitrary
  data, fsyncs, reports write vs write+sync MiB/s.
- `netread/` — raw network download throughput (no disk). Streams every file
  in a Hugging Face repo to `io.Discard`, reports per-file and total MiB/s.
- `naive/` — the real stdlib-only downloader, sequential, with per-file
  network and disk timing separated. Compare against `netread` (no disk) and
  `diskwrite` (no network) to see where time goes.

All three are Go standard library only — no Python, no `huggingface_hub`,
no `hf_xet`.

## Run

On a box with Go (e.g. `inf8.tinfoil.sh`, downloads to `/mnt/large`):

```bash
OUT_BASE=/mnt/large/modelwrap-bench bash bench/diskwrite/run.sh
OUT_BASE=/mnt/large/modelwrap-bench bash bench/netread/run.sh
OUT_BASE=/mnt/large/modelwrap-bench bash bench/naive/run.sh
```

Results land in `$OUT_BASE/{diskwrite,netread,naive}.tsv` (tab-separated).

`bench/xet_probe.py` is a separate one-off: it detects which download path
`huggingface_hub` actually takes (native Xet CAS vs plain HTTPS) for a file.
