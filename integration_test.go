//go:build integration

package modelwrap_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tinfoilsh/modelwrap"
	"github.com/tinfoilsh/modelwrap/unwrap"
	"github.com/tinfoilsh/modelwrap/wrap"
)

const (
	integrationModelName     = "hf-internal-testing/tiny-random-GPT2Model"
	integrationModelRevision = "d6694b0d8fe17978761c9305dc151780506b192e"
	integrationModel         = integrationModelName + "@" + integrationModelRevision

	// Tiny public model with two revisions that share LFS files:
	// seedRevisionNew is the child of seedRevisionOld and only adds
	// flax_model.msgpack, leaving pytorch_model.bin and tf_model.h5
	// identical, so packing old-then-new exercises download seeding.
	seedModelName   = "sshleifer/tiny-gpt2"
	seedRevisionOld = "f42686d7a97d000446a173ab001673a12e156924"
	seedRevisionNew = "5f91d94bd9cd7190a9f3216ff93cd1dd95f2c7be"
)

// integrationExpectedRefs pins the full packed EMWP ref
// (rootHash_hashOffset_uuid) of integrationModel per pack schema. Every
// entry is a frozen contract: a changed value means the schema's
// derivation changed, which breaks reproducibility of every artifact hash
// already enrolled under that schema. If a change is intentional it must
// ship as a NEW schema id, never by editing an existing entry.
var integrationExpectedRefs = map[int]string{
	1: "17c6605f3becf63a63da37cc6958b2d18f8abc750f9f546b9f57133db40c4326_2142208_9701bf21-6de2-59a2-bdea-141aaae05fc3",
	2: "e08ac80979ec407733e9958f6aefb4ccd015c79233ac62155d79db10b4ae1fd4_2142208_9701bf21-6de2-59a2-bdea-141aaae05fc3",
}

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
	// Check that the generated ref is still stable. The default request
	// must resolve to schema 1 and reproduce its frozen ref: a change here
	// is a breaking change for every already-enrolled artifact hash.
	if want := integrationExpectedRefs[1]; rawRef != want {
		t.Fatalf("packed ref = %s, want %s\n"+
			"The generated artifact changed. Schema 1 is frozen: find what changed "+
			"in the container image or the packer; an intentional derivation change must ship as a new schema.", rawRef, want)
	}
	sidecar := filepath.Join(work, "output", integrationModelName, integrationModelRevision+".schema")
	if data, err := os.ReadFile(sidecar); err != nil || string(data) != "1" {
		t.Fatalf("schema sidecar = %q, %v; want %q", data, err, "1")
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

	// Salt re-derived from the attested model identity; the artifact's
	// superblock is never read.
	verityName := "modelwrap-it-verity"
	if err := unwrap.OpenVerity("/dev/mapper/"+cryptName, verityName, ref.RootHash, ref.HashOffset, modelwrap.VeritySalt(integrationModel)); err != nil {
		t.Fatalf("opening dm-verity via derived salt: %v", err)
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

// TestPackSchemaStability packs the tiny integration model under every
// registered pack schema and checks each against its frozen pinned ref, so
// a derivation drift in any schema's toolchain or invocation fails CI
// loudly. It also proves the schema's toolchain actually engaged: schema 2
// passes --workers, which only a multithreaded mkfs.erofs build accepts.
// Needs network and the packer image toolchain (run via test/e2e.sh);
// verification is userspace-only, no privilege required.
func TestPackSchemaStability(t *testing.T) {
	if os.Getenv("TINFOIL_MODELWRAP_INTEGRATION") != "1" {
		t.Skip("set TINFOIL_MODELWRAP_INTEGRATION=1 to run")
	}

	for _, id := range modelwrap.SchemaIDs() {
		t.Run(fmt.Sprintf("schema-%d", id), func(t *testing.T) {
			want, ok := integrationExpectedRefs[id]
			if !ok {
				t.Fatalf("schema %d has no pinned ref: every registered schema needs a stability row", id)
			}

			work := t.TempDir()
			masterKey := []byte(strings.Repeat("k", modelwrap.EMWPMasterKeyBytes))
			keyFile := filepath.Join(work, "master.key")
			if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(masterKey)), 0600); err != nil {
				t.Fatal(err)
			}

			ref, err := wrap.Pack(wrap.Options{
				Model:     integrationModel,
				Schema:    id,
				CacheDir:  filepath.Join(work, "cache"),
				OutputDir: filepath.Join(work, "output"),
				Encrypt:   true,
				Verify:    true,
				KeyFile:   keyFile,
			})
			if err != nil {
				t.Fatalf("packing under schema %d: %v", id, err)
			}
			if ref != want {
				t.Fatalf("schema %d packed ref = %s, want pinned %s\n"+
					"The schema's derivation changed; schemas are frozen. An intentional "+
					"derivation change must ship as a new schema id.", id, ref, want)
			}

			sidecar := filepath.Join(work, "output", integrationModelName, integrationModelRevision+".schema")
			data, err := os.ReadFile(sidecar)
			if err != nil {
				t.Fatalf("reading schema sidecar: %v", err)
			}
			if string(data) != strconv.Itoa(id) {
				t.Fatalf("schema sidecar = %q, want %q", data, strconv.Itoa(id))
			}
		})
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
	if !bytes.Equal(got, plain) {
		t.Fatalf("kernel dm-crypt decryption does not match the original MWP plaintext "+
			"(first diff at byte %d): Go ciphertext is not dm-crypt compatible", firstDiffIdx(got, plain))
	}

	// Independent integrity check, in userspace so it needs no dm-verity
	// kernel target: the kernel-decrypted plaintext must satisfy the attested
	// root hash.
	if err := wrap.VerifyMWP("/dev/mapper/"+cryptName, filepath.Join(work, "output", "testmodel", "v1.info"), "testmodel@v1"); err != nil {
		t.Fatalf("veritysetup verify over kernel-decrypted plaintext: %v", err)
	}
}

// TestMWPSuperblockTamperIntegration demonstrates the superblock
// truncation issue and verifies the --no-superblock open path closes it.
// It packs a local directory as plaintext MWP, shrinks the superblock's
// data_blocks field by one, and checks that:
//
//  1. a legacy superblock-trusting open (plain --hash-offset) accepts the
//     tampered artifact and maps a silently truncated device;
//  2. the attested-identity open (derived salt, superblock never read)
//     opens the full, untruncated device despite the tampering.
//
// Requires the same privileged Linux environment as the round-trip test.
func TestMWPSuperblockTamperIntegration(t *testing.T) {
	if os.Getenv("TINFOIL_MODELWRAP_INTEGRATION") != "1" {
		t.Skip("set TINFOIL_MODELWRAP_INTEGRATION=1 to run")
	}

	work := t.TempDir()
	modelDir := filepath.Join(work, "model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A payload large enough for a few data blocks.
	payload := bytes.Repeat([]byte("tinfoil superblock tamper test\n"), 8192)
	for _, name := range []string{"weights.bin", "config.json"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), payload, 0644); err != nil {
			t.Fatal(err)
		}
	}

	rawRef, err := wrap.Pack(wrap.Options{
		Model:     "tamper-test",
		ModelDir:  modelDir,
		CacheDir:  filepath.Join(work, "cache"),
		OutputDir: filepath.Join(work, "output"),
		Verify:    true,
	})
	if err != nil {
		t.Fatalf("packing MWP: %v", err)
	}
	ref, err := modelwrap.ParseRef(rawRef)
	if err != nil {
		t.Fatalf("parsing packed ref %q: %v", rawRef, err)
	}
	revision, err := modelwrap.HashDir(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	model := "tamper-test@" + revision
	mwpFile := filepath.Join(work, "output", "tamper-test", revision+".mpk")
	hashOffset, err := ref.HashOffsetBytes()
	if err != nil {
		t.Fatal(err)
	}

	// Shrink data_blocks (u64 at superblock offset 72) by one block.
	f, err := os.OpenFile(mwpFile, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	blocksField := make([]byte, 8)
	if _, err := f.ReadAt(blocksField, int64(hashOffset)+72); err != nil {
		t.Fatal(err)
	}
	dataBlocks := binary.LittleEndian.Uint64(blocksField)
	binary.LittleEndian.PutUint64(blocksField, dataBlocks-1)
	if _, err := f.WriteAt(blocksField, int64(hashOffset)+72); err != nil {
		t.Fatal(err)
	}

	// 1. Legacy superblock-trusting open accepts the tampered artifact.
	legacyName := "modelwrap-it-tamper-legacy"
	legacy := exec.Command("veritysetup", "open", mwpFile, legacyName, mwpFile, ref.RootHash, "--hash-offset="+ref.HashOffset)
	legacy.Stdout, legacy.Stderr = os.Stdout, os.Stderr
	if err := legacy.Run(); err != nil {
		t.Fatalf("expected legacy superblock-trusting open to accept the truncated artifact, got: %v", err)
	}
	sectors, err := exec.Command("blockdev", "--getsz", "/dev/mapper/"+legacyName).Output()
	unwrap.CloseVerity(legacyName)
	if err != nil {
		t.Fatalf("reading truncated mapper size: %v", err)
	}
	if got, want := strings.TrimSpace(string(sectors)), fmt.Sprint((dataBlocks-1)*8); got != want {
		t.Fatalf("legacy mapper size = %s sectors, want truncated %s", got, want)
	}
	t.Logf("legacy open accepted tampered artifact with truncated size %d blocks", dataBlocks-1)

	// 2. Attested-identity open never reads the superblock and maps the full device.
	derivedName := "modelwrap-it-tamper-derived"
	if err := unwrap.OpenVerity(mwpFile, derivedName, ref.RootHash, ref.HashOffset, modelwrap.VeritySalt(model)); err != nil {
		t.Fatalf("derived-salt open failed on tampered superblock: %v", err)
	}
	t.Cleanup(func() { unwrap.CloseVerity(derivedName) })
	sectors, err = exec.Command("blockdev", "--getsz", "/dev/mapper/"+derivedName).Output()
	if err != nil {
		t.Fatalf("reading derived mapper size: %v", err)
	}
	if got, want := strings.TrimSpace(string(sectors)), fmt.Sprint(dataBlocks*8); got != want {
		t.Fatalf("derived mapper size = %s sectors, want full %s", got, want)
	}

	mountPoint := filepath.Join(work, "mnt")
	if err := unwrap.Mount("/dev/mapper/"+derivedName, mountPoint); err != nil {
		t.Fatalf("mounting verified EROFS: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("umount", mountPoint).Run() })
	got, err := os.ReadFile(filepath.Join(mountPoint, "weights.bin"))
	if err != nil {
		t.Fatalf("reading mounted file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("mounted file content mismatch")
	}
}

// TestSeededWrapMatchesColdWrap is the hash-neutrality differential test
// for cross-revision download seeding: a wrap whose download was seeded
// with hardlinks from a previous revision must produce artifacts
// byte-identical to a cold wrap of the same revision. It also proves the
// seeding path actually engaged: hf never hardlinks, so shared weights
// being the same inode across revision cache dirs can only come from the
// seeder. Needs network and the packer toolchain (run via test/e2e.sh).
func TestSeededWrapMatchesColdWrap(t *testing.T) {
	if os.Getenv("TINFOIL_MODELWRAP_INTEGRATION") != "1" {
		t.Skip("set TINFOIL_MODELWRAP_INTEGRATION=1 to run")
	}

	packMWP := func(work, model string) string {
		t.Helper()
		ref, err := wrap.Pack(wrap.Options{
			Model:     model,
			CacheDir:  filepath.Join(work, "cache"),
			OutputDir: filepath.Join(work, "output"),
			Verify:    true,
		})
		if err != nil {
			t.Fatalf("packing %s: %v", model, err)
		}
		return ref
	}

	// captureStdout redirects os.Stdout until the returned finish func is
	// called and returns everything written in between, re-emitting it to
	// the real stdout so the run log stays intact. finish is idempotent
	// and must be deferred: t.Fatal or a panic inside the capture window
	// then restores stdout instead of wedging the test binary once the
	// unread pipe fills. Swap and restore happen on this goroutine with
	// Pack running synchronously in between, and no writer outlives Pack
	// (it waits on its hf subprocess, whose dup of the pipe fd closes with
	// it), so the reader always reaches EOF once the write end closes.
	captureStdout := func() func() string {
		t.Helper()
		orig := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		captured := make(chan string, 1)
		go func() {
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			r.Close()
			captured <- buf.String()
		}()
		var out string
		var once sync.Once
		return func() string {
			once.Do(func() {
				os.Stdout = orig
				w.Close()
				out = <-captured
				fmt.Print(out)
			})
			return out
		}
	}

	// packMWPCaptured packs like packMWP but also captures everything the
	// packer (and its hf subprocess) writes to stdout, so a missing
	// seeding engagement can be attributed below.
	packMWPCaptured := func(work, model string) (string, string) {
		t.Helper()
		finish := captureStdout()
		defer finish()
		ref, packErr := wrap.Pack(wrap.Options{
			Model:     model,
			CacheDir:  filepath.Join(work, "cache"),
			OutputDir: filepath.Join(work, "output"),
			Verify:    true,
		})
		out := finish()
		if packErr != nil {
			t.Fatalf("packing %s: %v", model, packErr)
		}
		return ref, out
	}

	newModel := seedModelName + "@" + seedRevisionNew
	coldWork := t.TempDir()
	coldRef := packMWP(coldWork, newModel)

	// Same revision again, but with the old revision already in the cache
	// so the download is seeded.
	seedWork := t.TempDir()
	packMWP(seedWork, seedModelName+"@"+seedRevisionOld)

	// seedingEngaged proves the seeding path actually ran: hf never
	// hardlinks, so shared weights being the same inode across revision
	// cache dirs can only come from the seeder.
	seedingEngaged := func() bool {
		for _, name := range []string{"pytorch_model.bin", "tf_model.h5"} {
			oldFi, err := os.Stat(filepath.Join(seedWork, "cache", seedModelName, seedRevisionOld, name))
			if err != nil {
				return false
			}
			newFi, err := os.Stat(filepath.Join(seedWork, "cache", seedModelName, seedRevisionNew, name))
			if err != nil {
				return false
			}
			if !os.SameFile(oldFi, newFi) {
				return false
			}
		}
		return true
	}

	// Seeding degrades to a plain download on a transient Hub tree-API
	// failure, which would leave this differential trivially green without
	// exercising the mechanism. Two-tier policy: retry with a clean target
	// revision, then skip — loudly — only if the captured packer output
	// shows the seeder's own skip line; anything else is a mechanism
	// regression and fails hard. An unconditional skip is forbidden: it
	// would let a full seeding regression hide behind green CI.
	var seededRef string
	for attempt := 1; ; attempt++ {
		var out string
		seededRef, out = packMWPCaptured(seedWork, newModel)
		if seedingEngaged() {
			break
		}
		if attempt == 3 {
			if strings.Contains(out, "Not seeding") {
				t.Skipf("seeding never engaged after %d attempts — Hub tree API unavailable; differential not exercised this run\n%s", attempt, out)
			}
			t.Fatalf("seeding did not engage and the packer reported no seeding failure — mechanism regression, not Hub weather:\n%s", out)
		}
		if err := wrap.Delete(wrap.DeleteOptions{
			Model:     newModel,
			CacheDir:  filepath.Join(seedWork, "cache"),
			OutputDir: filepath.Join(seedWork, "output"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	if seededRef != coldRef {
		t.Errorf("seeded ref = %s, cold ref = %s: seeding changed the attested identity", seededRef, coldRef)
	}
	for _, suffix := range []string{".mpk", ".info"} {
		rel := filepath.Join("output", seedModelName, seedRevisionNew+suffix)
		cold, err := os.ReadFile(filepath.Join(coldWork, rel))
		if err != nil {
			t.Fatal(err)
		}
		seeded, err := os.ReadFile(filepath.Join(seedWork, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cold, seeded) {
			t.Errorf("seeded %s differs from cold wrap (first diff at byte %d)", rel, firstDiffIdx(seeded, cold))
		}
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
