// Package crypt implements the EMWP dm-crypt encryption (aes-xts-plain64,
// 512-bit key, 4096-byte sectors) in pure Go.
//
// The output is byte-identical to `cryptsetup open --type plain` with the
// parameters the packer uses, so the kernel dm-crypt consumer decrypts
// modelwrap artifacts unchanged. Producing the ciphertext in userspace lets
// the packer encrypt EMWP artifacts without cryptsetup, loop devices,
// device-mapper, or a privileged container, and without writing the volume
// key to disk.
//
// Only the IV-per-sector convention is dm-crypt specific; everything else
// (the two-key split, the tweak derivation, the little-endian sector
// encoding) is standard XTS as implemented by golang.org/x/crypto/xts. That
// one convention is pinned by the dm-crypt golden-vector test.
package crypt

import (
	"crypto/aes"
	"fmt"
	"io"

	"golang.org/x/crypto/xts"

	"github.com/tinfoilsh/modelwrap"
)

// SectorSize is the dm-crypt data-unit size (cryptsetup --sector-size). Each
// SectorSize-byte block is encrypted as one XTS data unit: the tweak is
// derived once from the sector's IV and chained by the GF(2^128) multiply
// across the block's 16-byte AES blocks.
const SectorSize = modelwrap.EMWPSectorSize

// ivSectorRatio converts a SectorSize-byte sector index into the 512-byte
// sector number that plain64 uses for the IV. dm-crypt keeps 512-byte IV
// numbering unless cryptsetup is given --iv-large-sectors, which the packer
// does not use, so each 4096-byte sector advances the IV by 8. This is the
// single dm-crypt specific convention; if TestDmcryptGolden ever fails, this
// constant (8 vs 1) is the first thing to check.
const ivSectorRatio = SectorSize / 512

// streamChunkBytes is the streaming buffer size: 4 MiB, sector-aligned,
// matching the old copyToDevice buffer so large artifacts never load fully
// into memory.
const streamChunkBytes = 1024 * SectorSize

// The packer and consumer always open dm-crypt with skip 0, so the volume's
// first sector is IV unit 0; there is no non-zero-skip path to support.

func newCipher(volumeKey []byte) (*xts.Cipher, error) {
	if len(volumeKey) != modelwrap.EMWPKeyBytes {
		return nil, fmt.Errorf("volume key is %d bytes, want %d", len(volumeKey), modelwrap.EMWPKeyBytes)
	}
	return xts.NewCipher(aes.NewCipher, volumeKey)
}

// transform encrypts or decrypts a sector-aligned buffer in place. baseUnit
// is the 0-based sector index of buf[0] within the volume.
func transform(c *xts.Cipher, buf []byte, baseUnit uint64, decrypt bool) {
	for off := 0; off < len(buf); off += SectorSize {
		iv := (baseUnit + uint64(off/SectorSize)) * ivSectorRatio
		s := buf[off : off+SectorSize]
		if decrypt {
			c.Decrypt(s, s, iv)
		} else {
			c.Encrypt(s, s, iv)
		}
	}
}

// Encrypt encrypts a whole sector-aligned plaintext with the raw 64-byte
// dm-crypt volume key (already derived via modelwrap.DeriveKey). It is a
// convenience for small buffers and tests; the packer uses EncryptStream.
func Encrypt(volumeKey, plaintext []byte) ([]byte, error) {
	return inMemory(volumeKey, plaintext, false)
}

// Decrypt is the inverse of Encrypt.
func Decrypt(volumeKey, ciphertext []byte) ([]byte, error) {
	return inMemory(volumeKey, ciphertext, true)
}

func inMemory(volumeKey, in []byte, decrypt bool) ([]byte, error) {
	if len(in)%SectorSize != 0 {
		return nil, fmt.Errorf("data length %d is not a multiple of sector size %d", len(in), SectorSize)
	}
	c, err := newCipher(volumeKey)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(in))
	copy(out, in)
	transform(c, out, 0, decrypt)
	return out, nil
}

// EncryptStream reads plaintext from src and writes ciphertext to dst,
// encrypting in sector-aligned chunks so large artifacts never load fully
// into memory. It returns the number of ciphertext bytes written (always a
// multiple of SectorSize).
//
// A trailing partial sector is zero-padded. In practice MWP images are always
// sector-aligned (the EROFS image and the dm-verity hash tree are both whole
// multiples of the 4096-byte block), so real artifacts encrypt with no
// padding and the padding path is purely defensive.
func EncryptStream(volumeKey []byte, dst io.Writer, src io.Reader) (int64, error) {
	return stream(volumeKey, dst, src, false)
}

// DecryptStream is the inverse of EncryptStream. Its input must be
// sector-aligned (ciphertext always is); a trailing partial sector is an
// error rather than being padded.
func DecryptStream(volumeKey []byte, dst io.Writer, src io.Reader) (int64, error) {
	return stream(volumeKey, dst, src, true)
}

func stream(volumeKey []byte, dst io.Writer, src io.Reader, decrypt bool) (int64, error) {
	c, err := newCipher(volumeKey)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, streamChunkBytes)
	var baseUnit uint64
	var written int64
	for {
		n, readErr := io.ReadFull(src, buf)

		// A short read from io.ReadFull means either end of stream (io.EOF or
		// io.ErrUnexpectedEOF) or a genuine read error. Only end of stream
		// justifies padding a trailing partial sector; on a real error we must
		// not emit that (incorrectly padded) ciphertext, so surface the error
		// before writing anything.
		eof := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if readErr != nil && !eof {
			return written, readErr
		}

		if n > 0 {
			full := n
			if rem := n % SectorSize; rem != 0 {
				if decrypt {
					return written, fmt.Errorf("ciphertext is not sector-aligned (trailing %d bytes)", rem)
				}
				// A partial sector here implies clean EOF, so the tail is the
				// real end of the data: zero-pad it to a full sector.
				full = n - rem + SectorSize
				clear(buf[n:full])
			}
			transform(c, buf[:full], baseUnit, decrypt)
			if _, err := dst.Write(buf[:full]); err != nil {
				return written, err
			}
			baseUnit += uint64(full / SectorSize)
			written += int64(full)
		}

		if eof {
			return written, nil
		}
	}
}
