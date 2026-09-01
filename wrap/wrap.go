// Package wrap implements the Modelwrap packer: it builds deterministic
// EROFS images of model directories, wraps them with dm-verity (MWP), and
// optionally encrypts them into a GPT disk image with dm-crypt (EMWP).
//
// It shells out to the pack schema's pinned mkfs.erofs, sgdisk, and (for
// verification) veritysetup, and is only intended to run inside the
// modelwrap container on Linux. The dm-verity hash tree itself is built
// natively, in parallel.
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
	"strconv"
	"strings"
	"syscall"

	"github.com/tinfoilsh/modelwrap"
	"github.com/tinfoilsh/modelwrap/crypt"
)

// Options configures a single packing run. Model is a Hugging Face model
// ID, preferably name@revision. ModelDir packs a local directory instead
// of downloading; if Model has no revision, the directory content hash is
// used as the revision. Schema selects the pack schema; 0 means
// modelwrap.DefaultSchema.
type Options struct {
	Model     string
	ModelDir  string
	CacheDir  string
	OutputDir string
	Schema    int
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

	schemaID := opts.Schema
	if schemaID == 0 {
		schemaID = modelwrap.DefaultSchema
	}
	schema, err := modelwrap.SchemaByID(schemaID)
	if err != nil {
		return "", err
	}
	fmt.Printf("Packing under schema %d (erofs-utils %s, %s)\n", schema.ID, schema.ErofsUtils, schema.Doc)

	model, modelDir, err := resolveModel(opts)
	if err != nil {
		return "", err
	}
	modelName, modelCommit, _ := strings.Cut(model, "@")

	outputModelDir := filepath.Join(opts.OutputDir, modelName)
	base := filepath.Join(outputModelDir, modelCommit)

	// Fail fast before a potentially huge download; the authoritative
	// check runs again under the artifact lock below.
	if _, err := checkExistingArtifacts(base, schema.ID); err != nil {
		return "", err
	}

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

	if err := os.MkdirAll(outputModelDir, 0755); err != nil {
		return "", err
	}

	// Single-flight per (model, revision): every writer — any schema, and
	// --delete — serializes here, so builds and multi-file publishes are
	// never interleaved.
	release, err := acquireArtifactLock(base + ".lock")
	if err != nil {
		return "", err
	}
	defer release()

	mwpFile := base + ".mpk"
	infoFile := base + ".info"

	reuse, err := checkExistingArtifacts(base, schema.ID)
	if err != nil {
		return "", err
	}
	if reuse {
		fmt.Printf("Using existing schema %d artifacts %s.*\n", schema.ID, base)
		if err := backfillSchemaSidecar(base, schema.ID); err != nil {
			return "", err
		}
	} else {
		tmpMWP, mwpRef, err := buildMWP(schema, model, modelDir, base)
		if err != nil {
			return "", err
		}
		if err := publishArtifacts(base, schema.ID, tmpMWP, mwpRef); err != nil {
			return "", err
		}
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
	emwpFile := base + ".emwp"
	emwpInfoFile := base + ".emwp.info"

	if _, err := os.Stat(emwpFile); os.IsNotExist(err) {
		if err := encryptEMWP(mwpFile, emwpFile, emwpRef, masterKey); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else {
		fmt.Printf("Using existing EMWP artifact: %s\n", emwpFile)
	}

	if err := writeStaged(emwpInfoFile, []byte(emwpRef.String()), 0644); err != nil {
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

// checkExistingArtifacts decides whether artifacts already on disk for
// base may satisfy a wrap request for schema want. It returns reuse=true
// only for a complete, schema-matching set (.mpk and .info both present,
// sidecar matching — an absent sidecar means schema 1). A complete set of
// another schema and any partial set fail loudly: existing artifacts are
// never silently reused across schemas, rebuilt over, or overwritten.
func checkExistingArtifacts(base string, want int) (bool, error) {
	var found, missing []string
	complete := true
	for _, suffix := range []string{".mpk", ".info", ".emwp", ".emwp.info"} {
		if _, err := os.Stat(base + suffix); err == nil {
			found = append(found, suffix)
		} else if !os.IsNotExist(err) {
			return false, err
		} else if suffix == ".mpk" || suffix == ".info" {
			complete = false
			missing = append(missing, suffix)
		}
	}
	got, sidecarPresent, err := readSchemaSidecar(base + ".schema")
	if err != nil {
		return false, err
	}
	if len(found) == 0 {
		// At most a dangling sidecar from a run that crashed before
		// publishing any artifact: it guards nothing, any schema may
		// proceed and republish it.
		return false, nil
	}
	if !complete {
		return false, fmt.Errorf("found a partial artifact set for %s (present: %s; missing: %s): "+
			"refusing to rebuild over it; --delete the revision to repack", base,
			strings.Join(found, " "), strings.Join(missing, " "))
	}
	if !sidecarPresent {
		got = 1 // artifacts predate sidecars
	}
	if got != want {
		return false, fmt.Errorf("existing artifacts %s.* were packed under schema %d, but schema %d was requested: "+
			"refusing to reuse or overwrite them; --delete the revision or move the artifacts aside to repack", base, got, want)
	}
	return true, nil
}

// readSchemaSidecar reads a <revision>.schema sidecar. present reports
// whether the file exists: artifacts packed before sidecars existed have
// none and mean schema 1, but only the caller knows whether artifacts
// exist at all.
func readSchemaSidecar(path string) (id int, present bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	id, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || id < 1 {
		return 0, true, fmt.Errorf("invalid schema sidecar %s: %q", path, data)
	}
	return id, true, nil
}

// backfillSchemaSidecar records the schema id next to reused artifacts
// that predate sidecars; an existing sidecar is left untouched.
func backfillSchemaSidecar(base string, id int) error {
	if _, present, err := readSchemaSidecar(base + ".schema"); err != nil || present {
		return err
	}
	return writeStaged(base+".schema", []byte(strconv.Itoa(id)), 0644)
}

// stagedPath creates a unique empty temp sibling of path (path.tmp.<rand>)
// with the given mode and returns its name. Uniqueness comes from
// os.CreateTemp's exclusive create, never from the PID: the packer
// container always runs as pid 1.
func stagedPath(path string, mode os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	err = f.Chmod(mode)
	if err2 := f.Close(); err == nil {
		err = err2
	}
	if err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// writeStaged atomically replaces path with data via a staged temp file.
func writeStaged(path string, data []byte, mode os.FileMode) error {
	tmp, err := stagedPath(path, mode)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// acquireArtifactLock takes an exclusive advisory flock on path,
// serializing all modelwrap writers for one (model, revision) on this
// host. The release func drops the lock. The re-stat loop handles a
// concurrent --delete unlinking the lock file between open and flock.
func acquireArtifactLock(path string) (release func(), err error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return nil, err
		}
		if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if err != syscall.EWOULDBLOCK {
				f.Close()
				return nil, fmt.Errorf("locking %s: %w", path, err)
			}
			fmt.Printf("Waiting for a concurrent modelwrap run holding %s\n", path)
			if err := flock(f, syscall.LOCK_EX); err != nil {
				f.Close()
				return nil, fmt.Errorf("locking %s: %w", path, err)
			}
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		pi, err := os.Stat(path)
		if err == nil && os.SameFile(fi, pi) {
			return func() { f.Close() }, nil
		}
		// The locked inode was unlinked (or replaced) by a concurrent
		// --delete; retry on the current file.
		f.Close()
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if err != syscall.EINTR {
			return err
		}
	}
}

// buildMWP builds a fresh EROFS image plus dm-verity hash tree for base,
// entirely in a staged temp file, and returns the temp path and artifact
// ref. It never resumes a partial build: the temp name is exclusive and
// mkfs truncates it, so the hash offset derived from the file size only
// ever describes bytes this run wrote. The tree is built natively (see
// modelwrap.FormatVerityHashTree), byte-identical to the veritysetup
// format invocation it replaced, but hashing data blocks on all cores.
func buildMWP(schema modelwrap.PackSchema, model, modelDir, base string) (string, string, error) {
	if _, err := os.Stat(schema.MkfsPath); err != nil {
		return "", "", fmt.Errorf("schema %d mkfs.erofs not found at %s (run inside the packer image): %w", schema.ID, schema.MkfsPath, err)
	}
	tmp, err := stagedPath(base+".mpk", 0644)
	if err != nil {
		return "", "", err
	}
	fail := func(err error) (string, string, error) {
		os.Remove(tmp)
		return "", "", err
	}

	args := schema.MkfsArgs(model, tmp, modelDir)
	fmt.Printf("Creating EROFS image %s.mpk\n+ %s %s\n", base, schema.MkfsPath, strings.Join(args, " "))
	if err := run(exec.Command(schema.MkfsPath, args...)); err != nil {
		return fail(err)
	}

	fi, err := os.Stat(tmp)
	if err != nil {
		return fail(err)
	}
	offset := (fi.Size() + 4095) / 4096 * 4096
	if fi.Size() != offset {
		// The strict consumer derives the data size from the hash offset
		// (data area == hashOffset bytes), which requires block alignment.
		// EROFS images are always 4K-aligned, so this should never fire.
		return fail(fmt.Errorf("EROFS image size %d is not a multiple of %d", fi.Size(), modelwrap.VerityDataBlockSize))
	}
	verityUUID := modelwrap.UUIDv5URL(model + "-inner")

	workers := runtime.GOMAXPROCS(0)
	fmt.Printf("Building verity hash tree for %s (Go, %d workers)\n", tmp, workers)
	rootHash, err := modelwrap.FormatVerityHashTree(tmp, modelwrap.VerityFormatParams{
		Salt:       modelwrap.VeritySalt(model),
		UUID:       verityUUID,
		DataBlocks: uint64(offset) / modelwrap.VerityDataBlockSize,
		HashOffset: uint64(offset),
		Workers:    workers,
	})
	if err != nil {
		return fail(fmt.Errorf("formatting dm-verity hash tree: %w", err))
	}
	fmt.Printf("Root hash: %s\n", hex.EncodeToString(rootHash))
	return tmp, fmt.Sprintf("%s_%d_%s", hex.EncodeToString(rootHash), offset, verityUUID), nil
}

// publishArtifacts renames the staged build into place, under the artifact
// lock. The order matters for crash recovery: the sidecar lands first (a
// dangling sidecar guards nothing and is overwritable) and the info file
// last — it is the commit record, so any interrupted publish leaves a set
// that checkExistingArtifacts refuses as partial rather than reuses.
func publishArtifacts(base string, schemaID int, tmpMWP, ref string) error {
	if err := writeStaged(base+".schema", []byte(strconv.Itoa(schemaID)), 0644); err != nil {
		return err
	}
	if err := testCrashAfter(".schema"); err != nil {
		return err
	}
	if err := os.Rename(tmpMWP, base+".mpk"); err != nil {
		return err
	}
	if err := testCrashAfter(".mpk"); err != nil {
		return err
	}
	// 0600 matches the mode veritysetup gave its --root-hash-file.
	return writeStaged(base+".info", []byte(ref), 0600)
}

// testCrashAfter simulates a crash between publish steps when the
// integration suite sets MODELWRAP_TEST_CRASH_AFTER to one of them.
func testCrashAfter(step string) error {
	if os.Getenv("MODELWRAP_TEST_CRASH_AFTER") == step {
		return fmt.Errorf("injected crash after publishing %s", step)
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

	dmKey, err := modelwrap.DeriveKey(masterKey, ref)
	if err != nil {
		return err
	}
	defer clear(dmKey)

	tmpFile, err := stagedPath(emwpFile, 0644)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile)

	fmt.Printf("Creating EMWP GPT image %s\n", emwpFile)
	if err := os.Truncate(tmpFile, totalSectors*modelwrap.GPTSectorSize); err != nil {
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

	plainFile, err := stagedPath(emwpFile+".verify", 0600)
	if err != nil {
		return err
	}
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

func run(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return nil
}
