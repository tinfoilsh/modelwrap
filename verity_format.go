package modelwrap

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Native dm-verity formatting: builds the superblock and hash tree that
// `veritysetup format` would produce, byte for byte, but hashes data
// blocks in parallel across cores. The root hash is a production attested
// identity, so output equivalence with veritysetup is a hard requirement;
// it is enforced by the differential tests in verity_format_test.go.
const (
	veritySBSaltAreaBytes = 256 // fixed salt field size in the superblock
	verityDigestSize      = sha256.Size
	verityDigestsPerBlock = VerityHashBlockSize / verityDigestSize

	// verityTaskHashBlocks is the number of hash blocks one worker task
	// produces: 16 hash blocks = 2048 data blocks = 8 MiB read per task.
	verityTaskHashBlocks = 16
)

// VerityFormatParams parameterizes one dm-verity format operation over a
// single file laid out as [data blocks][superblock][hash tree], the MWP
// layout. HashOffset is the byte offset of the superblock (the start of
// the hash area) and must leave room for the DataBlocks preceding it.
type VerityFormatParams struct {
	Salt       []byte
	UUID       string
	DataBlocks uint64
	HashOffset uint64
	Workers    int // hashing goroutines; <= 0 means GOMAXPROCS
}

// FormatVerityHashTree writes the dm-verity superblock and hash tree for
// the leading DataBlocks blocks of the file at path and returns the root
// hash. The output is byte-identical to
//
//	veritysetup format <path> <path> --format=1 --hash=sha256
//	    --data-block-size=4096 --hash-block-size=4096
//	    --salt=<salt> --uuid=<uuid> --hash-offset=<hashOffset>
//
// as produced by the pinned veritysetup, including the superblock and the
// zero padding of partial hash blocks, so root hashes of existing
// artifacts are unchanged.
func FormatVerityHashTree(path string, p VerityFormatParams) ([]byte, error) {
	if len(p.Salt) > veritySBSaltAreaBytes {
		return nil, fmt.Errorf("verity salt is %d bytes, exceeds maximum %d", len(p.Salt), veritySBSaltAreaBytes)
	}
	if p.DataBlocks == 0 {
		return nil, fmt.Errorf("verity data area is empty")
	}
	if p.HashOffset%VerityHashBlockSize != 0 {
		return nil, fmt.Errorf("hash offset %d is not a multiple of %d", p.HashOffset, VerityHashBlockSize)
	}
	if p.HashOffset < p.DataBlocks*VerityDataBlockSize {
		return nil, fmt.Errorf("hash offset %d overlaps the %d-block data area", p.HashOffset, p.DataBlocks)
	}
	sb, err := veritySuperblock(&p)
	if err != nil {
		return nil, err
	}
	workers := p.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Tree geometry, matching veritysetup: level 0 holds the digests of
	// the data blocks, each higher level the digests of the level below,
	// up to a single-block top level. On disk the levels are laid out
	// top-down, starting one hash block past the superblock slot.
	var levelBlocks []uint64
	for n := p.DataBlocks; n > 1; n = ceilDiv(n, verityDigestsPerBlock) {
		levelBlocks = append(levelBlocks, ceilDiv(n, verityDigestsPerBlock))
	}
	levelOffset := make([]int64, len(levelBlocks))
	next := int64(p.HashOffset) + VerityHashBlockSize
	for i := len(levelBlocks) - 1; i >= 0; i-- {
		levelOffset[i] = next
		next += int64(levelBlocks[i]) * VerityHashBlockSize
	}

	// The superblock occupies the first hash block; veritysetup pads its
	// block device writes to a full block, so the remaining 3584 bytes
	// are (over)written as zeros.
	if _, err := f.WriteAt(sb, int64(p.HashOffset)); err != nil {
		return nil, err
	}

	srcOff, srcBlocks := int64(0), p.DataBlocks
	for i := range levelBlocks {
		if err := verityHashLevel(f, p.Salt, srcOff, srcBlocks, levelOffset[i], workers); err != nil {
			return nil, err
		}
		srcOff, srcBlocks = levelOffset[i], levelBlocks[i]
	}

	// Root hash: the salted digest of the top-level block, or of the
	// single data block when the tree is empty.
	top := make([]byte, VerityHashBlockSize)
	if _, err := f.ReadAt(top, srcOff); err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(p.Salt)
	h.Write(top)
	root := h.Sum(nil)

	if err := f.Sync(); err != nil {
		return nil, err
	}
	return root, nil
}

// verityHashLevel computes the salted digests of srcBlocks hash-block-size
// blocks starting at srcOff and writes them packed into hash blocks at
// dstOff, zero-padding the final partial block. Worker tasks own disjoint
// destination block ranges, so the output is a pure function of the input
// regardless of scheduling.
func verityHashLevel(f *os.File, salt []byte, srcOff int64, srcBlocks uint64, dstOff int64, workers int) error {
	dstBlocks := ceilDiv(srcBlocks, verityDigestsPerBlock)
	tasks := ceilDiv(dstBlocks, verityTaskHashBlocks)
	if n := int(tasks); workers > n {
		workers = n
	}

	var (
		nextTask atomic.Uint64
		errOnce  sync.Once
		firstErr error
		wg       sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := make([]byte, verityTaskHashBlocks*verityDigestsPerBlock*VerityHashBlockSize)
			out := make([]byte, verityTaskHashBlocks*VerityHashBlockSize)
			h := sha256.New()
			for {
				t := nextTask.Add(1) - 1
				if t >= tasks {
					return
				}
				dstStart := t * verityTaskHashBlocks
				nDst := min(verityTaskHashBlocks, dstBlocks-dstStart)
				srcStart := dstStart * verityDigestsPerBlock
				nSrc := min(nDst*verityDigestsPerBlock, srcBlocks-srcStart)

				src := in[:nSrc*VerityHashBlockSize]
				if _, err := f.ReadAt(src, srcOff+int64(srcStart)*VerityHashBlockSize); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("reading blocks at %d: %w", srcStart, err) })
					return
				}
				// 128 32-byte digests exactly fill a 4096-byte hash
				// block, so packed digests are laid out linearly; only
				// the level's final partial block needs zero padding.
				dst := out[:nDst*VerityHashBlockSize]
				clear(dst[nSrc*verityDigestSize:])
				for i := range nSrc {
					h.Reset()
					h.Write(salt)
					h.Write(src[i*VerityHashBlockSize : (i+1)*VerityHashBlockSize])
					h.Sum(dst[i*verityDigestSize : i*verityDigestSize])
				}
				if _, err := f.WriteAt(dst, dstOff+int64(dstStart)*VerityHashBlockSize); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("writing hash blocks at %d: %w", dstStart, err) })
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// veritySuperblock encodes the little-endian on-disk superblock struct,
// padded to a full hash block.
func veritySuperblock(p *VerityFormatParams) ([]byte, error) {
	u, err := uuid.Parse(p.UUID)
	if err != nil {
		return nil, fmt.Errorf("invalid verity UUID %q: %w", p.UUID, err)
	}
	sb := make([]byte, VerityHashBlockSize)
	copy(sb[0:8], "verity\x00\x00")
	binary.LittleEndian.PutUint32(sb[8:12], 1) // superblock version
	binary.LittleEndian.PutUint32(sb[12:16], VerityFormat)
	copy(sb[16:32], u[:])
	copy(sb[32:64], VerityHashAlgorithm)
	binary.LittleEndian.PutUint32(sb[64:68], VerityDataBlockSize)
	binary.LittleEndian.PutUint32(sb[68:72], VerityHashBlockSize)
	binary.LittleEndian.PutUint64(sb[72:80], p.DataBlocks)
	binary.LittleEndian.PutUint16(sb[80:82], uint16(len(p.Salt)))
	copy(sb[88:88+veritySBSaltAreaBytes], p.Salt)
	return sb, nil
}

func ceilDiv(n, d uint64) uint64 { return (n + d - 1) / d }
