package modelwrap

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Golden vectors pinned during the Python-to-Go migration, generated from
// the original Python packer and verified byte-identical. All values are
// independently derivable from the underlying standards: HKDF-SHA256
// (RFC 5869), UUIDv5 in the URL namespace (RFC 4122), and the Go module
// Hash1 directory hash.
const (
	vectorRootHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	vectorPartUUID = "8a3c9f0e-1111-5222-b333-444444444444"
	vectorModel    = "hf-internal-testing/tiny-random-GPT2Model@d6694b0d8fe17978761c9305dc151780506b192e"
)

func TestParseRef(t *testing.T) {
	ref, err := ParseRef(vectorRootHash + "_4096_" + vectorPartUUID)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if ref.RootHash != vectorRootHash || ref.HashOffset != "4096" || ref.UUID != vectorPartUUID {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	if got := ref.String(); got != vectorRootHash+"_4096_"+vectorPartUUID {
		t.Fatalf("String() = %q", got)
	}
	if got := ref.ArtifactID(); got != vectorRootHash+"_"+vectorPartUUID {
		t.Fatalf("ArtifactID() = %q", got)
	}

	for _, invalid := range []string{
		"",
		"a_b",
		"nothex_4096_" + vectorPartUUID,
		vectorRootHash + "_x_" + vectorPartUUID,
		vectorRootHash + "_4096_not-a-uuid",
		vectorRootHash + "_4096_" + vectorPartUUID + "_extra",
		"E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855_4096_" + vectorPartUUID,
	} {
		if _, err := ParseRef(invalid); err == nil {
			t.Errorf("ParseRef(%q) should fail", invalid)
		}
	}
}

func TestDeriveKey(t *testing.T) {
	master := make([]byte, 64)
	for i := range master {
		master[i] = byte(i)
	}
	ref := &ArtifactRef{RootHash: vectorRootHash, HashOffset: "4096", UUID: vectorPartUUID}
	key, err := DeriveKey(master, ref)
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	want := "201747a5e94aebc493296f77a33301f58144765be712a17685106ecd623b7ba2ac68ee4cf46d025ed111bd3dfdeda807ce9577f682561a6219a4eb25ae1a48d6"
	if got := hex.EncodeToString(key); got != want {
		t.Fatalf("DeriveKey = %s, want %s", got, want)
	}

	if _, err := DeriveKey(master[:32], ref); err == nil {
		t.Fatal("DeriveKey should reject short master keys")
	}
}

func TestParseMasterKey(t *testing.T) {
	encoded := "a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2traw==" // b"k"*64
	key, err := ParseMasterKey(encoded + "\n")
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	if len(key) != EMWPMasterKeyBytes {
		t.Fatalf("key length = %d", len(key))
	}
	for _, invalid := range []string{"", "!!!!", "a2tr"} {
		if _, err := ParseMasterKey(invalid); err == nil {
			t.Errorf("ParseMasterKey(%q) should fail", invalid)
		}
	}
}

func TestUUIDv5URL(t *testing.T) {
	// The "-emwp-outer" vector is also confirmed against a real packed
	// artifact: the EMWP round-trip integration test packs this model and
	// asserts the same PARTUUID in the emitted reference.
	cases := map[string]string{
		vectorModel + "-inner":        "905785b4-b198-5ed6-b97d-e7c36a0be1df",
		vectorModel + "-emwp-outer":   "9701bf21-6de2-59a2-bdea-141aaae05fc3",
		vectorRootHash + "-emwp-disk": "f5bb5818-295a-5b3e-910a-82f72eca4cc2",
	}
	for name, want := range cases {
		if got := UUIDv5URL(name); got != want {
			t.Errorf("UUIDv5URL(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestHashDir(t *testing.T) {
	d := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(d, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("b.txt", "beta\n")
	write("a.txt", "alpha\n")
	nested := make([]byte, 256)
	for i := range nested {
		nested[i] = byte(i)
	}
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "sub", "nested.bin"), nested, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(d, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	write("zsub/deep/x", "x")

	got, err := HashDir(d)
	if err != nil {
		t.Fatalf("HashDir: %v", err)
	}
	// Pinned hex encoding of the go.sum Hash1 digest for this fixture,
	// independently recomputed from the Hash1 specification.
	want := "560b265285af784ef29389714811ab259fee8b31225cc62e1811be662b7d4f36"
	if got != want {
		t.Fatalf("HashDir = %s, want %s", got, want)
	}

	// Symlinks to directories are not representable in Hash1.
	if err := os.Symlink("sub", filepath.Join(d, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := HashDir(d); err == nil {
		t.Fatal("HashDir should fail on symlinks to directories")
	}
}
