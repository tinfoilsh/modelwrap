#!/usr/bin/env bash
# End-to-end EMWP regression harness.
#
# Builds modelwrap, packs a tiny local model directory as EMWP, exposes the
# encrypted payload partition through a synthetic PARTUUID path, then runs the
# compiled cvmimage boot mount path against it.
#
# Requires an x86 Linux host with Docker and the dm_verity kernel module
# available.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODELWRAP_DIR="${ROOT}/modelwrap"
CVMIMAGE_DIR="${ROOT}/cvmimage"
IMAGE="${IMAGE:-tinfoil-modelwrap:emwp-test}"
GO_IMAGE="${GO_IMAGE:-golang:1.25-trixie}"
MODEL="${MODEL:-}"
if [[ -z "${GOARCH:-}" ]]; then
  case "$(uname -m)" in
    x86_64) GOARCH=amd64 ;;
    arm64|aarch64) GOARCH=arm64 ;;
    *) echo "unsupported host architecture: $(uname -m); set GOARCH explicitly" >&2; exit 1 ;;
  esac
fi
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/modelwrap-emwp-e2e.XXXXXX")"
KEY_B64="$(python3 - <<'PY'
import base64
print(base64.b64encode(b"k" * 64).decode())
PY
)"

cleanup() {
  rm -rf "${WORK_DIR}" 2>/dev/null || sudo rm -rf "${WORK_DIR}" 2>/dev/null || true
}
trap cleanup EXIT

if [[ "$(uname -s)" == "Linux" ]] && command -v sudo >/dev/null 2>&1; then
  sudo modprobe dm_verity 2>/dev/null || true
fi

docker build -t "${IMAGE}" "${MODELWRAP_DIR}"
docker run --rm \
  -v "${CVMIMAGE_DIR}:/src/cvmimage:ro" \
  -v "${WORK_DIR}:/work" \
  -e GOARCH="${GOARCH}" \
  "${GO_IMAGE}" \
  bash -lc 'cd /src/cvmimage/tinfoil && GOOS=linux /usr/local/go/bin/go test -tags=integration -c ./cmd/boot -o /work/boot.test'

mkdir -p "${WORK_DIR}/cache"
mkdir -p "${WORK_DIR}/model"
mkdir -p "${WORK_DIR}/output"
printf 'hello emwp\n' > "${WORK_DIR}/model/model.txt"
printf '%s\n' "${KEY_B64}" > "${WORK_DIR}/emwp-key"

pack_args=(/app/pack.py --model-dir /model --encrypt --key-file /run/emwp-key --verify)
if [[ -n "${MODEL}" ]]; then
  pack_args=(/app/pack.py "${MODEL}" --model-dir /model --encrypt --key-file /run/emwp-key --verify)
fi

docker run --rm --privileged \
  -v "${WORK_DIR}/cache:/cache" \
  -v "${WORK_DIR}/model:/model:ro" \
  -v "${WORK_DIR}/emwp-key:/run/emwp-key:ro" \
  -v "${WORK_DIR}/output:/output" \
  --entrypoint python3 \
  "${IMAGE}" \
  "${pack_args[@]}"

INFO_FILE="$(find "${WORK_DIR}/output" -name '*.emwp.info' -print -quit)"
EMWP_FILE="${INFO_FILE%.info}"
test -f "${EMWP_FILE}"
test -f "${INFO_FILE}"
REF="$(cat "${INFO_FILE}")"
PARTUUID="${REF##*_}"

docker run --rm --privileged \
  -v "${WORK_DIR}/boot.test:/tmp/boot.test:ro" \
  -v "${EMWP_FILE}:/tmp/model.emwp:ro" \
  -e TINFOIL_EMWP_INTEGRATION=1 \
  -e TINFOIL_EMWP_REF="${REF}" \
  -e TINFOIL_EMWP_KEY_B64="${KEY_B64}" \
  --entrypoint bash \
  "${IMAGE}" \
  -lc '
    set -euo pipefail
    if ! dmsetup targets | awk "{print \$1}" | grep -qx verity; then
      echo "dm-verity device-mapper target is unavailable in this kernel; run this e2e on a Linux host with dm_verity loaded." >&2
      exit 2
    fi

    mkdir -p /mnt/ramdisk/private /mnt/ramdisk/public /dev/disk/by-partuuid
    LOOP="$(losetup --read-only --find --show /tmp/model.emwp)"
    PART_SECTORS="$(( $(blockdev --getsz "${LOOP}") - 2048 - 40 ))"
    dmsetup create emwp-e2e-part --table "0 ${PART_SECTORS} linear ${LOOP} 2048"
    dmsetup mknodes
    trap "umount /mnt/ramdisk/public/mwp/* 2>/dev/null || true; veritysetup close mwp-* 2>/dev/null || true; cryptsetup close emwp-*-crypt 2>/dev/null || true; dmsetup remove emwp-e2e-part 2>/dev/null || true; losetup -d ${LOOP} 2>/dev/null || true" EXIT
    ln -sf /dev/mapper/emwp-e2e-part "/dev/disk/by-partuuid/'"${PARTUUID}"'"

    /tmp/boot.test -test.run TestMountEncryptedModelPackIntegration -test.v
  '
