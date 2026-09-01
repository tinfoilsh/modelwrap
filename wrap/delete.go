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
	var errs []error
	if _, err := os.Stat(outputModelDir); err == nil {
		errs = append(errs, deleteArtifacts(base)...)
	} else if !os.IsNotExist(err) {
		return err
	}

	cacheRevisionDir := filepath.Join(opts.CacheDir, filepath.FromSlash(modelName), revision)
	if err := os.RemoveAll(cacheRevisionDir); err != nil {
		errs = append(errs, fmt.Errorf("removing %s: %w", cacheRevisionDir, err))
	}

	// Prune only empty parent directories. A non-empty directory simply means
	// another revision/model still exists and is intentionally retained.
	pruneEmptyParents(outputModelDir, opts.OutputDir)
	pruneEmptyParents(filepath.Dir(cacheRevisionDir), opts.CacheDir)
	return errors.Join(errs...)
}

// deleteArtifacts removes one revision's published artifacts under the
// artifact lock, in the inverse of the publish order (.info first): a
// deletion interrupted mid-way leaves a set the packer already refuses as
// partial instead of one it would trust.
func deleteArtifacts(base string) []error {
	release, err := acquireArtifactLock(base + ".lock")
	if err != nil {
		return []error{err}
	}
	defer release()

	var errs []error
	for _, suffix := range []string{
		".emwp.info", ".emwp", ".info", ".mpk", ".schema",
		".mpk.tmp", ".info.tmp", ".emwp.tmp", ".emwp.info.tmp", ".emwp.verify.tmp",
	} {
		if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("removing %s: %w", base+suffix, err))
		}
	}
	// Staged leftovers (<artifact>.tmp.<rand>) from crashed runs. Live runs
	// hold the lock, so the glob can never unlink another job's temp file.
	if tmps, err := filepath.Glob(base + ".*.tmp.*"); err == nil {
		for _, tmp := range tmps {
			if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("removing %s: %w", tmp, err))
			}
		}
	}
	// Safe while held: acquireArtifactLock re-checks the path after locking.
	if err := os.Remove(base + ".lock"); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("removing %s: %w", base+".lock", err))
	}
	return errs
}

func splitPinnedModel(model string) (string, string, error) {
	name, revision, ok := strings.Cut(model, "@")
	if !ok || name == "" || revision == "" {
		return "", "", fmt.Errorf("model must be pinned as model@revision")
	}
	if strings.Contains(revision, "/") || strings.Contains(revision, `\`) || revision == "." || revision == ".." {
		return "", "", fmt.Errorf("invalid model revision %q", revision)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.Contains(segment, `\`) {
			return "", "", fmt.Errorf("invalid model name %q", name)
		}
	}
	return name, revision, nil
}

func pruneEmptyParents(dir, stop string) {
	stop = filepath.Clean(stop)
	for dir = filepath.Clean(dir); dir != stop && dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}
