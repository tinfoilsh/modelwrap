package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tinfoilsh/modelwrap/wrap"
)

func TestDockerRunArgs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HF_TOKEN", "secret")
	t.Setenv("PRIVATE_MODEL_KEY_B64", "")
	t.Setenv("PRIVATE_MODEL_KEY_FILE", "")
	if err := os.WriteFile(filepath.Join(dir, "master.key"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "weights"), 0755); err != nil {
		t.Fatal(err)
	}

	opts := cliOptions{
		Options: wrap.Options{
			Model:    "org/model@rev",
			ModelDir: "weights",
			KeyFile:  "master.key",
			Schema:   2,
			Encrypt:  true,
			Verify:   true,
		},
		image: "ghcr.io/tinfoilsh/modelwrap@sha256:deadbeef",
	}
	got, err := dockerRunArgs(opts)
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}

	want := []string{
		"run", "--rm",
		"-v", filepath.Join(dir, "output") + ":/output",
		"-v", filepath.Join(dir, "cache") + ":/cache",
		"-v", filepath.Join(dir, "weights") + ":/model:ro",
		"-v", filepath.Join(dir, "master.key") + ":/run/modelwrap-key:ro",
		"-e", "HF_TOKEN",
		"ghcr.io/tinfoilsh/modelwrap@sha256:deadbeef",
		"--model-dir", "/model",
		"--key-file", "/run/modelwrap-key",
		"--encrypt", "--verify",
		"--schema", "2",
		"org/model@rev",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dockerRunArgs mismatch:\n got %q\nwant %q", got, want)
	}

	// EMWP packing is fully userspace and must not request privilege.
	for _, arg := range got {
		if arg == "--privileged" {
			t.Fatal("EMWP packing must not be privileged")
		}
	}

	// Secret values must never appear in the docker command line.
	for _, arg := range got {
		if arg == "secret" {
			t.Fatal("secret value leaked into docker args")
		}
	}

	// Host directories are pre-created so docker does not own them as root.
	for _, sub := range []string{"output", "cache"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("expected %s directory to be created: %v", sub, err)
		}
	}
}

func TestDockerRunArgsPlainMWP(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HF_TOKEN", "")
	t.Setenv("PRIVATE_MODEL_KEY_B64", "")
	t.Setenv("PRIVATE_MODEL_KEY_FILE", "")

	got, err := dockerRunArgs(cliOptions{
		Options: wrap.Options{Model: "org/model@rev"},
		image:   "img",
	})
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}
	for _, arg := range got {
		if arg == "--privileged" {
			t.Fatal("plain MWP packing should not be privileged")
		}
	}
	if got[len(got)-1] != "org/model@rev" {
		t.Fatalf("expected model as final arg: %q", got)
	}
}

// TestParseArgsSchemaValidation: the --schema flag and MODELWRAP_SCHEMA
// are two doors to the same selection and must accept and reject
// identical values; an explicit flag is validated even when it equals the
// unset sentinel, and the flag overrides the env.
func TestParseArgsSchemaValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		env     string
		args    []string
		want    int
		wantErr string
	}{
		"absent-both":        {want: 0},
		"env-selects":        {env: "2", want: 2},
		"flag-selects":       {args: []string{"--schema", "2"}, want: 2},
		"flag-overrides-env": {env: "1", args: []string{"--schema", "2"}, want: 2},
		"env-zero":           {env: "0", wantErr: "invalid MODELWRAP_SCHEMA"},
		"env-negative":       {env: "-1", wantErr: "invalid MODELWRAP_SCHEMA"},
		"env-garbage":        {env: "x", wantErr: "invalid MODELWRAP_SCHEMA"},
		"flag-zero":          {args: []string{"--schema", "0"}, wantErr: "invalid --schema"},
		"flag-negative":      {args: []string{"--schema", "-1"}, wantErr: "invalid --schema"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MODELWRAP_SCHEMA", tc.env)
			opts, err := parseArgs(append(tc.args, "org/model@rev"))
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("parseArgs err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if opts.Schema != tc.want {
				t.Fatalf("Schema = %d, want %d", opts.Schema, tc.want)
			}
		})
	}
}

func TestDockerRunArgsDelete(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HF_TOKEN", "")
	t.Setenv("PRIVATE_MODEL_KEY_B64", "")
	t.Setenv("PRIVATE_MODEL_KEY_FILE", "")

	got, err := dockerRunArgs(cliOptions{
		Options: wrap.Options{Model: "org/model@rev"},
		image:   "img",
		delete:  true,
	})
	if err != nil {
		t.Fatalf("dockerRunArgs: %v", err)
	}
	wantTail := []string{"img", "--delete", "org/model@rev"}
	if !reflect.DeepEqual(got[len(got)-len(wantTail):], wantTail) {
		t.Fatalf("delete args tail = %q, want %q", got, wantTail)
	}
}
