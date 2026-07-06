package modelwrap

import (
	"encoding/hex"
	"strings"
	"testing"
)

const testHashOffset = uint64(40960) // 10 data blocks

func TestVerityParamsForArtifact(t *testing.T) {
	salt := VeritySalt("org/model@revision")

	if _, err := VerityParamsForArtifact(testHashOffset, salt[:16]); err == nil {
		t.Error("short salt was accepted")
	}
	if _, err := VerityParamsForArtifact(testHashOffset+1, salt); err == nil {
		t.Error("unaligned hash offset was accepted")
	}
	if _, err := VerityParamsForArtifact(0, salt); err == nil {
		t.Error("zero hash offset was accepted")
	}

	params, err := VerityParamsForArtifact(testHashOffset, salt)
	if err != nil {
		t.Fatalf("deriving params: %v", err)
	}
	if params.DataBlocks != testHashOffset/VerityDataBlockSize {
		t.Fatalf("data blocks = %d, want %d", params.DataBlocks, testHashOffset/VerityDataBlockSize)
	}
	if params.HashTreeOffset != testHashOffset+VerityHashBlockSize {
		t.Fatalf("hash tree offset = %d, want %d", params.HashTreeOffset, testHashOffset+VerityHashBlockSize)
	}

	args := strings.Join(params.VeritysetupArgs(), " ")
	for _, want := range []string{
		"--no-superblock",
		"--format=1",
		"--hash=sha256",
		"--data-block-size=4096",
		"--hash-block-size=4096",
		"--data-blocks=10",
		"--hash-offset=45056",
		"--salt=" + hex.EncodeToString(salt),
	} {
		if !strings.Contains(args, want) {
			t.Errorf("veritysetup args %q missing %q", args, want)
		}
	}
}

func TestVeritySaltDeterministic(t *testing.T) {
	a := VeritySalt("org/model@revision")
	b := VeritySalt("org/model@revision")
	if string(a) != string(b) || len(a) != VeritySaltBytes {
		t.Fatal("salt derivation is not deterministic 32 bytes")
	}
	if string(a) == string(VeritySalt("org/model@other")) {
		t.Fatal("different identities produced the same salt")
	}
}
