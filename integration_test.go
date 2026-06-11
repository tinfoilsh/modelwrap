//go:build integration

package modelwrap_test

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tinfoilsh/modelwrap"
	"github.com/tinfoilsh/modelwrap/unwrap"
	"github.com/tinfoilsh/modelwrap/wrap"
)

const (
	integrationModelName     = "hf-internal-testing/tiny-random-GPT2Model"
	integrationModelRevision = "d6694b0d8fe17978761c9305dc151780506b192e"
	integrationModel         = integrationModelName + "@" + integrationModelRevision
)

// TestEMWPRoundTripIntegration downloads and packs a tiny public model as
// EMWP and then consumes it through the unwrap path: loop-mount the
// encrypted payload partition, open dm-crypt with the derived key, open
// dm-verity, and mount the verified EROFS. It needs a privileged Linux
// environment with the modelwrap packer tools, network access, loop
// devices, and dm_verity available (run via test/e2e.sh).
func TestEMWPRoundTripIntegration(t *testing.T) {
	if os.Getenv("TINFOIL_MODELWRAP_INTEGRATION") != "1" {
		t.Skip("set TINFOIL_MODELWRAP_INTEGRATION=1 to run")
	}

	work := t.TempDir()
	masterKey := []byte(strings.Repeat("k", modelwrap.EMWPMasterKeyBytes))
	keyFile := filepath.Join(work, "master.key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(masterKey)), 0600); err != nil {
		t.Fatal(err)
	}

	rawRef, err := wrap.Pack(wrap.Options{
		Model:     integrationModel,
		CacheDir:  filepath.Join(work, "cache"),
		OutputDir: filepath.Join(work, "output"),
		Encrypt:   true,
		Verify:    true,
		KeyFile:   keyFile,
	})
	if err != nil {
		t.Fatalf("packing EMWP: %v", err)
	}
	ref, err := modelwrap.ParseRef(rawRef)
	if err != nil {
		t.Fatalf("parsing packed ref %q: %v", rawRef, err)
	}
	if want := modelwrap.UUIDv5URL(integrationModel + "-emwp-outer"); ref.UUID != want {
		t.Fatalf("EMWP PARTUUID = %s, want %s", ref.UUID, want)
	}

	emwpFile := filepath.Join(work, "output", integrationModelName, integrationModelRevision+".emwp")

	// Expose the encrypted payload partition the same way the consumer
	// sees it: a read-only block device covering exactly the partition.
	fi, err := os.Stat(emwpFile)
	if err != nil {
		t.Fatal(err)
	}
	partOffset := int64(modelwrap.EMWPPartitionStartSector * modelwrap.GPTSectorSize)
	partSize := fi.Size() - partOffset - int64(modelwrap.EMWPGPTTrailingSectors*modelwrap.GPTSectorSize)
	out, err := exec.Command(
		"losetup", "--read-only", "--find", "--show",
		"--offset", fmt.Sprint(partOffset),
		"--sizelimit", fmt.Sprint(partSize),
		emwpFile,
	).Output()
	if err != nil {
		t.Fatalf("losetup: %v", err)
	}
	loopDev := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loopDev).Run() })

	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		t.Fatalf("deriving dm-crypt key: %v", err)
	}
	dmKeyFile := filepath.Join(work, "dm.key")
	if err := os.WriteFile(dmKeyFile, dmKey, 0600); err != nil {
		t.Fatal(err)
	}

	cryptName := "modelwrap-it-crypt"
	if err := unwrap.OpenCrypt(loopDev, cryptName, dmKeyFile); err != nil {
		t.Fatalf("opening dm-crypt: %v", err)
	}
	t.Cleanup(func() { unwrap.CloseCrypt(cryptName) })

	verityName := "modelwrap-it-verity"
	if err := unwrap.OpenVerity("/dev/mapper/"+cryptName, verityName, ref.RootHash, ref.HashOffset); err != nil {
		t.Fatalf("opening dm-verity: %v", err)
	}
	t.Cleanup(func() { unwrap.CloseVerity(verityName) })

	mountPoint := filepath.Join(work, "mnt")
	if err := unwrap.Mount("/dev/mapper/"+verityName, mountPoint); err != nil {
		t.Fatalf("mounting verified EROFS: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("umount", mountPoint).Run() })

	for _, name := range []string{"config.json", "pytorch_model.bin"} {
		if _, err := os.Stat(filepath.Join(mountPoint, name)); err != nil {
			t.Fatalf("checking mounted model file %s: %v", name, err)
		}
	}
}

// TestEMWPGoEncryptKernelDecrypt is the authoritative compatibility test for
// the native-Go dm-crypt encryption: it packs a local directory as EMWP
// (encrypted entirely in userspace by package crypt, no cryptsetup) and then
// decrypts it with the real kernel dm-crypt via cryptsetup, verifies it with
// kernel dm-verity, and mounts it. If the Go ciphertext were not byte-exact
// dm-crypt output, the kernel would decrypt garbage and dm-verity would fail.
// It needs no network or huggingface tooling, only a privileged Linux
// environment with erofs-utils, cryptsetup, and gdisk.
func TestEMWPGoEncryptKernelDecrypt(t *testing.T) {
	if os.Getenv("TINFOIL_MODELWRAP_INTEGRATION") != "1" {
		t.Skip("set TINFOIL_MODELWRAP_INTEGRATION=1 to run")
	}

	work := t.TempDir()
	modelDir := filepath.Join(work, "model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Content large enough to span many 4096-byte sectors, so the kernel
	// must walk the per-sector IV across the whole partition to recover it.
	modelFiles := map[string][]byte{
		"config.json": []byte(`{"model_type":"test","n":12345}`),
		"pytorch_model.bin": func() []byte {
			b := make([]byte, 512*1024)
			for i := range b {
				b[i] = byte(i*131 + 7)
			}
			return b
		}(),
	}
	for name, content := range modelFiles {
		if err := os.WriteFile(filepath.Join(modelDir, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}

	masterKey := []byte(strings.Repeat("k", modelwrap.EMWPMasterKeyBytes))
	keyFile := filepath.Join(work, "master.key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(masterKey)), 0600); err != nil {
		t.Fatal(err)
	}

	// Pack with the Go encryptor. Verify:false on purpose: the only
	// verification that matters here is the kernel's, performed below.
	rawRef, err := wrap.Pack(wrap.Options{
		Model:     "testmodel@v1",
		ModelDir:  modelDir,
		CacheDir:  filepath.Join(work, "cache"),
		OutputDir: filepath.Join(work, "output"),
		Encrypt:   true,
		Verify:    false,
		KeyFile:   keyFile,
	})
	if err != nil {
		t.Fatalf("packing EMWP: %v", err)
	}
	ref, err := modelwrap.ParseRef(rawRef)
	if err != nil {
		t.Fatalf("parsing packed ref %q: %v", rawRef, err)
	}

	emwpFile := filepath.Join(work, "output", "testmodel", "v1.emwp")
	fi, err := os.Stat(emwpFile)
	if err != nil {
		t.Fatal(err)
	}

	// Expose the encrypted payload partition exactly as the consumer does.
	partOffset := int64(modelwrap.EMWPPartitionStartSector * modelwrap.GPTSectorSize)
	partSize := fi.Size() - partOffset - int64(modelwrap.EMWPGPTTrailingSectors*modelwrap.GPTSectorSize)
	out, err := exec.Command(
		"losetup", "--read-only", "--find", "--show",
		"--offset", fmt.Sprint(partOffset),
		"--sizelimit", fmt.Sprint(partSize),
		emwpFile,
	).Output()
	if err != nil {
		t.Fatalf("losetup: %v", err)
	}
	loopDev := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loopDev).Run() })

	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		t.Fatalf("deriving dm-crypt key: %v", err)
	}
	dmKeyFile := filepath.Join(work, "dm.key")
	if err := os.WriteFile(dmKeyFile, dmKey, 0600); err != nil {
		t.Fatal(err)
	}

	// Kernel dm-crypt decrypt of the Go-produced ciphertext. If the Go XTS
	// output were not byte-exact dm-crypt ciphertext, the kernel would
	// produce different plaintext through this mapping.
	cryptName := "modelwrap-goit-crypt"
	if err := unwrap.OpenCrypt(loopDev, cryptName, dmKeyFile); err != nil {
		t.Fatalf("kernel cryptsetup open of Go ciphertext: %v", err)
	}
	t.Cleanup(func() { unwrap.CloseCrypt(cryptName) })

	// Authoritative check: the bytes the kernel dm-crypt produces must equal
	// the original MWP plaintext the packer encrypted in Go.
	plain, err := os.ReadFile(filepath.Join(work, "output", "testmodel", "v1.mpk"))
	if err != nil {
		t.Fatal(err)
	}
	dev, err := os.Open("/dev/mapper/" + cryptName)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	got := make([]byte, len(plain))
	if _, err := io.ReadFull(dev, got); err != nil {
		t.Fatalf("reading kernel-decrypted device: %v", err)
	}
	if !bytesEqual(got, plain) {
		t.Fatalf("kernel dm-crypt decryption does not match the original MWP plaintext "+
			"(first diff at byte %d): Go ciphertext is not dm-crypt compatible", firstDiffIdx(got, plain))
	}

	// Independent integrity check, in userspace so it needs no dm-verity
	// kernel target: the kernel-decrypted plaintext must satisfy the attested
	// root hash.
	if err := wrap.VerifyMWP("/dev/mapper/"+cryptName, filepath.Join(work, "output", "testmodel", "v1.info")); err != nil {
		t.Fatalf("veritysetup verify over kernel-decrypted plaintext: %v", err)
	}
}

func firstDiffIdx(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
