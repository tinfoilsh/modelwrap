// Package modelwrap defines the Modelwrap (MWP) and Encrypted Modelwrap
// (EMWP) artifact format: the cryptographic constants, the artifact
// reference grammar, the deterministic identity derivations, and the key
// derivation used by both the packer and the consumer. See SPEC.md for the
// full format specification.
package modelwrap

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/mod/sumdb/dirhash"
)

// EMWP dm-crypt parameters.
const (
	EMWPCipher         = "aes-xts-plain64"
	EMWPKeySizeBits    = 512
	EMWPKeyBytes       = EMWPKeySizeBits / 8
	EMWPMasterKeyBytes = 64
	EMWPSectorSize     = 4096
	EMWPKeyDeriveInfo  = "tinfoil/emwp/dm-crypt-key/v1"
)

// EMWP GPT disk image geometry. The encrypted payload partition starts at
// a fixed sector so ciphertext placement is deterministic.
const (
	GPTSectorSize            = 512
	EMWPPartitionStartSector = 2048
	EMWPGPTTrailingSectors   = 40
)

var (
	rootHashPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	hashOffsetPattern = regexp.MustCompile(`^[0-9]+$`)
	uuidPattern       = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
)

// ArtifactRef is a parsed artifact reference of the form
// rootHash_hashOffset_uuid. For MWP artifacts the UUID is the dm-verity
// superblock UUID; for EMWP artifacts it is the GPT PARTUUID of the
// encrypted payload partition.
type ArtifactRef struct {
	RootHash   string
	HashOffset string
	UUID       string
}

// ParseRef parses and validates a rootHash_hashOffset_uuid reference.
func ParseRef(ref string) (*ArtifactRef, error) {
	parts := strings.Split(ref, "_")
	if len(parts) != 3 {
		return nil, fmt.Errorf("expected rootHash_hashOffset_uuid")
	}

	r := &ArtifactRef{
		RootHash:   parts[0],
		HashOffset: parts[1],
		UUID:       parts[2],
	}
	if !rootHashPattern.MatchString(r.RootHash) {
		return nil, fmt.Errorf("invalid root hash format: %s", r.RootHash)
	}
	if !hashOffsetPattern.MatchString(r.HashOffset) {
		return nil, fmt.Errorf("invalid hash offset format: %s", r.HashOffset)
	}
	if !uuidPattern.MatchString(r.UUID) {
		return nil, fmt.Errorf("invalid UUID format: %s", r.UUID)
	}
	return r, nil
}

// String returns the canonical rootHash_hashOffset_uuid form.
func (r *ArtifactRef) String() string {
	return r.RootHash + "_" + r.HashOffset + "_" + r.UUID
}

// ArtifactID returns the rootHash_uuid identity used as the HKDF salt for
// EMWP key derivation, binding the derived key to one specific artifact.
func (r *ArtifactRef) ArtifactID() string {
	return r.RootHash + "_" + r.UUID
}

// HashOffsetBytes parses the reference's hash offset field.
func (r *ArtifactRef) HashOffsetBytes() (uint64, error) {
	offset, err := strconv.ParseUint(r.HashOffset, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid hash offset %q: %w", r.HashOffset, err)
	}
	return offset, nil
}

// DeriveKey derives the per-artifact dm-crypt key from the EMWP master key
// using HKDF-SHA256 with the artifact ID as salt.
func DeriveKey(masterKey []byte, ref *ArtifactRef) ([]byte, error) {
	if len(masterKey) != EMWPMasterKeyBytes {
		return nil, fmt.Errorf("EMWP master key is %d bytes, want %d", len(masterKey), EMWPMasterKeyBytes)
	}
	return hkdf.Key(sha256.New, masterKey, []byte(ref.ArtifactID()), EMWPKeyDeriveInfo, EMWPKeyBytes)
}

// ParseMasterKey decodes and validates a base64-encoded EMWP master key.
func ParseMasterKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decoding EMWP master key as base64: %w", err)
	}
	if len(key) != EMWPMasterKeyBytes {
		return nil, fmt.Errorf("EMWP master key decoded to %d bytes, want %d", len(key), EMWPMasterKeyBytes)
	}
	return key, nil
}

// UUIDv5URL computes the deterministic RFC 4122 version 5 UUID of name in
// the URL namespace. All artifact UUIDs (dm-verity UUID, GPT disk GUID,
// GPT PARTUUID) are derived from the model identity this way.
func UUIDv5URL(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}

// HashDir computes a deterministic content hash over a model directory
// tree, used as the revision for local model directories.
func HashDir(dir string) (string, error) {
	h1, err := dirhash.HashDir(dir, "", dirhash.Hash1)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h1, "h1:"))
	if err != nil {
		return "", fmt.Errorf("decoding dirhash output: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
