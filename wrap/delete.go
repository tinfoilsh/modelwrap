package wrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DeleteOptions identifies one pinned model revision and the local modelwrap
// directories that contain its source cache and generated artifacts.
type DeleteOptions struct {
	Model     string
	CacheDir  string
	OutputDir string
}

// Delete removes every generated artifact and the downloaded source cache for
// one pinned model revision. It is idempotent so callers can safely retry after
// a partial failure.
func Delete(opts DeleteOptions) error {
	modelName, revision, err := splitPinnedModel(opts.Model)
	if err != nil {
		return err
	}
	if opts.CacheDir == "" {
		opts.CacheDir = "cache"
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "output"
	}

	outputModelDir := filepath.Join(opts.OutputDir, filepath.FromSlash(modelName))
	base := filepath.Join(outputModelDir, revision)
	cacheRevisionDir := filepath.Join(opts.CacheDir, filepath.FromSlash(modelName), revision)

	// One uninterrupted hold of the revision lock covers every removal,
	// the cache included: a same-revision Pack reads the cache dir during
	// its locked build, and ripping files out mid-mkfs would produce a
	// silently wrong artifact, not an error. The dir is created so the
	// lock always exists to take; pruneEmptyParents cleans it back up.
	//
	// The two writers outside the lock stay safe without it: a Pack still
	// in its pre-lock download phase fails loudly (hf re-verifies files
	// and errors on vanished paths, and mkfs refuses a missing dir once
	// the Pack holds the lock), and cross-revision seeding FROM this
	// cache links first and verifies the pinned target inode after, so a
	// vanished source is an ENOENT skip and an already-linked file stays
	// verified (see seedFromPreviousRevisions).
	if err := os.MkdirAll(outputModelDir, 0755); err != nil {
		return err
	}
	release, err := acquireArtifactLock(base + ".lock")
	if err != nil {
		return err
	}

	// Published artifacts go in the inverse of the publish order (.info
	// first): a deletion interrupted mid-way leaves a set the packer
	// already refuses as partial instead of one it would trust. The fixed
	// *.tmp names are exact-literal leftovers of pre-schema packer
	// releases.
	var errs []error
	for _, suffix := range []string{
		".emwp.info", ".emwp", ".info", ".mpk", ".schema",
		".mpk.tmp", ".info.tmp", ".emwp.tmp", ".emwp.info.tmp", ".emwp.verify.tmp",
	} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("removing %s: %w", base+suffix, err))
		}
	}
	// Staged leftovers (<revision>..staged<suffix>.<rand>) from crashed
	// runs, matched by literal name comparison — never by glob, which
	// would give pattern semantics to the user-supplied revision. Live
	// runs hold the lock, so this can never unlink another job's temp
	// file.
	if entries, err := os.ReadDir(outputModelDir); err != nil {
		errs = append(errs, fmt.Errorf("listing %s: %w", outputModelDir, err))
	} else {
		for _, entry := range entries {
			if !isStagedLeftover(entry.Name(), revision) {
				continue
			}
			path := filepath.Join(outputModelDir, entry.Name())
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("removing %s: %w", path, err))
			}
		}
	}
	if err := os.RemoveAll(cacheRevisionDir); err != nil {
		errs = append(errs, fmt.Errorf("removing %s: %w", cacheRevisionDir, err))
	}
	// Safe while held: acquireArtifactLock re-checks the path after locking.
	if err := os.Remove(base + ".lock"); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing %s: %w", base+".lock", err))
	}
	release()

	// Prune only empty parent directories. A non-empty directory simply means
	// another revision/model still exists and is intentionally retained.
	pruneEmptyParents(outputModelDir, opts.OutputDir)
	pruneEmptyParents(filepath.Dir(cacheRevisionDir), opts.CacheDir)
	return errors.Join(errs...)
}

// isStagedLeftover reports whether name is a staged temp file of exactly
// this revision (the <revision>..staged<suffix>.<rand> shape stagedPath
// creates), by literal string comparison only. The grammar is unambiguous
// by construction: revisions never contain ".." (git-invalid, and
// splitPinnedModel rejects it), so the "..staged" token can only ever
// terminate this exact revision — a suffix enumeration would let a dotted
// revision like "foo.emwp" alias another revision's staged files.
func isStagedLeftover(name, revision string) bool {
	prefix := revision + "..staged."
	return strings.HasPrefix(name, prefix) && len(name) > len(prefix)
}

func splitPinnedModel(model string) (string, string, error) {
	name, revision, ok := strings.Cut(model, "@")
	if !ok || name == "" || revision == "" {
		return "", "", fmt.Errorf("model must be pinned as model@revision")
	}
	if err := validatePinnedModel(name, revision); err != nil {
		return "", "", err
	}
	return name, revision, nil
}

// validatePinnedModel is the one gate for user-supplied model identities:
// both the packer (before it writes anything) and --delete enforce it, so
// nothing can be packed that deletion would later reject. Legitimate ids
// pass trivially — HF names and git refnames forbid everything rejected
// here, and hash-derived local revisions are plain hex.
func validatePinnedModel(name, revision string) error {
	// Pattern metacharacters never appear in legitimate model names or
	// revisions; rejecting them keeps every path derived from user input
	// free of glob semantics.
	if strings.ContainsAny(name+revision, `*?[`) {
		return fmt.Errorf("invalid model %q: pattern metacharacters are not allowed", name+"@"+revision)
	}
	// ".." never appears in a git refname; rejecting it keeps the staged
	// file grammar (<revision>..staged<suffix>.<rand>) unambiguous — a
	// revision like "x..staged" would alias revision "x"'s staged files.
	if strings.Contains(revision, "..") {
		return fmt.Errorf("invalid model revision %q: '..' is not allowed", revision)
	}
	if strings.Contains(revision, "/") || strings.Contains(revision, `\`) || revision == "." || revision == "" {
		return fmt.Errorf("invalid model revision %q", revision)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.Contains(segment, `\`) {
			return fmt.Errorf("invalid model name %q", name)
		}
	}
	return nil
}

func pruneEmptyParents(dir, stop string) {
	stop = filepath.Clean(stop)
	for dir = filepath.Clean(dir); dir != stop && dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}
