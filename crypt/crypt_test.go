package crypt

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDmcryptGolden is the authoritative compatibility check: it asserts
// crypt.Encrypt reproduces, byte for byte, the ciphertext that the real
// cryptsetup produced for the same key and plaintext with the packer's exact
// flags (see testdata/gen-golden.sh). Passing this proves the kernel
// dm-crypt consumer will decrypt artifacts this package encrypts, and pins
// the one dm-crypt specific convention (the per-sector IV) without needing
// cryptsetup at test time.
func TestDmcryptGolden(t *testing.T) {
	key := readTestdata(t, "key.bin")
	pt := readTestdata(t, "plaintext.bin")
	ct := readTestdata(t, "ciphertext.bin")

	got, err := Encrypt(key, pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.Equal(got, ct) {
		t.Fatalf("ciphertext does not match dm-crypt golden vector;\n"+
			"the per-sector IV convention is likely wrong (try ivSectorRatio=1).\n"+
			"first mismatch at byte %d", firstDiff(got, ct))
	}

	back, err := Decrypt(key, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(back, pt) {
		t.Fatal("Decrypt(golden ciphertext) != plaintext")
	}
}

// TestDmcryptGoldenStream checks the streaming path produces the same golden
// ciphertext and round-trips, including across chunk boundaries.
func TestDmcryptGoldenStream(t *testing.T) {
	key := readTestdata(t, "key.bin")
	pt := readTestdata(t, "plaintext.bin")
	ct := readTestdata(t, "ciphertext.bin")

	var enc bytes.Buffer
	if _, err := EncryptStream(key, &enc, bytes.NewReader(pt)); err != nil {
		t.Fatalf("EncryptStream: %v", err)
	}
	if !bytes.Equal(enc.Bytes(), ct) {
		t.Fatalf("streamed ciphertext != golden (first mismatch at %d)", firstDiff(enc.Bytes(), ct))
	}

	var dec bytes.Buffer
	if _, err := DecryptStream(key, &dec, bytes.NewReader(ct)); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if !bytes.Equal(dec.Bytes(), pt) {
		t.Fatal("DecryptStream(golden) != plaintext")
	}
}

// TestStreamPadsTrailingSector confirms a non-sector-aligned plaintext is
// zero-padded on encrypt and recovered (with padding) on decrypt, matching
// the old backing-file behavior.
func TestStreamPadsTrailingSector(t *testing.T) {
	key := bytes.Repeat([]byte{0x5A}, 64)
	pt := bytes.Repeat([]byte{0xEE}, 2*SectorSize+123) // unaligned

	var enc bytes.Buffer
	n, err := EncryptStream(key, &enc, bytes.NewReader(pt))
	if err != nil {
		t.Fatalf("EncryptStream: %v", err)
	}
	if n != 3*SectorSize || enc.Len() != 3*SectorSize {
		t.Fatalf("padded ciphertext = %d bytes, want %d", enc.Len(), 3*SectorSize)
	}

	var dec bytes.Buffer
	if _, err := DecryptStream(key, &dec, &enc); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if !bytes.Equal(dec.Bytes()[:len(pt)], pt) {
		t.Fatal("decrypted prefix != original plaintext")
	}
	for i := len(pt); i < dec.Len(); i++ {
		if dec.Bytes()[i] != 0 {
			t.Fatalf("pad byte at %d = %d, want 0", i, dec.Bytes()[i])
		}
	}
}

// TestIdenticalSectorsDistinctCiphertext confirms the per-sector IV is
// actually applied: identical plaintext sectors at different offsets must
// encrypt to different ciphertext. testdata's sector 5 duplicates sector 0.
func TestIdenticalSectorsDistinctCiphertext(t *testing.T) {
	key := readTestdata(t, "key.bin")
	pt := readTestdata(t, "plaintext.bin")
	if !bytes.Equal(pt[0:SectorSize], pt[5*SectorSize:6*SectorSize]) {
		t.Skip("testdata sector 5 no longer duplicates sector 0")
	}
	ct, err := Encrypt(key, pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct[0:SectorSize], ct[5*SectorSize:6*SectorSize]) {
		t.Fatal("identical plaintext sectors produced identical ciphertext; IV not per-sector")
	}
}

func TestRejectsBadInput(t *testing.T) {
	good := bytes.Repeat([]byte{1}, 64)
	if _, err := Encrypt(good[:32], make([]byte, SectorSize)); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := Encrypt(good, make([]byte, SectorSize+1)); err == nil {
		t.Fatal("expected error for non-sector-multiple length")
	}
	if _, err := DecryptStream(good, &bytes.Buffer{}, bytes.NewReader(make([]byte, SectorSize+1))); err == nil {
		t.Fatal("expected error decrypting non-sector-aligned ciphertext")
	}
}

// errReader yields data once together with a non-EOF error, mimicking a
// reader that fails mid-stream after a partial, non-sector-aligned read.
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

// TestEncryptStreamReadErrorNoWrite guards against treating a failed partial
// read as end-of-stream: a genuine read error must surface without emitting
// any (zero-padded) ciphertext.
func TestEncryptStreamReadErrorNoWrite(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 64)
	boom := errors.New("boom")
	r := &errReader{data: bytes.Repeat([]byte{0xCD}, 100), err: boom} // 100 % SectorSize != 0

	var dst bytes.Buffer
	n, err := EncryptStream(key, &dst, r)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n != 0 || dst.Len() != 0 {
		t.Fatalf("wrote %d bytes (reported %d) despite read error; want none", dst.Len(), n)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading testdata/%s: %v", name, err)
	}
	return b
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
