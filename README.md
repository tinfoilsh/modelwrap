# Modelwrap

Builds reproducible dm-verity EROFS images of Hugging Face models. Learn more about how this works on the [Tinfoil blog](https://tinfoil.sh/blog/2026-02-03-proving-model-identity).

## Build image

```bash
docker run --rm -it \
  -v $(pwd)/cache:/cache \
  -v $(pwd)/output:/output \
  -e HF_TOKEN="${HF_TOKEN}" \
  -e MODEL=meta-llama/Llama-3.2-1B@4e20de362430cd3b72f300e6b0f18e50e7166e08 \
  ghcr.io/tinfoilsh/modelwrap@sha256:<digest>
```

Notes:
- `MODEL` should include an explicit `@revision` (commit hash) for reproducible builds.
- If `@revision` is omitted, modelwrap resolves the current HEAD commit, which may change over time.
- Set `HF_TOKEN` when accessing gated or private Hugging Face models.
- Pass `--verify` to run `veritysetup verify` after packing or reusing cached output.
- Use a digest-pinned container image, as shown above, when invoking modelwrap in production.

To verify during the same run:

```bash
docker run --rm -it \
  -v $(pwd)/cache:/cache \
  -v $(pwd)/output:/output \
  -e HF_TOKEN="${HF_TOKEN}" \
  -e MODEL=meta-llama/Llama-3.2-1B@4e20de362430cd3b72f300e6b0f18e50e7166e08 \
  ghcr.io/tinfoilsh/modelwrap@sha256:<digest> \
  --verify
```

`modelwrap` emits two files in the output directory:

- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.mpk`: dm-verity EROFS image
- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.info`: metadata file in the format `ROOTHASH_OFFSET_VERITYUUID`

## Supply Chain Pins

The published image is built from a digest-pinned official Python image on Debian Trixie, installs `erofs-utils` and `cryptsetup` from a dated `snapshot.debian.org` archive, and installs Python dependencies from a hash-checked `requirements.txt`.

The packer currently pins:

- `erofs-utils=1.8.6-1`
- `cryptsetup=2:2.7.5-2`

`pack.py` passes the dm-verity hash algorithm, format, and block sizes explicitly so tool default changes do not silently alter the dm-verity format.

To update Python dependencies, edit `requirements.in` and regenerate the lockfile:

```bash
python3 -m piptools compile --generate-hashes --output-file requirements.txt requirements.in
```
