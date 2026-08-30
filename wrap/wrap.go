// Package wrap implements the Modelwrap packer: it builds deterministic
// EROFS images of model directories, wraps them with dm-verity (MWP), and
// optionally encrypts them into a GPT disk image with dm-crypt (EMWP).
//
// It shells out to mkfs.erofs, sgdisk, and (for verification) veritysetup,
// and is only intended to run inside the modelwrap container on Linux. The
// dm-verity hash tree itself is built natively, in parallel.
package wrap

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tinfoilsh/modelwrap"
	"github.com/tinfoilsh/modelwrap/crypt"
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
		if err := seedFromPreviousRevisions(modelName, modelCommit, modelDir, opts.HFToken); err != nil {
			return "", err
		}
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
		if err := VerifyMWP(mwpFile, infoFile, model); err != nil {
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
		if err := VerifyEMWP(emwpFile, emwpInfoFile, masterKey, model); err != nil {
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
	// Escape each path segment individually: model IDs contain a "/"
	// separator (org/name) that must not be percent-encoded.
	segments := strings.Split(model, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	req, err := http.NewRequest("GET", "https://huggingface.co/api/models/"+strings.Join(segments, "/"), nil)
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
// The tree is built natively (see modelwrap.FormatVerityHashTree), with
// output byte-identical to the veritysetup format invocation it replaced,
// but hashing data blocks on all cores.
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
	if fi.Size() != offset {
		// The strict consumer derives the data size from the hash offset
		// (data area == hashOffset bytes), which requires block alignment.
		// EROFS images are always 4K-aligned, so this should never fire.
		return fmt.Errorf("EROFS image size %d is not a multiple of %d", fi.Size(), modelwrap.VerityDataBlockSize)
	}
	verityUUID := modelwrap.UUIDv5URL(model + "-inner")
	salt := modelwrap.VeritySalt(model)

	workers := runtime.GOMAXPROCS(0)
	fmt.Printf("Building verity hash tree for %s (Go, %d workers)\n", mwpFile, workers)
	rootHash, err := modelwrap.FormatVerityHashTree(mwpFile, modelwrap.VerityFormatParams{
		Salt:       salt,
		UUID:       verityUUID,
		DataBlocks: uint64(offset) / modelwrap.VerityDataBlockSize,
		HashOffset: uint64(offset),
		Workers:    workers,
	})
	if err != nil {
		return fmt.Errorf("formatting dm-verity hash tree: %w", err)
	}
	fmt.Printf("Root hash: %s\n", hex.EncodeToString(rootHash))

	// 0600 matches the mode veritysetup gave its --root-hash-file.
	ref := fmt.Sprintf("%s_%d_%s", hex.EncodeToString(rootHash), offset, verityUUID)
	if err := os.WriteFile(infoFile+".tmp", []byte(ref), 0600); err != nil {
		return err
	}
	return os.Rename(infoFile+".tmp", infoFile)
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

	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		return err
	}
	defer clear(dmKey)

	if err := os.Remove(tmpFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	defer os.Remove(tmpFile)

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

	if err := encryptPayload(tmpFile, mwpFile, dmKey); err != nil {
		return err
	}
	return os.Rename(tmpFile, emwpFile)
}

// encryptPayload streams the dm-crypt encryption of mwpFile into the
// encrypted payload partition of the GPT image at imgFile.
func encryptPayload(imgFile, mwpFile string, dmKey []byte) error {
	in, err := os.Open(mwpFile)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(imgFile, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer out.Close()

	payload := io.NewOffsetWriter(out, modelwrap.EMWPPartitionStartSector*modelwrap.GPTSectorSize)
	if _, err := crypt.EncryptStream(dmKey, payload, in); err != nil {
		return fmt.Errorf("encrypting %s: %w", mwpFile, err)
	}
	return out.Sync()
}

// VerifyMWP runs an offline dm-verity verification of an MWP artifact
// against its info file, through the same strict path consumers use:
// veritysetup with --no-superblock and fully explicit parameters, with
// the salt derived from the model identity.
func VerifyMWP(mwpFile, infoFile, model string) error {
	ref, err := parseInfoFile(infoFile)
	if err != nil {
		return err
	}
	if _, err := os.Stat(mwpFile); err != nil {
		return fmt.Errorf("MWP artifact not found: %s", mwpFile)
	}

	offset, err := ref.HashOffsetBytes()
	if err != nil {
		return err
	}
	params, err := modelwrap.VerityParamsForArtifact(offset, modelwrap.VeritySalt(model))
	if err != nil {
		return err
	}

	fmt.Printf("Verifying dm-verity artifact %s\n", mwpFile)
	args := append([]string{
		"verify",
		mwpFile, // data device
		mwpFile, // hash device
		ref.RootHash,
	}, params.VeritysetupArgs()...)
	err = run(exec.Command("veritysetup", args...))
	if err != nil {
		return err
	}
	fmt.Println("Verification OK.")
	return nil
}

// VerifyEMWP decrypts an EMWP artifact's payload partition in userspace and
// verifies the inner dm-verity tree of the recovered MWP plaintext.
func VerifyEMWP(emwpFile, infoFile string, masterKey []byte, model string) error {
	ref, err := parseInfoFile(infoFile)
	if err != nil {
		return err
	}
	fi, err := os.Stat(emwpFile)
	if err != nil {
		return fmt.Errorf("EMWP artifact not found: %s", emwpFile)
	}

	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		return err
	}
	defer clear(dmKey)

	plainFile := emwpFile + ".verify.tmp"
	defer os.Remove(plainFile)
	if err := decryptPayload(emwpFile, plainFile, fi.Size(), dmKey); err != nil {
		return err
	}
	return VerifyMWP(plainFile, infoFile, model)
}

// decryptPayload streams the decrypted MWP plaintext of an EMWP image's
// payload partition into plainFile.
func decryptPayload(emwpFile, plainFile string, emwpSize int64, dmKey []byte) error {
	partOffset := int64(modelwrap.EMWPPartitionStartSector * modelwrap.GPTSectorSize)
	partSize := emwpSize - partOffset - int64(modelwrap.EMWPGPTTrailingSectors*modelwrap.GPTSectorSize)

	in, err := os.Open(emwpFile)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(plainFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()

	payload := io.NewSectionReader(in, partOffset, partSize)
	if _, err := crypt.DecryptStream(dmKey, out, payload); err != nil {
		return fmt.Errorf("decrypting EMWP payload: %w", err)
	}
	return out.Sync()
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

func run(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return nil
}
