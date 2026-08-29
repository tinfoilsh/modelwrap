package wrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadModelUsesSharedHubCacheAcrossRevisions(t *testing.T) {
	work := t.TempDir()
	binDir := filepath.Join(work, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(work, "hf.log")
	fakeHF := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$HF_TEST_LOG"
revision=
cache=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --revision) revision=$2; shift 2 ;;
    --cache-dir) cache=$2; shift 2 ;;
    *) shift ;;
  esac
done
blobs="$cache/models--org--model/blobs"
snapshot="$cache/models--org--model/snapshots/$revision"
mkdir -p "$blobs" "$snapshot"
if [ ! -f "$blobs/weights" ]; then
  printf 'unchanged weights' > "$blobs/weights"
  printf 'downloaded\n' >> "$HF_TEST_LOG"
fi
printf 'config %s' "$revision" > "$blobs/config-$revision"
ln -sf ../../blobs/weights "$snapshot/weights.bin"
ln -sf ../../blobs/config-$revision "$snapshot/config.json"
printf '%s\n' "$snapshot"
`
	if err := os.WriteFile(filepath.Join(binDir, "hf"), []byte(fakeHF), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HF_TEST_LOG", logFile)

	cacheDir := filepath.Join(work, "cache")
	for _, revision := range []string{"rev1", "rev2"} {
		dir := filepath.Join(cacheDir, "org", "model", revision)
		if err := downloadModel("org/model", revision, dir, cacheDir, ""); err != nil {
			t.Fatalf("downloadModel(%s): %v", revision, err)
		}
		for name, want := range map[string]string{
			"weights.bin": "unchanged weights",
			"config.json": "config " + revision,
		} {
			path := filepath.Join(dir, name)
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatalf("%s = %q, want %q", path, got, want)
			}
			if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("materialized file %s is not a regular file", path)
			}
		}
	}

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	wantCacheArg := "--cache-dir " + filepath.Join(cacheDir, "huggingface", "hub")
	if strings.Count(log, wantCacheArg) != 2 {
		t.Fatalf("hf invocations did not share cache %q:\n%s", wantCacheArg, log)
	}
	if strings.Count(log, "downloaded\n") != 1 {
		t.Fatalf("unchanged weight blob was downloaded more than once:\n%s", log)
	}
}
