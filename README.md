# Modelwrap

Builds reproducible dm-verity EROFS images of Hugging Face models. Learn more about how this works on the [Tinfoil blog](https://tinfoil.sh/blog/2026-02-03-proving-model-identity).

## Build image

```bash
docker run --rm -it \
  -v $(pwd)/cache:/cache \
  -v $(pwd)/output:/output \
  -e HF_TOKEN="${HF_TOKEN}" \
  -e MODEL=meta-llama/Llama-3.2-1B@4e20de362430cd3b72f300e6b0f18e50e7166e08 \
  ghcr.io/tinfoilsh/modelwrap
```

Notes:
- `MODEL` should include an explicit `@revision` (commit hash) for reproducible builds.
- If `@revision` is omitted, modelwrap resolves the current HEAD commit, which may change over time.

`modelwrap` emits two files in the output directory:

- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.mpk`: dm-verity EROFS image
- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.info`: metadata file in the format `ROOTHASH_OFFSET_VERITYUUID`
