package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteRemovesOnlyPinnedRevision(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	cacheDir := filepath.Join(dir, "cache")
	modelDir := filepath.Join(outputDir, "org", "model")
	cacheModelDir := filepath.Join(cacheDir, "org", "model")
	for _, path := range []string{
		filepath.Join(modelDir, "rev1.mpk"),
		filepath.Join(modelDir, "rev1.info"),
		filepath.Join(modelDir, "rev1.emwp"),
		filepath.Join(modelDir, "rev1.emwp.info"),
		filepath.Join(modelDir, "rev1.emwp.verify.tmp"),
		filepath.Join(modelDir, "rev1.schema"),
		filepath.Join(modelDir, "rev1..staged.mpk.12345"),
		filepath.Join(modelDir, "rev1..staged.emwp.info.999"),
		filepath.Join(modelDir, "rev2.mpk"),
		filepath.Join(modelDir, "rev2..staged.mpk.777"),
		filepath.Join(modelDir, "rev1.evil..staged.mpk.5"),
		filepath.Join(cacheModelDir, "rev1", "weights.bin"),
		filepath.Join(cacheModelDir, "rev2", "weights.bin"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	opts := DeleteOptions{Model: "org/model@rev1", OutputDir: outputDir, CacheDir: cacheDir}
	if err := Delete(opts); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Retrying an already-completed deletion must remain safe.
	if err := Delete(opts); err != nil {
		t.Fatalf("Delete retry: %v", err)
	}

	for _, path := range []string{
		filepath.Join(modelDir, "rev1.mpk"),
		filepath.Join(modelDir, "rev1.info"),
		filepath.Join(modelDir, "rev1.emwp"),
		filepath.Join(modelDir, "rev1.emwp.info"),
		filepath.Join(modelDir, "rev1.emwp.verify.tmp"),
		filepath.Join(modelDir, "rev1.schema"),
		filepath.Join(modelDir, "rev1..staged.mpk.12345"),
		filepath.Join(modelDir, "rev1..staged.emwp.info.999"),
		filepath.Join(cacheModelDir, "rev1"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, got %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(modelDir, "rev2.mpk"),
		filepath.Join(modelDir, "rev2..staged.mpk.777"),
		filepath.Join(modelDir, "rev1.evil..staged.mpk.5"),
		filepath.Join(cacheModelDir, "rev2", "weights.bin"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected sibling revision %s to remain: %v", path, err)
		}
	}
}

func TestDeleteRequiresSafePinnedModel(t *testing.T) {
	for _, model := range []string{
		"org/model",
		"org/model@",
		"org/model@../revision",
		"../model@revision",
		`org\model@revision`,
		"org/model@rev*",
		"org/model@re?v",
		"org/model@rev[1",
		"org/mo*del@rev",
		"org/model@a..b",
	} {
		if err := Delete(DeleteOptions{Model: model, OutputDir: t.TempDir(), CacheDir: t.TempDir()}); err == nil {
			t.Errorf("Delete(%q) should fail", model)
		}
	}
}

// TestDeleteRejectsMetacharactersUntouched: a revision containing pattern
// metacharacters is rejected before anything is removed — it must never
// expand across sibling revisions' files (a live sibling build holds a
// different revision's lock, so nothing else protects them).
func TestDeleteRejectsMetacharactersUntouched(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	modelDir := filepath.Join(outputDir, "org", "model")
	files := []string{
		"foo*.mpk", "foo*..staged.mpk.1", // legal unix names for the weird revision itself
		"rev1.mpk", "rev1..staged.mpk.2", // siblings a glob for foo* could reach
		"foox..staged.mpk.3", // what a ? pattern would reach
	}
	for _, name := range files {
		path := filepath.Join(modelDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := Delete(DeleteOptions{Model: "org/model@foo*", OutputDir: outputDir, CacheDir: filepath.Join(dir, "cache")})
	if err == nil || !strings.Contains(err.Error(), "metacharacters") {
		t.Fatalf("Delete = %v, want metacharacter rejection", err)
	}
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(modelDir, name)); err != nil {
			t.Errorf("rejected delete touched %s: %v", name, err)
		}
	}
}

func TestIsStagedLeftover(t *testing.T) {
	for name, want := range map[string]bool{
		"rev1..staged.mpk.12345":     true,
		"rev1..staged.emwp.info.9":   true,
		"rev1..staged.emwp.verify.9": true,
		"rev1..staged.schema.4":      true,
		"rev1.mpk":                   false, // published artifact, not staged
		"rev1.mpk.tmp":               false, // legacy fixed name, removed explicitly
		"rev1..staged.":              false, // no suffix or random part
		"rev2..staged.mpk.7":         false, // sibling revision
		"rev1.evil..staged.mpk.5":    false, // dot-sibling revision
		"rev1.lock":                  false,
	} {
		if got := isStagedLeftover(name, "rev1"); got != want {
			t.Errorf("isStagedLeftover(%q) = %v, want %v", name, got, want)
		}
	}

	// The collision the ".." anchor exists to prevent: a revision named
	// "foo.emwp" staging its .info must never look like revision "foo"'s
	// staged .emwp.info, in either direction.
	if isStagedLeftover("foo.emwp..staged.info.7", "foo") {
		t.Error(`revision "foo.emwp"'s staged .info matched revision "foo"`)
	}
	if isStagedLeftover("foo..staged.emwp.info.7", "foo.emwp") {
		t.Error(`revision "foo"'s staged .emwp.info matched revision "foo.emwp"`)
	}
	if !isStagedLeftover("foo.emwp..staged.info.7", "foo.emwp") {
		t.Error(`revision "foo.emwp" failed to match its own staged .info`)
	}
}

// TestDeleteDottedSiblingStagedFiles: deleting revision "foo" must leave
// dotted sibling "foo.emwp"'s staged files untouched, and vice versa.
func TestDeleteDottedSiblingStagedFiles(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "output")
	modelDir := filepath.Join(outputDir, "org", "model")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foo := []string{"foo.mpk", "foo..staged.mpk.1", "foo..staged.emwp.info.2"}
	fooEmwp := []string{"foo.emwp.mpk", "foo.emwp..staged.info.3", "foo.emwp..staged.mpk.4"}
	for _, name := range append(append([]string{}, foo...), fooEmwp...) {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	opts := func(revision string) DeleteOptions {
		return DeleteOptions{Model: "org/model@" + revision, OutputDir: outputDir, CacheDir: filepath.Join(dir, "cache")}
	}
	if err := Delete(opts("foo")); err != nil {
		t.Fatalf("Delete(foo): %v", err)
	}
	for _, name := range []string{"foo..staged.mpk.1", "foo..staged.emwp.info.2"} {
		if _, err := os.Stat(filepath.Join(modelDir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got %v", name, err)
		}
	}
	for _, name := range fooEmwp {
		if _, err := os.Stat(filepath.Join(modelDir, name)); err != nil {
			t.Errorf("Delete(foo) touched dotted sibling file %s: %v", name, err)
		}
	}

	if err := Delete(opts("foo.emwp")); err != nil {
		t.Fatalf("Delete(foo.emwp): %v", err)
	}
	for _, name := range fooEmwp {
		if _, err := os.Stat(filepath.Join(modelDir, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, got %v", name, err)
		}
	}
}
