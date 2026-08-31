package wrap

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tinfoilsh/modelwrap"
)

// validRef is a syntactically valid rootHash_hashOffset_uuid for fixtures.
const validRef = "17c6605f3becf63a63da37cc6958b2d18f8abc750f9f546b9f57133db40c4326_2142208_9701bf21-6de2-59a2-bdea-141aaae05fc3"

// packFixture creates a local model dir plus pre-existing output artifacts
// for its revision, returning the pack options and the artifact base path.
func packFixture(t *testing.T, sidecar string) (Options, string) {
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
	if err := os.WriteFile(base+".mpk", []byte("mpk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".info", []byte(validRef), 0600); err != nil {
		t.Fatal(err)
	}
	if sidecar != "" {
		if err := os.WriteFile(base+".schema", []byte(sidecar), 0644); err != nil {
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

// TestPackRejectsSchemaMismatch: existing artifacts satisfy a wrap request
// only under the same schema; a different schema must fail loudly before
// touching them, never silently return the other schema's ref.
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
			opts, base := packFixture(t, tc.sidecar)
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

// TestPackReusesSameSchemaArtifacts: existing artifacts of the requested
// schema are reused, and the resolved schema is recorded in the sidecar
// (also backfilled for pre-sidecar schema-1 artifacts).
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
			opts, base := packFixture(t, tc.sidecar)
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

func TestPackRejectsUnknownSchema(t *testing.T) {
	opts, _ := packFixture(t, "")
	opts.Schema = 99
	_, err := Pack(opts)
	if err == nil || !strings.Contains(err.Error(), "unknown pack schema") {
		t.Fatalf("Pack = %v, want unknown-schema error", err)
	}
}

func TestTmpPathProcessUnique(t *testing.T) {
	got := tmpPath("/out/rev.mpk")
	want := "/out/rev.mpk.tmp." + strconv.Itoa(os.Getpid())
	if got != want {
		t.Fatalf("tmpPath = %s, want %s", got, want)
	}
}

func TestReadSchemaSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rev.schema")

	if id, err := readSchemaSidecar(path); err != nil || id != 1 {
		t.Fatalf("absent sidecar = (%d, %v), want (1, nil)", id, err)
	}
	for content, want := range map[string]int{"1": 1, "2": 2, "2\n": 2} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if id, err := readSchemaSidecar(path); err != nil || id != want {
			t.Fatalf("sidecar %q = (%d, %v), want (%d, nil)", content, id, err, want)
		}
	}
	for _, content := range []string{"", "x", "0", "-1", "1.5"} {
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if id, err := readSchemaSidecar(path); err == nil {
			t.Fatalf("sidecar %q = (%d, nil), want error", content, id)
		}
	}
}
