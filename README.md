# Modelwrap

Builds reproducible dm-verity EROFS images of Hugging Face models. Learn more about how this works on the [Tinfoil blog](https://tinfoil.sh/blog/2026-02-03-proving-model-identity).

## Usage

```bash
docker run --rm -it \
  -v $(pwd)/cache:/cache \
  -v $(pwd)/output:/output \
  -e HF_TOKEN="${HF_TOKEN}" \
  ghcr.io/tinfoilsh/modelwrap@sha256:<digest> \
  meta-llama/Llama-3.2-1B@4e20de362430cd3b72f300e6b0f18e50e7166e08
```

To pack and encrypt a local/private model directory:

```bash
docker run --rm -it --privileged \
  -v /path/to/model:/model:ro \
  -v $(pwd)/output:/output \
  -e PRIVATE_MODEL_KEY_B64="${PRIVATE_MODEL_KEY_B64}" \
  ghcr.io/tinfoilsh/modelwrap@sha256:<digest> \
  --model-dir /model \
  --encrypt
```

## Arguments

- `model`: Hugging Face model ID, preferably with `@revision`. If omitted with `--model-dir`, modelwrap derives `basename@contentHash`.
- `--model-dir <path>`: pack a local/private model directory instead of downloading from Hugging Face. If `model` is provided without `@revision`, modelwrap uses the directory content hash as the revision.
- `--encrypt`: emit encrypted modelwrap output (`.emwp`). Requires device-mapper and loop device access; `--privileged` is the simplest Docker setup. Also requires `--key-file`, `PRIVATE_MODEL_KEY_FILE`, or `PRIVATE_MODEL_KEY_B64`.
- `--key-file <path>`: file containing the base64-encoded 64-byte EMWP master key.
- `--verify`: optional. Runs `veritysetup verify` for MWP and decrypts then verifies EMWP, which is useful for cached artifacts or release checks.

Environment fallbacks are supported for wrapper scripts: `MODEL`, `MODEL_DIR`, `VERIFY=1`, and `ENCRYPTION=1`. Set `HF_TOKEN` when accessing gated or private Hugging Face models. Use a digest-pinned container image, as shown above, when invoking modelwrap in production.

MWP mode emits:

- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.mpk`: dm-verity EROFS image
- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.info`: metadata file in the format `ROOTHASH_OFFSET_VERITYUUID`

EMWP mode additionally emits:

- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.emwp`: disk image with one encrypted payload partition
- `output/meta-llama/Llama-3.2-1B/4e20de362430cd3b72f300e6b0f18e50e7166e08.emwp.info`: metadata file in the format `ROOTHASH_OFFSET_PARTUUID`

## Supply Chain Pins

The published image is built from a digest-pinned official Python image on Debian Trixie, installs `erofs-utils` and `cryptsetup` from a dated `snapshot.debian.org` archive, and installs Python dependencies from a hash-checked `requirements.txt`.

The packer currently pins:

- `erofs-utils=1.8.6-1`
- `cryptsetup=2:2.7.5-2`
- `gdisk=1.0.10-2`

`pack.py` passes the dm-verity hash algorithm, format, and block sizes explicitly so tool default changes do not silently alter the dm-verity format.

To update Python dependencies, edit `requirements.in` and regenerate the lockfile:

```bash
python3 -m piptools compile --generate-hashes --output-file requirements.txt requirements.in
```
