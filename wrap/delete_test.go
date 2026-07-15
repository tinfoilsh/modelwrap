package wrap

import (
	"os"
	"path/filepath"
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
		filepath.Join(modelDir, "rev2.mpk"),
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
		filepath.Join(cacheModelDir, "rev1"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, got %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(modelDir, "rev2.mpk"),
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
	} {
		if err := Delete(DeleteOptions{Model: model, OutputDir: t.TempDir(), CacheDir: t.TempDir()}); err == nil {
			t.Errorf("Delete(%q) should fail", model)
		}
	}
}
