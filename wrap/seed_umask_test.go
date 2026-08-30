//go:build unix

package wrap

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Reused caches outlive umask configuration: a wrap running under a
// stricter umask must probe its own download mode and reject sibling
// copies from a 022-era cache, seeding only what a cold download would
// reproduce today. Umask is process-wide, which is safe to toggle here:
// no test in this package runs in parallel.
func TestSeedingUnderNonDefaultUmask(t *testing.T) {
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)

	cache := t.TempDir()
	sibling := filepath.Join(cache, "revA")
	target := filepath.Join(cache, "revB")
	oldEra := []byte("downloaded under umask 022")
	current := []byte("downloaded under umask 077")
	writeFiles(t, sibling, map[string][]byte{
		"old-era.bin": oldEra,
		"current.bin": current,
	})
	if err := os.Chmod(filepath.Join(sibling, "current.bin"), 0o600); err != nil {
		t.Fatal(err)
	}

	mode, err := probeDownloadMode(target)
	if err != nil {
		t.Fatal(err)
	}
	if mode != 0o600 {
		t.Fatalf("probed download mode = %o under umask 077, want 600", mode)
	}
	if _, err := os.Lstat(filepath.Join(target, ".cache", ".seed-mode-probe")); !os.IsNotExist(err) {
		t.Error("probe file left behind")
	}

	files := []lfsFile{
		{Path: "old-era.bin", SHA256: sha256Hex(oldEra), Size: int64(len(oldEra))},
		{Path: "current.bin", SHA256: sha256Hex(current), Size: int64(len(current))},
	}
	linked, _, _, err := seedFiles(files, []string{sibling}, target, mode)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 1 {
		t.Errorf("linked = %d, want only the mode-matching candidate", linked)
	}
	if _, err := os.Lstat(filepath.Join(target, "old-era.bin")); !os.IsNotExist(err) {
		t.Error("0644-era candidate seeded despite the 077 umask")
	}
	mustSameFile(t, filepath.Join(target, "current.bin"), filepath.Join(sibling, "current.bin"))
}
