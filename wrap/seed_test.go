package wrap

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeFiles writes fixtures and chmods them to exactly 0o644 so tests are
// deterministic under any umask.
func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustSameFile(t *testing.T, a, b string) {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fa, fb) {
		t.Errorf("expected %s to be a hardlink of %s", a, b)
	}
}

func TestSeedFilesHardlinksOnlyVerifiedMatches(t *testing.T) {
	cache := t.TempDir()
	sibling := filepath.Join(cache, "revA")
	target := filepath.Join(cache, "revB")
	good := []byte("good weights content")
	nested := []byte("nested shard content")
	writeFiles(t, sibling, map[string][]byte{
		"weights.bin":       good,
		"sub/shard.bin":     nested,
		"corrupt.bin":       []byte("BAD but right sized!"),
		"truncated.bin":     []byte("short"),
		"already-there.bin": []byte("sibling copy"),
	})
	writeFiles(t, target, map[string][]byte{
		"already-there.bin": []byte("target copy"),
	})

	files := []lfsFile{
		{Path: "weights.bin", SHA256: sha256Hex(good), Size: int64(len(good))},
		{Path: "sub/shard.bin", SHA256: sha256Hex(nested), Size: int64(len(nested))},
		{Path: "corrupt.bin", SHA256: sha256Hex([]byte("expected other data!")), Size: 20},
		{Path: "truncated.bin", SHA256: sha256Hex([]byte("full content")), Size: 12},
		{Path: "missing.bin", SHA256: sha256Hex([]byte("never downloaded")), Size: 16},
		{Path: "already-there.bin", SHA256: sha256Hex([]byte("sibling copy")), Size: 12},
	}
	linked, linkedBytes, sources, err := seedFiles(files, []string{sibling}, target, 0o644)
	if err != nil {
		t.Fatal(err)
	}

	if linked != 2 {
		t.Errorf("linked = %d, want 2", linked)
	}
	if want := int64(len(good) + len(nested)); linkedBytes != want {
		t.Errorf("linkedBytes = %d, want %d", linkedBytes, want)
	}
	if len(sources) != 1 || sources[0] != "revA" {
		t.Errorf("sources = %v, want [revA]", sources)
	}
	mustSameFile(t, filepath.Join(target, "weights.bin"), filepath.Join(sibling, "weights.bin"))
	mustSameFile(t, filepath.Join(target, "sub/shard.bin"), filepath.Join(sibling, "sub/shard.bin"))
	for _, name := range []string{"corrupt.bin", "truncated.bin", "missing.bin"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Errorf("%s should not have been seeded", name)
		}
	}
	// A file already in the target dir is left for hf to resolve.
	content, err := os.ReadFile(filepath.Join(target, "already-there.bin"))
	if err != nil || string(content) != "target copy" {
		t.Errorf("already-there.bin overwritten: %q, %v", content, err)
	}
}

// A candidate whose content does not hash to the expected SHA256 is
// verified through the already-created link (so the checked inode is the
// pinned one) and the link is removed again, leaving the sibling intact.
func TestSeedFilesUnlinksHashMismatch(t *testing.T) {
	cache := t.TempDir()
	sibling := filepath.Join(cache, "revA")
	target := filepath.Join(cache, "revB")
	bad := []byte("right size, bad data")
	writeFiles(t, sibling, map[string][]byte{"weights.bin": bad})

	files := []lfsFile{{Path: "weights.bin", SHA256: sha256Hex([]byte("the expected contents")), Size: int64(len(bad))}}
	linked, linkedBytes, sources, err := seedFiles(files, []string{sibling}, target, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 0 || linkedBytes != 0 || len(sources) != 0 {
		t.Errorf("linked = %d, %d bytes, sources %v; want nothing seeded", linked, linkedBytes, sources)
	}
	if _, err := os.Lstat(filepath.Join(target, "weights.bin")); !os.IsNotExist(err) {
		t.Error("mismatched hardlink was not removed from the target dir")
	}
	got, err := os.ReadFile(filepath.Join(sibling, "weights.bin"))
	if err != nil || string(got) != string(bad) {
		t.Errorf("sibling file disturbed: %q, %v", got, err)
	}
}

// mkfs.erofs stores permission bits, so a sibling copy whose mode differs
// from what a fresh download would get today (probed at runtime) must not
// be seeded even if the content matches: it would break seeded==cold byte
// identity.
func TestSeedFilesSkipsNonStandardMode(t *testing.T) {
	cache := t.TempDir()
	sibling := filepath.Join(cache, "revA")
	target := filepath.Join(cache, "revB")
	content := []byte("correct contents")
	writeFiles(t, sibling, map[string][]byte{"weights.bin": content})

	mode, err := probeDownloadMode(target)
	if err != nil {
		t.Fatal(err)
	}
	// A mode from a different umask era: flip the group/world read bits so
	// it differs from the probed mode regardless of the test environment.
	if err := os.Chmod(filepath.Join(sibling, "weights.bin"), mode^0o044); err != nil {
		t.Fatal(err)
	}

	files := []lfsFile{{Path: "weights.bin", SHA256: sha256Hex(content), Size: int64(len(content))}}
	if linked, _, _, err := seedFiles(files, []string{sibling}, target, mode); err != nil || linked != 0 {
		t.Errorf("linked = %d (err %v), want 0 for mode-drifted sibling", linked, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "weights.bin")); !os.IsNotExist(err) {
		t.Error("mode-drifted sibling file should not have been seeded")
	}
}

// An invalid candidate in the newest sibling is unlinked again and must
// not stop an older sibling's valid copy from being seeded.
func TestSeedFilesTriesNextSiblingAfterMismatch(t *testing.T) {
	cache := t.TempDir()
	newer := filepath.Join(cache, "newer")
	older := filepath.Join(cache, "older")
	target := filepath.Join(cache, "target")
	content := []byte("the expected contents")
	writeFiles(t, newer, map[string][]byte{"weights.bin": []byte("right size, bad data!")})
	writeFiles(t, older, map[string][]byte{"weights.bin": content})

	files := []lfsFile{{Path: "weights.bin", SHA256: sha256Hex(content), Size: int64(len(content))}}
	linked, _, sources, err := seedFiles(files, []string{newer, older}, target, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 1 || len(sources) != 1 || sources[0] != "older" {
		t.Errorf("linked = %d, sources = %v; want 1 file from older", linked, sources)
	}
	mustSameFile(t, filepath.Join(target, "weights.bin"), filepath.Join(older, "weights.bin"))
}

// If an unverified link cannot be removed again, seeding must fail the
// wrap instead of leaving a file hf may trust unverified.
func TestSeedFilesFatalWhenUnverifiedFileUnremovable(t *testing.T) {
	oldRemove := removeSeedTarget
	removeSeedTarget = func(string) error { return fmt.Errorf("injected remove failure") }
	defer func() { removeSeedTarget = oldRemove }()

	cache := t.TempDir()
	sibling := filepath.Join(cache, "revA")
	target := filepath.Join(cache, "revB")
	bad := []byte("right size, bad data")
	writeFiles(t, sibling, map[string][]byte{"weights.bin": bad})

	files := []lfsFile{{Path: "weights.bin", SHA256: sha256Hex([]byte("the expected contents")), Size: int64(len(bad))}}
	linked, _, _, err := seedFiles(files, []string{sibling}, target, 0o644)
	if err == nil || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("err = %v, want fatal unverified-file error", err)
	}
	if linked != 0 {
		t.Errorf("linked = %d, want 0 on fatal error", linked)
	}
}

func TestSeedFilesPrefersMostRecentSibling(t *testing.T) {
	cache := t.TempDir()
	content := []byte("identical everywhere")
	older := filepath.Join(cache, "older")
	newer := filepath.Join(cache, "newer")
	target := filepath.Join(cache, "target")
	writeFiles(t, older, map[string][]byte{"weights.bin": content})
	writeFiles(t, newer, map[string][]byte{"weights.bin": content})
	if err := os.Chtimes(older, time.Time{}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	siblings, err := siblingRevisionDirs(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 2 || siblings[0] != newer || siblings[1] != older {
		t.Fatalf("siblings = %v, want [%s %s]", siblings, newer, older)
	}

	files := []lfsFile{{Path: "weights.bin", SHA256: sha256Hex(content), Size: int64(len(content))}}
	linked, _, sources, err := seedFiles(files, siblings, target, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 1 || len(sources) != 1 || sources[0] != "newer" {
		t.Errorf("linked = %d, sources = %v, want 1 file from newer", linked, sources)
	}
	mustSameFile(t, filepath.Join(target, "weights.bin"), filepath.Join(newer, "weights.bin"))
}

func TestSiblingRevisionDirsMissingParent(t *testing.T) {
	siblings, err := siblingRevisionDirs(filepath.Join(t.TempDir(), "model", "revision"))
	if err != nil || siblings != nil {
		t.Errorf("siblingRevisionDirs = %v, %v; want nil, nil", siblings, err)
	}
}

// Deleting a seeded-from revision must not disturb revisions that hardlink
// its files, and vice versa: only the directory entry goes away.
func TestDeleteLeavesSeededSiblingIntact(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache")
	sibling := filepath.Join(cache, "org", "model", "revA")
	target := filepath.Join(cache, "org", "model", "revB")
	content := []byte("shared weights")
	writeFiles(t, sibling, map[string][]byte{"weights.bin": content})

	files := []lfsFile{{Path: "weights.bin", SHA256: sha256Hex(content), Size: int64(len(content))}}
	if linked, _, _, err := seedFiles(files, []string{sibling}, target, 0o644); err != nil || linked != 1 {
		t.Fatal("seeding failed")
	}

	err := Delete(DeleteOptions{Model: "org/model@revA", CacheDir: cache, OutputDir: filepath.Join(dir, "output")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sibling); !os.IsNotExist(err) {
		t.Error("revA cache should be gone")
	}
	got, err := os.ReadFile(filepath.Join(target, "weights.bin"))
	if err != nil || string(got) != string(content) {
		t.Errorf("seeded file damaged by sibling delete: %q, %v", got, err)
	}
}

func TestListLFSFiles(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/models/test-org/test-model/tree/testrev" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?recursive=true&cursor=abc>; rel="next"`, server.URL, r.URL.Path))
			fmt.Fprint(w, `[
				{"type":"directory","oid":"d1","path":"sub"},
				{"type":"file","oid":"aaaa","size":100,"path":"config.json"},
				{"type":"file","oid":"bbbb","size":7,"path":"a.bin","lfs":{"oid":"1111","size":7,"pointerSize":130}}
			]`)
			return
		}
		fmt.Fprint(w, `[
			{"type":"file","oid":"cccc","size":9,"path":"sub/b.bin","lfs":{"oid":"2222","size":9,"pointerSize":130}},
			{"type":"file","oid":"dddd","size":5,"path":"../evil","lfs":{"oid":"3333","size":5,"pointerSize":130}}
		]`)
	}))
	defer server.Close()
	oldBase := hfAPIBase
	hfAPIBase = server.URL
	defer func() { hfAPIBase = oldBase }()

	files, err := listLFSFiles("test-org/test-model", "testrev", "test-token")
	if err != nil {
		t.Fatal(err)
	}
	want := []lfsFile{
		{Path: "a.bin", SHA256: "1111", Size: 7},
		{Path: "sub/b.bin", SHA256: "2222", Size: 9},
	}
	if len(files) != len(want) {
		t.Fatalf("files = %+v, want %+v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("files[%d] = %+v, want %+v", i, files[i], want[i])
		}
	}
}

func TestListLFSFilesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gated", http.StatusUnauthorized)
	}))
	defer server.Close()
	oldBase := hfAPIBase
	hfAPIBase = server.URL
	defer func() { hfAPIBase = oldBase }()

	if _, err := listLFSFiles("org/gated-model", "rev", ""); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
