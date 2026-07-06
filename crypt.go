package modelwrap

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// EMWP dm-crypt parameters. These are passed explicitly to cryptsetup so
// tool default changes never silently alter the artifact format; with
// --type plain there is no on-disk encryption metadata at all.
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

// CryptsetupArgs returns the explicit cryptsetup arguments pinning the
// EMWP cipher parameters for a plain open, so tool default changes can
// never alter the mapping. --skip 0 pins the plain64 IV numbering to
// start at the volume's first sector, matching the Go encryptor and the
// dm-crypt golden vector.
func CryptsetupArgs() []string {
	return []string{
		"--type", "plain",
		"--cipher", EMWPCipher,
		"--key-size", strconv.Itoa(EMWPKeySizeBits),
		"--sector-size", strconv.Itoa(EMWPSectorSize),
		"--skip", "0",
	}
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
