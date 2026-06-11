#!/usr/bin/env bash
# Regenerate the dm-crypt golden vector used by TestDmcryptGolden:
#
#   key.bin         64-byte volume key (bytes 0x00..0x3f).
#   plaintext.bin   Six 4096-byte sectors with distinct fill bytes; sector 5
#                   repeats sector 0 so the vector exercises identical
#                   plaintext at distinct IVs.
#   ciphertext.bin  plaintext.bin encrypted by the real cryptsetup with the
#                   exact flags the packer uses (aes-xts-plain64, 512-bit key,
#                   4096-byte sectors, skip 0).
#
# crypt.Encrypt must reproduce ciphertext.bin byte for byte, which pins the
# packer's native-Go XTS to kernel dm-crypt. Everything runs inside one
# privileged container, so the only host requirement is Docker:
#
#   ./crypt/testdata/gen-golden.sh
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

docker run --rm --privileged -i -v "$PWD:/td" debian:trixie-slim bash -s <<'SCRIPT'
set -euo pipefail
S=4096

# emit_sector N writes S copies of the byte value N to stdout.
emit_sector() { printf "\\x$(printf '%02x' "$1")%.0s" $(seq "$S"); }

# key.bin: the 64 bytes 0x00..0x3f.
for i in $(seq 0 63); do printf "\\x$(printf '%02x' "$i")"; done > /td/key.bin

# plaintext.bin: sectors filled with 0xA0..0xA4, then a sixth repeating 0xA0.
{ for v in 160 161 162 163 164; do emit_sector "$v"; done; emit_sector 160; } > /td/plaintext.bin

# ciphertext.bin: encrypt with the real cryptsetup, matching the packer flags.
# The cryptsetup version is not pinned: dm-crypt aes-xts-plain64 output is
# kernel/format-determined and stable across cryptsetup versions.
apt-get update -qq && apt-get install -y -qq cryptsetup-bin >/dev/null
cp /td/plaintext.bin /work_ct
cryptsetup open --type plain --cipher aes-xts-plain64 --key-size 512 \
  --sector-size 4096 --key-file /td/key.bin --skip 0 /work_ct golden
dd if=/td/plaintext.bin of=/dev/mapper/golden bs=4096 conv=notrunc status=none
sync
cryptsetup close golden
cp /work_ct /td/ciphertext.bin
SCRIPT

echo "wrote key.bin ($(wc -c < key.bin)B), plaintext.bin ($(wc -c < plaintext.bin)B), ciphertext.bin ($(wc -c < ciphertext.bin)B)"
