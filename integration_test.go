//go:build integration

package modelwrap_test

import (
	"encoding/base64"
	"fmt"
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
