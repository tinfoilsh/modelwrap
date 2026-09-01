# Modelwrap

Builds reproducible dm-verity EROFS images of Hugging Face models. Learn more about how this works on the [Tinfoil blog](https://tinfoil.sh/blog/2026-02-03-proving-model-identity).

For the artifact format, trust assumptions, and EMWP cryptographic parameters, see [SPEC.md](SPEC.md).

This repository is also the Go module `github.com/tinfoilsh/modelwrap`. The root package defines the protocol surface shared by both sides (format constants, artifact reference parsing, EMWP key derivation), `wrap` implements the packer, and `unwrap` implements the consumer side (used by [cvmimage](https://github.com/tinfoilsh/cvmimage) to mount model packs at boot).

## Usage

The `modelwrap` CLI is a launcher: it runs the packing inside a
digest-pinned container image (requires docker), so artifact bytes are
always produced by the pinned toolchain. Release binaries embed the
matching image digest.

```bash
modelwrap mistralai/Ministral-3-3B-Instruct-2512@cfcb068fa7c44114cf77a462357c6cdcd2c304b4
```

Delete the generated artifacts and downloaded cache for one pinned revision:

```sh
modelwrap --delete mistralai/Ministral-3-3B-Instruct-2512@cfcb068fa7c44114cf77a462357c6cdcd2c304b4
```

To pack and encrypt a local/private model directory:

```bash
PRIVATE_MODEL_KEY_B64="${PRIVATE_MODEL_KEY_B64}" modelwrap --model-dir /path/to/model --encrypt
```

## Arguments

- `model`: Hugging Face model ID, preferably with `@revision`. If omitted with `--model-dir`, modelwrap derives `basename@contentHash`.
- `--model-dir <path>`: pack a local/private model directory instead of downloading from Hugging Face. If `model` is provided without `@revision`, modelwrap uses the directory content hash as the revision.
- `--encrypt`: emit encrypted modelwrap output (`.emwp`). Requires a master key via `--key-file` or `PRIVATE_MODEL_KEY_B64`.
- `--key-file <path>`: file containing the base64-encoded 64-byte EMWP master key.
- `--verify`: optional. Runs `veritysetup verify` for MWP and decrypts then verifies EMWP, which is useful for cached artifacts or release checks.
- `--schema <N>`: pack schema, the frozen derivation from model bytes to image bytes (also `MODELWRAP_SCHEMA`; absent = default schema). See the pack schemas section of `SPEC.md`.
- `--output <path>` / `--cache <path>`: output and download cache directories (default `./output`, `./cache`).
- `--image <ref>`: override the packer container image the launcher runs (defaults to the release-pinned digest; also `MODELWRAP_IMAGE`).
- `--local`: run the packer directly on the current machine instead of in a container. Requires the schema's pinned `mkfs.erofs` at its `/opt/modelwrap/schemas/` path (the packer refuses a PATH fallback: the schema names an exact toolchain).

Set `HF_TOKEN` when accessing gated or private Hugging Face models; the launcher passes it into the container without exposing the value on the docker command line.

MWP mode emits:

- `output/mistralai/Ministral-3-3B-Instruct-2512/cfcb068fa7c44114cf77a462357c6cdcd2c304b4.mpk`: dm-verity EROFS image
- `output/mistralai/Ministral-3-3B-Instruct-2512/cfcb068fa7c44114cf77a462357c6cdcd2c304b4.info`: metadata file in the format `ROOTHASH_OFFSET_VERITYUUID`
- `output/mistralai/Ministral-3-3B-Instruct-2512/cfcb068fa7c44114cf77a462357c6cdcd2c304b4.schema`: pack schema id the artifacts were produced under (absent = schema 1)

EMWP mode additionally emits:

- `output/mistralai/Ministral-3-3B-Instruct-2512/cfcb068fa7c44114cf77a462357c6cdcd2c304b4.emwp`: disk image with one encrypted payload partition
- `output/mistralai/Ministral-3-3B-Instruct-2512/cfcb068fa7c44114cf77a462357c6cdcd2c304b4.emwp.info`: metadata file in the format `ROOTHASH_OFFSET_PARTUUID`

## Supply Chain Pins

The published image is built in two digest-pinned stages: a Go builder that compiles the `modelwrap` binary (`CGO_ENABLED=0`, `-trimpath`, hash-locked Go dependencies via `go.sum`), and a runtime image based on the official Python image on Debian Trixie that installs `cryptsetup` and `gdisk` from a dated `snapshot.debian.org` archive plus a hash-checked `requirements.txt` (only `huggingface_hub`, which provides the `hf` CLI used for downloads).

The packer currently pins:

- per-schema `mkfs.erofs` builds vendored under `/opt/modelwrap/schemas/` from sha256-pinned debs of the dated snapshot archive: schema 1 = `erofs-utils 1.5-1`, schema 2 = `erofs-utils 1.8.6-1` (multithreaded)
- `cryptsetup=2:2.7.5-2`
- `gdisk=1.0.10-2`

The packer passes the dm-verity hash algorithm, format, and block sizes explicitly so tool default changes do not silently alter the dm-verity format.

To update Python dependencies, edit `requirements.in` and regenerate the lockfile:

```bash
python3 -m piptools compile --generate-hashes --output-file requirements.txt requirements.in
```
