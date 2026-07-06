package modelwrap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// dm-verity format parameters. These are passed explicitly to veritysetup
// so tool default changes never silently alter the artifact format, and
// consumers never read them from the artifact.
const (
	VerityFormat        = 1
	VerityHashAlgorithm = "sha256"
	VerityDataBlockSize = 4096
	VerityHashBlockSize = 4096
	VeritySaltBytes     = 32 // a SHA-256 digest

	maxHashOffset = 1 << 62 // fits int64 with room for the tree offset
)

// VeritySalt returns the deterministic dm-verity salt for a model
// identity string (name@revision): the SHA-256 of the identity. The
// packer uses this when formatting; the consumer re-derives it from the
// attested model identity, so no on-disk metadata is ever consulted.
func VeritySalt(model string) []byte {
	sum := sha256.Sum256([]byte(model))
	return sum[:]
}

// VerityParams are the fully explicit parameters for opening an MWP hash
// tree with `veritysetup --no-superblock`, so that no unverified on-disk
// metadata is ever parsed by the dm-verity stack.
type VerityParams struct {
	Salt           []byte
	DataBlocks     uint64
	HashTreeOffset uint64 // byte offset of the first hash tree block
}

// VeritysetupArgs returns the explicit veritysetup arguments pinning
// every format parameter for a --no-superblock open or verify.
func (p *VerityParams) VeritysetupArgs() []string {
	return []string{
		"--no-superblock",
		fmt.Sprintf("--format=%d", VerityFormat),
		"--hash=" + VerityHashAlgorithm,
		fmt.Sprintf("--data-block-size=%d", VerityDataBlockSize),
		fmt.Sprintf("--hash-block-size=%d", VerityHashBlockSize),
		fmt.Sprintf("--data-blocks=%d", p.DataBlocks),
		fmt.Sprintf("--hash-offset=%d", p.HashTreeOffset),
		"--salt=" + hex.EncodeToString(p.Salt),
	}
}

// VerityParamsForArtifact derives explicit dm-verity parameters from the
// attested hash offset and the salt. In the MWP layout the data area is
// exactly the hashOffset bytes preceding the hash area, so the data size
// is attested rather than read from the artifact, and the hash tree
// begins one hash block past hashOffset (the slot occupied by the
// untrusted, ignored superblock the packer writes there).
func VerityParamsForArtifact(hashOffset uint64, salt []byte) (*VerityParams, error) {
	if len(salt) != VeritySaltBytes {
		return nil, fmt.Errorf("verity salt is %d bytes, want %d", len(salt), VeritySaltBytes)
	}
	if hashOffset == 0 || hashOffset%VerityDataBlockSize != 0 {
		return nil, fmt.Errorf("hash offset %d is not a positive multiple of %d", hashOffset, VerityDataBlockSize)
	}
	if hashOffset > maxHashOffset {
		return nil, fmt.Errorf("hash offset %d exceeds maximum %d", hashOffset, uint64(maxHashOffset))
	}
	return &VerityParams{
		Salt:           salt,
		DataBlocks:     hashOffset / VerityDataBlockSize,
		HashTreeOffset: hashOffset + VerityHashBlockSize,
	}, nil
}
