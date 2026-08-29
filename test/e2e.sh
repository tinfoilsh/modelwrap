#!/usr/bin/env bash
# Modelwrap end-to-end test, self-contained to this repository.
#
# Builds the modelwrap image, runs the module's pack->unwrap round-trip
# integration test inside it, then smoke-tests the CLI entrypoint by
# packing and verifying a tiny public model as EMWP.
#
# Requires an x86 Linux host with Docker, network access, and the
# dm_verity kernel module available. The consumer-side test for cvmimage
# lives in the cvmimage repository (emwp-e2e.sh).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${IMAGE:-tinfoil-modelwrap:e2e}"
GO_IMAGE="${GO_IMAGE:-golang:1.25-trixie}"
MODEL="${MODEL:-hf-internal-testing/tiny-random-GPT2Model@d6694b0d8fe17978761c9305dc151780506b192e}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/modelwrap-e2e.XXXXXX")"
KEY_B64="$(head -c 64 /dev/zero | tr '\0' 'k' | base64 | tr -d '\n')"

cleanup() {
  rm -rf "${WORK_DIR}" 2>/dev/null || sudo rm -rf "${WORK_DIR}" 2>/dev/null || true
}
trap cleanup EXIT

if [[ "$(uname -s)" == "Linux" ]] && command -v sudo >/dev/null 2>&1; then
  sudo modprobe dm_verity 2>/dev/null || true
fi

docker build -t "${IMAGE}" "${ROOT}"

docker run --rm \
  -v "${ROOT}:/src:ro" \
  -v "${WORK_DIR}:/work" \
  -e GOFLAGS=-buildvcs=false \
  "${GO_IMAGE}" \
  bash -c 'export PATH="${PATH}:/usr/local/go/bin" && cd /src && GOOS=linux go test -tags=integration -c . -o /work/modelwrap.test'

# Protocol round trip plus superblock tamper regression: pack with wrap,
# consume with unwrap, reject/neutralize tampered superblocks. Also the
# verity differential suite: the packer image ships the pinned veritysetup
# the Go hash-tree builder must match byte for byte (-test.short keeps the
# multi-GiB fixtures out of CI; the boundary cases all run). And the
# download-seeding differential: a seeded wrap must be byte-identical to a
# cold wrap.
docker run --rm --privileged \
  -v "${WORK_DIR}/modelwrap.test:/tmp/modelwrap.test:ro" \
  -e TINFOIL_MODELWRAP_INTEGRATION=1 \
  --entrypoint /tmp/modelwrap.test \
  "${IMAGE}" \
  -test.run 'TestEMWPRoundTripIntegration|TestEMWPGoEncryptKernelDecrypt|TestMWPSuperblockTamperIntegration|TestFormatVerityHashTreeDifferential|TestSeededWrapMatchesColdWrap' -test.short -test.v

# CLI smoke test: the user-facing entrypoint with volumes and key file.
# Packing is userspace-only, so it runs unprivileged.
mkdir -p "${WORK_DIR}/cache" "${WORK_DIR}/output"
printf '%s\n' "${KEY_B64}" > "${WORK_DIR}/emwp-key"

docker run --rm \
  -v "${WORK_DIR}/cache:/cache" \
  -v "${WORK_DIR}/emwp-key:/run/emwp-key:ro" \
  -v "${WORK_DIR}/output:/output" \
  "${IMAGE}" \
  --encrypt --key-file /run/emwp-key --verify "${MODEL}"

INFO_FILE="$(find "${WORK_DIR}/output" -name '*.emwp.info' -print -quit)"
test -f "${INFO_FILE}"
test -f "${INFO_FILE%.info}"
echo "modelwrap e2e OK: $(cat "${INFO_FILE}")"
