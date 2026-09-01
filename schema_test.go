package modelwrap

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSchemaRegistry(t *testing.T) {
	if DefaultSchema != 1 {
		t.Fatalf("DefaultSchema = %d, want 1: the default flips only as an explicit release decision", DefaultSchema)
	}
	def, err := SchemaByID(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaByID(DefaultSchema): %v", err)
	}
	if !def.Stable {
		t.Fatal("the default schema must be stable")
	}

	if _, err := SchemaByID(99); err == nil || !strings.Contains(err.Error(), "unknown pack schema") {
		t.Fatalf("SchemaByID(99) = %v, want unknown-schema error", err)
	}
	if _, err := SchemaByID(0); err == nil {
		t.Fatal("SchemaByID(0) should fail: 0 is not a schema id")
	}

	if got, want := SchemaIDs(), []int{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("SchemaIDs() = %v, want %v", got, want)
	}
	seenPaths := map[string]int{}
	for _, id := range SchemaIDs() {
		s, err := SchemaByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if s.ID != id {
			t.Fatalf("schema %d has ID %d", id, s.ID)
		}
		if s.MkfsPath == "" || s.ErofsUtils == "" || s.MkfsArgs == nil {
			t.Fatalf("schema %d is missing toolchain fields", id)
		}
		if prev, dup := seenPaths[s.MkfsPath]; dup {
			t.Fatalf("schemas %d and %d share mkfs path %s", prev, id, s.MkfsPath)
		}
		seenPaths[s.MkfsPath] = id
	}
}

// TestSchema1DerivationFrozen pins schema 1's exact mkfs.erofs invocation:
// it must stay byte-identical to the pre-schema packer forever. Any change
// here breaks reproducibility of every production artifact hash.
func TestSchema1DerivationFrozen(t *testing.T) {
	s, err := SchemaByID(1)
	if err != nil {
		t.Fatal(err)
	}
	if s.ErofsUtils != "1.5-1" {
		t.Fatalf("schema 1 erofs-utils = %s, want 1.5-1", s.ErofsUtils)
	}
	if s.MkfsPath != "/opt/modelwrap/schemas/v1/mkfs.erofs" {
		t.Fatalf("schema 1 mkfs path = %s", s.MkfsPath)
	}
	got := s.MkfsArgs("org/model@rev", "/out/img.tmp", "/cache/dir")
	want := []string{
		"--all-root",
		"-T0",
		"-U" + UUIDv5URL("org/model@rev-inner"),
		"/out/img.tmp",
		"/cache/dir",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema 1 mkfs args changed:\n got %q\nwant %q", got, want)
	}
}

func TestSchema2Derivation(t *testing.T) {
	s, err := SchemaByID(2)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Stable {
		t.Fatal("schema 2 is a production schema and must be stable")
	}
	if s.ErofsUtils != "1.8.6-1" {
		t.Fatalf("schema 2 erofs-utils = %s, want 1.8.6-1", s.ErofsUtils)
	}
	got := s.MkfsArgs("org/model@rev", "/out/img.tmp", "/cache/dir")
	if got[len(got)-2] != "/out/img.tmp" || got[len(got)-1] != "/cache/dir" {
		t.Fatalf("schema 2 args must end with image and source dir: %q", got)
	}
	for _, arg := range []string{"--all-root", "-T0", "-U" + UUIDv5URL("org/model@rev-inner"), "-b4096"} {
		if !slices.Contains(got, arg) {
			t.Fatalf("schema 2 args missing %q: %q", arg, got)
		}
	}
	if !slices.ContainsFunc(got, func(a string) bool { return strings.HasPrefix(a, "--workers=") }) {
		t.Fatalf("schema 2 args missing --workers: %q", got)
	}
}
