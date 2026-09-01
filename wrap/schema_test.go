package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tinfoilsh/modelwrap"
)

// validRef is a syntactically valid rootHash_hashOffset_uuid for fixtures.
const validRef = "17c6605f3becf63a63da37cc6958b2d18f8abc750f9f546b9f57133db40c4326_2142208_9701bf21-6de2-59a2-bdea-141aaae05fc3"

// packFixture creates a local model dir plus pre-existing output files for
// its revision (suffix -> content), returning the pack options and the
// artifact base path.
func packFixture(t *testing.T, files map[string]string) (Options, string) {
	t.Helper()
	work := t.TempDir()
	modelDir := filepath.Join(work, "model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "weights.bin"), []byte("weights"), 0644); err != nil {
		t.Fatal(err)
	}
	revision, err := modelwrap.HashDir(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(work, "output", "model", revision)
	if err := os.MkdirAll(filepath.Dir(base), 0755); err != nil {
		t.Fatal(err)
	}
	for suffix, content := range files {
		if err := os.WriteFile(base+suffix, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return Options{
		Model:     "model",
		ModelDir:  modelDir,
		CacheDir:  filepath.Join(work, "cache"),
		OutputDir: filepath.Join(work, "output"),
	}, base
}

func completeSet(sidecar string) map[string]string {
	files := map[string]string{".mpk": "mpk", ".info": validRef}
	if sidecar != "" {
		files[".schema"] = sidecar
	}
	return files
}

// TestPackRejectsSchemaMismatch: a complete artifact set satisfies a wrap
// request only under the same schema; a different schema must fail loudly
// before touching it, never silently return the other schema's ref.
func TestPackRejectsSchemaMismatch(t *testing.T) {
	for name, tc := range map[string]struct {
		sidecar string
		request int
	}{
		"sidecar-2-request-default": {sidecar: "2", request: 0},
		"sidecar-2-request-1":       {sidecar: "2", request: 1},
		"no-sidecar-request-2":      {sidecar: "", request: 2}, // absent sidecar = schema 1
		"sidecar-1-request-2":       {sidecar: "1", request: 2},
	} {
		t.Run(name, func(t *testing.T) {
			opts, base := packFixture(t, completeSet(tc.sidecar))
			opts.Schema = tc.request
			_, err := Pack(opts)
			if err == nil || !strings.Contains(err.Error(), "schema") {
				t.Fatalf("Pack = %v, want loud schema mismatch error", err)
			}
			// The mismatch must not have altered the existing artifacts.
			data, err := os.ReadFile(base + ".info")
			if err != nil || string(data) != validRef {
				t.Fatalf("existing info file was modified: %q, %v", data, err)
			}
		})
	}
}

// TestPackRejectsPartialArtifacts: any incomplete artifact set (missing
// .mpk or .info) is refused loudly — never rebuilt over, never reused.
func TestPackRejectsPartialArtifacts(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"mpk-only":         {".mpk": "mpk"},
		"info-only":        {".info": validRef},
		"emwp-only":        {".emwp": "emwp"},
		"mpk-with-sidecar": {".mpk": "mpk", ".schema": "1"},
		"emwp-with-trioless-info": {
			".emwp": "emwp", ".emwp.info": validRef, ".info": validRef,
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts, _ := packFixture(t, files)
			_, err := Pack(opts)
			if err == nil || !strings.Contains(err.Error(), "partial artifact set") {
				t.Fatalf("Pack = %v, want partial-artifact-set error", err)
			}
			if !strings.Contains(err.Error(), "--delete") {
				t.Fatalf("partial-set error should instruct --delete: %v", err)
			}
		})
	}
}

// TestPackReusesSameSchemaArtifacts: a complete set of the requested
// schema is reused, and the schema is backfilled into the sidecar only for
// pre-sidecar schema-1 artifacts.
func TestPackReusesSameSchemaArtifacts(t *testing.T) {
	for name, tc := range map[string]struct {
		sidecar string
		request int
	}{
		"no-sidecar-request-default": {sidecar: "", request: 0},
		"no-sidecar-request-1":       {sidecar: "", request: 1},
		"sidecar-1-request-default":  {sidecar: "1", request: 0},
		"sidecar-2-request-2":        {sidecar: "2", request: 2},
	} {
		t.Run(name, func(t *testing.T) {
			opts, base := packFixture(t, completeSet(tc.sidecar))
			opts.Schema = tc.request
			ref, err := Pack(opts)
			if err != nil {
				t.Fatalf("Pack: %v", err)
			}
			if ref != validRef {
				t.Fatalf("Pack ref = %s, want %s", ref, validRef)
			}
			want := "1"
			if tc.request == 2 {
				want = "2"
			}
			data, err := os.ReadFile(base + ".schema")
			if err != nil {
				t.Fatalf("reading schema sidecar: %v", err)
			}
			if string(data) != want {
				t.Fatalf("schema sidecar = %q, want %q (plain decimal, no newline)", data, want)
			}
		})
	}
}

// TestCheckExistingArtifactsDanglingSidecar: a sidecar without any
// artifacts records intent from an interrupted publish; it guards nothing
// and must not block any schema.
func TestCheckExistingArtifactsDanglingSidecar(t *testing.T) {
	for _, request := range []int{1, 2} {
		_, base := packFixture(t, map[string]string{".schema": "2"})
		reuse, err := checkExistingArtifacts(base, request)
		if err != nil || reuse {
			t.Fatalf("dangling sidecar, request %d: (reuse=%v, err=%v), want (false, nil)", request, reuse, err)
		}
	}
}

func TestPackRejectsUnknownSchema(t *testing.T) {
	opts, _ := packFixture(t, nil)
	opts.Schema = 99
	_, err := Pack(opts)
	if err == nil || !strings.Contains(err.Error(), "unknown pack schema") {
		t.Fatalf("Pack = %v, want unknown-schema error", err)
	}
}

func TestReadSchemaSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rev.schema")

	if id, present, err := readSchemaSidecar(path); err != nil || present || id != 0 {
		t.Fatalf("absent sidecar = (%d, %v, %v), want (0, false, nil)", id, present, err)
	}
	for content, want := range map[string]int{"1": 1, "2": 2, "2\n": 2} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if id, present, err := readSchemaSidecar(path); err != nil || !present || id != want {
			t.Fatalf("sidecar %q = (%d, %v, %v), want (%d, true, nil)", content, id, present, err, want)
		}
	}
	for _, content := range []string{"", "x", "0", "-1", "1.5"} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if id, _, err := readSchemaSidecar(path); err == nil {
			t.Fatalf("sidecar %q = (%d, nil), want error", content, id)
		}
	}
}

// TestStagedPathUnique: staged temp files are created exclusively with
// random names — two stagings of the same target never collide, even in
// one process (the packer container always runs as pid 1, so PIDs cannot
// provide uniqueness) — and follow the <revision>..staged<suffix>.<rand>
// grammar whose ".." anchor no revision can contain.
func TestStagedPathUnique(t *testing.T) {
	base := filepath.Join(t.TempDir(), "rev")
	a, err := stagedPath(base, ".mpk", 0644)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stagedPath(base, ".mpk", 0600)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("stagedPath returned the same name twice: %s", a)
	}
	for path, mode := range map[string]os.FileMode{a: 0644, b: 0600} {
		prefix := base + "..staged.mpk."
		if !strings.HasPrefix(path, prefix) || len(path) == len(prefix) {
			t.Fatalf("staged path %s does not extend %s", path, prefix)
		}
		if !isStagedLeftover(filepath.Base(path), "rev") {
			t.Fatalf("Delete's matcher does not recognize staged path %s", path)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("staged file missing: %v", err)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("staged file %s mode = %v, want %v", path, fi.Mode().Perm(), mode)
		}
	}
}

// TestDeleteWaitsForArtifactLock: every removal Delete performs — the
// cache dir included, which a same-revision Pack reads during its locked
// build — happens inside the revision lock hold, so a held lock stalls
// the entire deletion.
func TestDeleteWaitsForArtifactLock(t *testing.T) {
	work := t.TempDir()
	outputModelDir := filepath.Join(work, "output", "org", "model")
	cacheRevisionDir := filepath.Join(work, "cache", "org", "model", "rev1")
	if err := os.MkdirAll(outputModelDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheRevisionDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(outputModelDir, "rev1.mpk"),
		filepath.Join(outputModelDir, "rev1.info"),
		filepath.Join(cacheRevisionDir, "weights.bin"),
	} {
		if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	release, err := acquireArtifactLock(filepath.Join(outputModelDir, "rev1.lock"))
	if err != nil {
		t.Fatal(err)
	}

	var released atomic.Bool
	done := make(chan error, 1)
	go func() {
		err := Delete(DeleteOptions{
			Model:     "org/model@rev1",
			CacheDir:  filepath.Join(work, "cache"),
			OutputDir: filepath.Join(work, "output"),
		})
		if err == nil && !released.Load() {
			err = os.ErrInvalid // deletion ran while the lock was held
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Delete completed while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Nothing may be removed while the lock is held.
	for _, path := range []string{
		filepath.Join(outputModelDir, "rev1.mpk"),
		filepath.Join(cacheRevisionDir, "weights.bin"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s removed while the lock was held: %v", path, err)
		}
	}

	released.Store(true)
	release()
	if err := <-done; err != nil {
		t.Fatalf("Delete after release: %v", err)
	}
	for _, path := range []string{
		filepath.Join(outputModelDir, "rev1.mpk"),
		filepath.Join(outputModelDir, "rev1.info"),
		cacheRevisionDir,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed after release, got %v", path, err)
		}
	}
}

// TestArtifactLockExcludes: a second acquirer of the same artifact lock
// must not enter until the first releases.
func TestArtifactLockExcludes(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "rev.lock")
	release, err := acquireArtifactLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	var released atomic.Bool
	acquired := make(chan error, 1)
	go func() {
		release2, err := acquireArtifactLock(lockPath)
		if err == nil {
			if !released.Load() {
				err = os.ErrInvalid // acquired while still held
			}
			release2()
		}
		acquired <- err
	}()

	select {
	case err := <-acquired:
		t.Fatalf("second acquire completed while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	released.Store(true)
	release()
	if err := <-acquired; err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
}
