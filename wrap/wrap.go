// Package wrap implements the Modelwrap packer: it builds deterministic
// EROFS images of model directories, wraps them with dm-verity (MWP), and
// optionally encrypts them into a GPT disk image with dm-crypt (EMWP).
//
// It shells out to mkfs.erofs, veritysetup, cryptsetup, and sgdisk, and is
// only intended to run inside the modelwrap container on Linux.
package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tinfoilsh/modelwrap"
)

// Options configures a single packing run. Model is a Hugging Face model
// ID, preferably name@revision. ModelDir packs a local directory instead
// of downloading; if Model has no revision, the directory content hash is
// used as the revision.
type Options struct {
	Model     string
	ModelDir  string
	CacheDir  string
	OutputDir string
	Encrypt   bool
	Verify    bool
	KeyFile   string
	HFToken   string
}

// Pack runs the full packing flow and returns the final artifact
// reference string (EMWP if encryption was requested, MWP otherwise).
func Pack(opts Options) (string, error) {
	if opts.CacheDir == "" {
		opts.CacheDir = "cache"
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "output"
	}
	for _, dir := range []string{opts.CacheDir, opts.OutputDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
	}

	model, modelDir, err := resolveModel(opts)
	if err != nil {
		return "", err
	}
	modelName, modelCommit, _ := strings.Cut(model, "@")

	if opts.ModelDir != "" {
		fmt.Printf("Using local model directory %s as %s\n", modelDir, model)
	} else {
		fmt.Printf("Downloading %s to %s\n", model, modelDir)
		if err := downloadModel(modelName, modelCommit, modelDir, opts.HFToken); err != nil {
			return "", err
		}
		// Remove the download cache for reproducibility.
		if err := os.RemoveAll(filepath.Join(modelDir, ".cache")); err != nil {
			return "", err
		}
	}

	outputModelDir := filepath.Join(opts.OutputDir, modelName)
	if err := os.MkdirAll(outputModelDir, 0755); err != nil {
		return "", err
	}

	mwpFile := filepath.Join(outputModelDir, modelCommit+".mpk")
	infoFile := filepath.Join(outputModelDir, modelCommit+".info")

	if err := makeEROFS(model, modelDir, mwpFile); err != nil {
		return "", err
	}
	if err := formatVerity(model, mwpFile, infoFile); err != nil {
		return "", err
	}

	if opts.Verify {
		if err := VerifyMWP(mwpFile, infoFile); err != nil {
			return "", err
		}
	} else {
		fmt.Println("Skipping dm-verity verification. Pass --verify or set VERIFY=1 to verify cached artifacts.")
	}

	ref, err := parseInfoFile(infoFile)
	if err != nil {
		return "", err
	}

	if !opts.Encrypt {
		return ref.String(), nil
	}

	masterKey, err := loadMasterKey(opts.KeyFile)
	if err != nil {
		return "", err
	}

	emwpRef := &modelwrap.ArtifactRef{
		RootHash:   ref.RootHash,
		HashOffset: ref.HashOffset,
		UUID:       modelwrap.UUIDv5URL(model + "-emwp-outer"),
	}
	emwpFile := filepath.Join(outputModelDir, modelCommit+".emwp")
	emwpInfoFile := filepath.Join(outputModelDir, modelCommit+".emwp.info")

	if _, err := os.Stat(emwpFile); os.IsNotExist(err) {
		if err := encryptEMWP(mwpFile, emwpFile, emwpRef, masterKey); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else {
		fmt.Printf("Using existing EMWP artifact: %s\n", emwpFile)
	}

	if err := os.WriteFile(emwpInfoFile+".tmp", []byte(emwpRef.String()), 0644); err != nil {
		return "", err
	}
	if err := os.Rename(emwpInfoFile+".tmp", emwpInfoFile); err != nil {
		return "", err
	}

	if opts.Verify {
		if err := VerifyEMWP(emwpFile, emwpInfoFile, masterKey); err != nil {
			return "", err
		}
	}
	return emwpRef.String(), nil
}

// resolveModel determines the model@revision identity and the directory
// containing the model files.
func resolveModel(opts Options) (model, modelDir string, err error) {
	model = opts.Model

	if opts.ModelDir != "" {
		fi, err := os.Stat(opts.ModelDir)
		if err != nil || !fi.IsDir() {
			return "", "", fmt.Errorf("MODEL_DIR is not a directory: %s", opts.ModelDir)
		}
		localRevision, err := modelwrap.HashDir(opts.ModelDir)
		if err != nil {
			return "", "", fmt.Errorf("hashing model directory: %w", err)
		}
		if model == "" {
			abs, err := filepath.Abs(opts.ModelDir)
			if err != nil {
				return "", "", err
			}
			name := filepath.Base(abs)
			if name == "/" || name == "." {
				name = "model"
			}
			model = name + "@" + localRevision
		} else if !strings.Contains(model, "@") {
			model = model + "@" + localRevision
		}
		return model, opts.ModelDir, nil
	}

	if model == "" {
		return "", "", fmt.Errorf("model argument or MODEL environment variable is required")
	}
	if !strings.Contains(model, "@") {
		sha, err := resolveHFRevision(model, opts.HFToken)
		if err != nil {
			return "", "", err
		}
		fmt.Printf("Resolved %s default branch HEAD -> %s\n", model, sha)
		model = model + "@" + sha
	}
	return model, filepath.Join(opts.CacheDir, strings.Replace(model, "@", "/", 1)), nil
}

// resolveHFRevision resolves the default branch HEAD commit of a Hugging
// Face model via the Hub API.
func resolveHFRevision(model, token string) (string, error) {
	req, err := http.NewRequest("GET", "https://huggingface.co/api/models/"+url.PathEscape(model), nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying Hugging Face API for %s: %w", model, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("querying Hugging Face API for %s: %s", model, resp.Status)
	}
	var info struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decoding Hugging Face API response for %s: %w", model, err)
	}
	if info.SHA == "" {
		return "", fmt.Errorf("could not resolve HEAD commit for %s; specify the commit explicitly: %s@<commit>", model, model)
	}
	return info.SHA, nil
}

// downloadModel fetches a model snapshot using the official `hf` CLI from
// huggingface_hub, which handles auth, resume, and xet-backed transfers.
func downloadModel(name, revision, dir, token string) error {
	cmd := exec.Command("hf", "download", name, "--revision", revision, "--local-dir", dir)
	if token != "" {
		cmd.Env = append(os.Environ(), "HF_TOKEN="+token)
	}
	return run(cmd)
}

// makeEROFS builds the deterministic EROFS image if it does not exist.
func makeEROFS(model, modelDir, mwpFile string) error {
	if _, err := os.Stat(mwpFile); err == nil {
		fmt.Printf("Using existing EROFS image %s\n", mwpFile)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	fmt.Printf("Creating EROFS image %s\n", mwpFile)
	err := run(exec.Command(
		"mkfs.erofs",
		"--all-root",
		"-T0", // Zero timestamps
		"-U"+modelwrap.UUIDv5URL(model+"-inner"), // Static filesystem UUID
		mwpFile+".tmp",
		modelDir,
	))
	if err != nil {
		return err
	}
	return os.Rename(mwpFile+".tmp", mwpFile)
}

// formatVerity appends the dm-verity hash tree to the EROFS image and
// writes the rootHash_hashOffset_uuid info file, if not already done.
func formatVerity(model, mwpFile, infoFile string) error {
	if _, err := os.Stat(infoFile); err == nil {
		fmt.Printf("dm-verity volume already exists at %s\n", mwpFile)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	fi, err := os.Stat(mwpFile)
	if err != nil {
		return err
	}
	offset := (fi.Size() + 4095) / 4096 * 4096
	verityUUID := modelwrap.UUIDv5URL(model + "-inner")
	salt := sha256.Sum256([]byte(model))

	fmt.Printf("Running veritysetup on %s\n", mwpFile)
	err = run(exec.Command(
		"veritysetup",
		fmt.Sprintf("--format=%d", modelwrap.VerityFormat),
		"--hash="+modelwrap.VerityHashAlgorithm,
		fmt.Sprintf("--data-block-size=%d", modelwrap.VerityDataBlockSize),
		fmt.Sprintf("--hash-block-size=%d", modelwrap.VerityHashBlockSize),
		"--salt="+hex.EncodeToString(salt[:]),
		"--uuid="+verityUUID,
		fmt.Sprintf("--hash-offset=%d", offset),
		"--root-hash-file="+infoFile,
		"format",
		mwpFile, // data device
		mwpFile, // hash device
	))
	if err != nil {
		return err
	}

	f, err := os.OpenFile(infoFile, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open dm-verity info file %s: %w", infoFile, err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "_%d_%s", offset, verityUUID); err != nil {
		return err
	}
	return nil
}

// encryptEMWP wraps an MWP file into a GPT disk image whose single
// partition is a raw dm-crypt encryption of the MWP content. The disk
// GUID and PARTUUID are deterministic so the artifact is reproducible.
func encryptEMWP(mwpFile, emwpFile string, ref *modelwrap.ArtifactRef, masterKey []byte) error {
	fi, err := os.Stat(mwpFile)
	if err != nil {
		return err
	}
	encryptedSize := (fi.Size() + modelwrap.EMWPSectorSize - 1) / modelwrap.EMWPSectorSize * modelwrap.EMWPSectorSize
	sectors := encryptedSize / modelwrap.GPTSectorSize
	endSector := modelwrap.EMWPPartitionStartSector + sectors - 1
	totalSectors := endSector + 1 + modelwrap.EMWPGPTTrailingSectors
	diskUUID := modelwrap.UUIDv5URL(ref.RootHash + "-emwp-disk")
	tmpFile := emwpFile + ".tmp"
	dmKeyFile := emwpFile + ".key.tmp"
	mapperName := "modelwrap-emwp-" + ref.RootHash[:16]

	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		return err
	}

	for _, path := range []string{tmpFile, dmKeyFile} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	defer func() {
		closeCryptMapper(mapperName)
		os.Remove(dmKeyFile)
		os.Remove(tmpFile)
	}()

	fmt.Printf("Creating EMWP GPT image %s\n", emwpFile)
	if err := createSparseFile(tmpFile, totalSectors*modelwrap.GPTSectorSize); err != nil {
		return err
	}
	err = run(exec.Command(
		"sgdisk",
		"--clear",
		"--disk-guid="+diskUUID,
		fmt.Sprintf("--new=1:%d:%d", modelwrap.EMWPPartitionStartSector, endSector),
		"--typecode=1:8300",
		"--partition-guid=1:"+ref.UUID,
		"--change-name=1:emwp",
		tmpFile,
	))
	if err != nil {
		return err
	}

	if err := os.WriteFile(dmKeyFile, dmKey, 0600); err != nil {
		return err
	}
	err = run(exec.Command(
		"cryptsetup", "open",
		"--type", "plain",
		"--cipher", modelwrap.EMWPCipher,
		"--key-size", strconv.Itoa(modelwrap.EMWPKeySizeBits),
		"--sector-size", strconv.Itoa(modelwrap.EMWPSectorSize),
		"--key-file", dmKeyFile,
		"--offset", strconv.Itoa(modelwrap.EMWPPartitionStartSector),
		"--skip", "0",
		"--size", strconv.FormatInt(sectors, 10),
		tmpFile,
		mapperName,
	))
	if err != nil {
		return err
	}

	if err := copyToDevice(mwpFile, "/dev/mapper/"+mapperName); err != nil {
		return err
	}
	if err := closeCryptMapper(mapperName); err != nil {
		return err
	}
	return os.Rename(tmpFile, emwpFile)
}

// VerifyMWP runs an offline dm-verity verification of an MWP artifact
// against its info file.
func VerifyMWP(mwpFile, infoFile string) error {
	ref, err := parseInfoFile(infoFile)
	if err != nil {
		return err
	}
	if _, err := os.Stat(mwpFile); err != nil {
		return fmt.Errorf("MWP artifact not found: %s", mwpFile)
	}

	fmt.Printf("Verifying dm-verity artifact %s\n", mwpFile)
	err = run(exec.Command(
		"veritysetup",
		fmt.Sprintf("--format=%d", modelwrap.VerityFormat),
		"--hash="+modelwrap.VerityHashAlgorithm,
		fmt.Sprintf("--data-block-size=%d", modelwrap.VerityDataBlockSize),
		fmt.Sprintf("--hash-block-size=%d", modelwrap.VerityHashBlockSize),
		"--hash-offset="+ref.HashOffset,
		"verify",
		mwpFile, // data device
		mwpFile, // hash device
		ref.RootHash,
	))
	if err != nil {
		return err
	}
	fmt.Println("Verification OK.")
	return nil
}

// VerifyEMWP decrypts an EMWP artifact through a temporary dm-crypt
// mapping and verifies the inner dm-verity tree.
func VerifyEMWP(emwpFile, infoFile string, masterKey []byte) error {
	ref, err := parseInfoFile(infoFile)
	if err != nil {
		return err
	}
	fi, err := os.Stat(emwpFile)
	if err != nil {
		return fmt.Errorf("EMWP artifact not found: %s", emwpFile)
	}

	sectors := fi.Size()/modelwrap.GPTSectorSize - modelwrap.EMWPPartitionStartSector - modelwrap.EMWPGPTTrailingSectors
	dmKeyFile := emwpFile + ".key.tmp"
	mapperName := "modelwrap-emwp-verify-" + ref.RootHash[:16]
	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		return err
	}

	defer func() {
		closeCryptMapper(mapperName)
		os.Remove(dmKeyFile)
	}()
	if err := os.WriteFile(dmKeyFile, dmKey, 0600); err != nil {
		return err
	}
	err = run(exec.Command(
		"cryptsetup", "open",
		"--type", "plain",
		"--cipher", modelwrap.EMWPCipher,
		"--key-size", strconv.Itoa(modelwrap.EMWPKeySizeBits),
		"--sector-size", strconv.Itoa(modelwrap.EMWPSectorSize),
		"--key-file", dmKeyFile,
		"--offset", strconv.Itoa(modelwrap.EMWPPartitionStartSector),
		"--skip", "0",
		"--size", strconv.FormatInt(sectors, 10),
		emwpFile,
		mapperName,
	))
	if err != nil {
		return err
	}
	return VerifyMWP("/dev/mapper/"+mapperName, infoFile)
}

// LoadMasterKey loads the EMWP master key from keyFile if set, else from
// the PRIVATE_MODEL_KEY_FILE or PRIVATE_MODEL_KEY_B64 environment.
func loadMasterKey(keyFile string) ([]byte, error) {
	if keyFile == "" {
		keyFile = os.Getenv("PRIVATE_MODEL_KEY_FILE")
	}
	var encoded string
	if keyFile != "" {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("reading EMWP master key file: %w", err)
		}
		encoded = string(data)
	} else {
		encoded = os.Getenv("PRIVATE_MODEL_KEY_B64")
	}
	if strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("--key-file, PRIVATE_MODEL_KEY_FILE, or PRIVATE_MODEL_KEY_B64 is required for --encrypt")
	}
	return modelwrap.ParseMasterKey(encoded)
}

func parseInfoFile(infoFile string) (*modelwrap.ArtifactRef, error) {
	data, err := os.ReadFile(infoFile)
	if err != nil {
		return nil, err
	}
	ref, err := modelwrap.ParseRef(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid info file %s: %w", infoFile, err)
	}
	return ref, nil
}

func createSparseFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// copyToDevice writes src into the block device dst and syncs it.
func copyToDevice(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.CopyBuffer(out, in, make([]byte, 4*1024*1024)); err != nil {
		return fmt.Errorf("writing %s to %s: %w", src, dst, err)
	}
	return out.Sync()
}

func closeCryptMapper(name string) error {
	if _, err := os.Stat("/dev/mapper/" + name); os.IsNotExist(err) {
		return nil
	}
	return run(exec.Command("cryptsetup", "close", name))
}

func run(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return nil
}
