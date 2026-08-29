package modelwrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Fixed parameters shared by the golden vectors and the differential
// tests. The golden root hashes below were produced by the pinned
// veritysetup 2.7.5 with exactly these parameters.
var (
	testVeritySalt = func() []byte {
		salt := make([]byte, VeritySaltBytes)
		for i := range salt {
			salt[i] = byte(i)
		}
		return salt
	}()
	testVerityUUID = "9701bf21-6de2-59a2-bdea-141aaae05fc3"
)

// writePatternFile writes n bytes of the fixed pattern used to record the
// golden vectors (byte i is (i*131+7) mod 256).
func writePatternFile(t *testing.T, path string, n int64) {
	t.Helper()
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = byte(i*131 + 7)
	}
	writeChunks(t, path, n, func(buf []byte) {
		for w := buf; len(w) > 0; w = w[copy(w, chunk):] {
		}
	})
}

// writeRandomFile writes n bytes of seeded pseudo-random data.
func writeRandomFile(t *testing.T, path string, n, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	writeChunks(t, path, n, func(buf []byte) {
		for i := 0; i+8 <= len(buf); i += 8 {
			binary.LittleEndian.PutUint64(buf[i:], rng.Uint64())
		}
		for i := len(buf) &^ 7; i < len(buf); i++ {
			buf[i] = byte(rng.Uint64())
		}
	})
}

func writeChunks(t *testing.T, path string, n int64, fill func([]byte)) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 4*1024*1024)
	for n > 0 {
		chunk := buf[:min(n, int64(len(buf)))]
		fill(chunk)
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
		n -= int64(len(chunk))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func formatParams(dataBlocks uint64, workers int) VerityFormatParams {
	return VerityFormatParams{
		Salt:       testVeritySalt,
		UUID:       testVerityUUID,
		DataBlocks: dataBlocks,
		HashOffset: dataBlocks * VerityDataBlockSize,
		Workers:    workers,
	}
}

// TestFormatVerityHashTreeGolden pins the Go builder to root hashes
// recorded from the pinned veritysetup (2.7.5) for tree shapes crossing
// every structural boundary: no tree (1 block), one partial leaf block,
// exactly one full leaf block, one past it, exactly two full levels, and
// three levels. It runs everywhere, without veritysetup installed.
func TestFormatVerityHashTreeGolden(t *testing.T) {
	golden := []struct {
		blocks uint64
		root   string
	}{
		{1, "f2d0775a0fd4dfb0cf94beff32c480402d3075ccc0527687926da13d939f572e"},
		{2, "f8d72cfd06a61471c226caa84afe15e2d1e16d10b89c5ff6d995798bebcd9ce4"},
		{128, "b8c5526364d5373c3392b8d1cb4ab2dc5860ae043d9c2c85e1d76fb50b799a06"},
		{129, "d6fa40ecac97c059df45e548038fc27f6dfad0208b89b54fce783b1073741111"},
		{16384, "b5a614a2807f870e3f4eb25c80df482cd1aff5ab9a6871d7e7854722a3750956"},
		{16385, "914391908342093b8a8cc1f764fe4b084b718b12aed4dc2754844d13988090fe"},
	}
	for _, g := range golden {
		if testing.Short() && g.blocks > 1000 {
			continue
		}
		img := filepath.Join(t.TempDir(), "img")
		writePatternFile(t, img, int64(g.blocks)*VerityDataBlockSize)
		root, err := FormatVerityHashTree(img, formatParams(g.blocks, 0))
		if err != nil {
			t.Fatalf("%d blocks: %v", g.blocks, err)
		}
		if got := hex.EncodeToString(root); got != g.root {
			t.Errorf("%d blocks: root hash = %s, want %s", g.blocks, got, g.root)
		}
		// The superblock's data_blocks field (offset 72) and the salt
		// length field (offset 80) are part of the pinned layout.
		sb := make([]byte, 128)
		f, err := os.Open(img)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.ReadAt(sb, int64(g.blocks)*VerityDataBlockSize); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if got := binary.LittleEndian.Uint64(sb[72:80]); got != g.blocks {
			t.Errorf("%d blocks: superblock data_blocks = %d", g.blocks, got)
		}
		if got := binary.LittleEndian.Uint16(sb[80:82]); got != VeritySaltBytes {
			t.Errorf("%d blocks: superblock salt_size = %d", g.blocks, got)
		}
	}
}

// TestFormatVerityHashTreeDeterminism checks that the output is identical
// regardless of worker count.
func TestFormatVerityHashTreeDeterminism(t *testing.T) {
	const blocks = 1000
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	writeRandomFile(t, data, blocks*VerityDataBlockSize, 42)

	var wantRoot []byte
	var wantSum [32]byte
	for i, workers := range []int{1, 3, 16} {
		img := filepath.Join(dir, "img"+strconv.Itoa(workers))
		copyFile(t, data, img)
		root, err := FormatVerityHashTree(img, formatParams(blocks, workers))
		if err != nil {
			t.Fatal(err)
		}
		sum := fileSum(t, img)
		if i == 0 {
			wantRoot, wantSum = root, sum
			continue
		}
		if !bytes.Equal(root, wantRoot) || sum != wantSum {
			t.Errorf("workers=%d produced different output", workers)
		}
	}
}

func TestFormatVerityHashTreeErrors(t *testing.T) {
	img := filepath.Join(t.TempDir(), "img")
	writePatternFile(t, img, 2*VerityDataBlockSize)

	for name, p := range map[string]VerityFormatParams{
		"zero data blocks":   {Salt: testVeritySalt, UUID: testVerityUUID, DataBlocks: 0, HashOffset: 4096},
		"unaligned offset":   {Salt: testVeritySalt, UUID: testVerityUUID, DataBlocks: 1, HashOffset: 4097},
		"overlapping tree":   {Salt: testVeritySalt, UUID: testVerityUUID, DataBlocks: 2, HashOffset: 4096},
		"oversized salt":     {Salt: make([]byte, 257), UUID: testVerityUUID, DataBlocks: 1, HashOffset: 4096},
		"bad uuid":           {Salt: testVeritySalt, UUID: "not-a-uuid", DataBlocks: 1, HashOffset: 4096},
		"misaligned 4095ish": {Salt: testVeritySalt, UUID: testVerityUUID, DataBlocks: 4095 / VerityDataBlockSize, HashOffset: 4096},
	} {
		if _, err := FormatVerityHashTree(img, p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestFormatVerityHashTreeDifferential formats identical inputs with the
// real veritysetup (invoked exactly as the packer used to invoke it) and
// with the Go builder, and requires byte-identical results: same root
// hash, same file size, same bytes everywhere including the superblock
// and hash tree. It also runs `veritysetup verify` over the Go output
// through the strict --no-superblock consumer parameters. Skipped when
// veritysetup is not installed (run it inside the packer container).
func TestFormatVerityHashTreeDifferential(t *testing.T) {
	if _, err := exec.LookPath("veritysetup"); err != nil {
		t.Skip("veritysetup not installed")
	}

	blockCases := []uint64{1, 2, 127, 128, 129, 130, 16384, 16385}
	rng := rand.New(rand.NewSource(7))
	for range 3 {
		blockCases = append(blockCases, 2+uint64(rng.Intn(20000)))
	}
	if !testing.Short() {
		// ~300 MiB and ~1.5 GiB: multi-level trees at realistic scale.
		blockCases = append(blockCases, 76800, 392003)
	}

	for _, blocks := range blockCases {
		t.Run(fmt.Sprintf("%dblocks", blocks), func(t *testing.T) {
			dir := t.TempDir()
			ref := filepath.Join(dir, "ref.img")
			got := filepath.Join(dir, "got.img")
			size := int64(blocks) * VerityDataBlockSize
			writeRandomFile(t, ref, size, int64(blocks))
			copyFile(t, ref, got)

			infoFile := filepath.Join(dir, "info")
			runVeritysetupFormat(t, ref, infoFile, uint64(size))
			wantRoot, err := os.ReadFile(infoFile)
			if err != nil {
				t.Fatal(err)
			}

			root, err := FormatVerityHashTree(got, formatParams(blocks, 0))
			if err != nil {
				t.Fatal(err)
			}
			if g := hex.EncodeToString(root); g != string(wantRoot) {
				t.Fatalf("root hash = %s, veritysetup wrote %q", g, wantRoot)
			}
			assertFilesIdentical(t, ref, got)

			// The strict consumer path must accept the Go output.
			params, err := VerityParamsForArtifact(uint64(size), testVeritySalt)
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{"verify", got, got, string(wantRoot)}, params.VeritysetupArgs()...)
			if out, err := exec.Command("veritysetup", args...).CombinedOutput(); err != nil {
				t.Fatalf("veritysetup verify of Go output: %v\n%s", err, out)
			}
		})
	}

	// A data device whose size is not a block multiple: veritysetup
	// ignores the trailing partial block (the packer rejects this shape
	// before formatting, but the builder must still match).
	t.Run("4097bytes", func(t *testing.T) {
		dir := t.TempDir()
		ref := filepath.Join(dir, "ref.img")
		got := filepath.Join(dir, "got.img")
		writeRandomFile(t, ref, 4097, 4097)
		copyFile(t, ref, got)

		infoFile := filepath.Join(dir, "info")
		runVeritysetupFormat(t, ref, infoFile, 8192)
		wantRoot, err := os.ReadFile(infoFile)
		if err != nil {
			t.Fatal(err)
		}
		root, err := FormatVerityHashTree(got, VerityFormatParams{
			Salt: testVeritySalt, UUID: testVerityUUID, DataBlocks: 1, HashOffset: 8192,
		})
		if err != nil {
			t.Fatal(err)
		}
		if g := hex.EncodeToString(root); g != string(wantRoot) {
			t.Fatalf("root hash = %s, veritysetup wrote %q", g, wantRoot)
		}
		assertFilesIdentical(t, ref, got)
	})
}

// runVeritysetupFormat invokes veritysetup exactly as wrap.formatVerity
// did before the native builder replaced it.
func runVeritysetupFormat(t *testing.T, img, infoFile string, hashOffset uint64) {
	t.Helper()
	out, err := exec.Command(
		"veritysetup",
		fmt.Sprintf("--format=%d", VerityFormat),
		"--hash="+VerityHashAlgorithm,
		fmt.Sprintf("--data-block-size=%d", VerityDataBlockSize),
		fmt.Sprintf("--hash-block-size=%d", VerityHashBlockSize),
		"--salt="+hex.EncodeToString(testVeritySalt),
		"--uuid="+testVerityUUID,
		fmt.Sprintf("--hash-offset=%d", hashOffset),
		"--root-hash-file="+infoFile,
		"format",
		img,
		img,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("veritysetup format: %v\n%s", err, out)
	}
}

func assertFilesIdentical(t *testing.T, ref, got string) {
	t.Helper()
	fa, err := os.Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	defer fa.Close()
	fb, err := os.Open(got)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Close()

	ba := make([]byte, 4*1024*1024)
	bb := make([]byte, 4*1024*1024)
	var offset int64
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if !bytes.Equal(ba[:na], bb[:nb]) {
			diff := offset
			for i := 0; i < min(na, nb); i++ {
				if ba[i] != bb[i] {
					diff += int64(i)
					break
				}
			}
			t.Fatalf("files differ at byte %d (sizes %d vs %d read so far)", diff, offset+int64(na), offset+int64(nb))
		}
		offset += int64(na)
		if ea != nil || eb != nil {
			if (ea == io.EOF || ea == io.ErrUnexpectedEOF) && (eb == io.EOF || eb == io.ErrUnexpectedEOF) {
				if na != nb {
					t.Fatalf("file sizes differ: %d vs %d", offset, offset-int64(na)+int64(nb))
				}
				return
			}
			t.Fatalf("read: %v / %v", ea, eb)
		}
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// BenchmarkFormatVerityHashTree measures the native builder. Size and
// worker count are configurable:
//
//	MODELWRAP_BENCH_BYTES=8589934592 MODELWRAP_BENCH_WORKERS=8 \
//	    go test -bench FormatVerityHashTree -benchtime 1x
func BenchmarkFormatVerityHashTree(b *testing.B) {
	size := int64(256 << 20)
	if env := os.Getenv("MODELWRAP_BENCH_BYTES"); env != "" {
		n, err := strconv.ParseInt(env, 10, 64)
		if err != nil {
			b.Fatal(err)
		}
		size = n / VerityDataBlockSize * VerityDataBlockSize
	}
	workers := 0
	if env := os.Getenv("MODELWRAP_BENCH_WORKERS"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil {
			b.Fatal(err)
		}
		workers = n
	}

	img := filepath.Join(b.TempDir(), "img")
	writeRandomBenchFile(b, img, size)
	blocks := uint64(size) / VerityDataBlockSize

	b.SetBytes(size)
	b.ResetTimer()
	for b.Loop() {
		if _, err := FormatVerityHashTree(img, formatParams(blocks, workers)); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := os.Truncate(img, size); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func writeRandomBenchFile(b *testing.B, path string, n int64) {
	b.Helper()
	rng := rand.New(rand.NewSource(1))
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 4*1024*1024)
	for n > 0 {
		chunk := buf[:min(n, int64(len(buf)))]
		for i := 0; i+8 <= len(chunk); i += 8 {
			binary.LittleEndian.PutUint64(chunk[i:], rng.Uint64())
		}
		if _, err := f.Write(chunk); err != nil {
			b.Fatal(err)
		}
		n -= int64(len(chunk))
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
}
