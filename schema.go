package modelwrap

import (
	"fmt"
	"runtime"
	"slices"
	"strconv"
)

// A PackSchema is a frozen, numbered specification of the derivation from
// model directory bytes to EROFS image bytes: file layout and ordering,
// the exact pinned mkfs.erofs build, and its flags. Pack identity is
// (repo@revision, schema) -> root hash.
//
// The dm-verity parameters (verity.go) are deliberately not part of a
// schema: they are frozen across all schemas, so a schema changes how
// bytes are arranged, never how they are proven. Consumers and the
// rootHash_hashOffset_uuid reference format are schema-independent.
type PackSchema struct {
	ID int
	// Stable schemas are permanent reproducibility contracts: their
	// output for a given input must never change, and they stay
	// buildable indefinitely as the reproduction path for every
	// artifact packed under them.
	Stable bool
	// ErofsUtils is the pinned Debian erofs-utils version vendored in
	// the packer image for this schema.
	ErofsUtils string
	// MkfsPath is the schema's vendored mkfs.erofs inside the packer image.
	MkfsPath string
	// MkfsArgs builds the mkfs.erofs argument list deriving imgFile from
	// modelDir. Every argument that can influence output bytes is fixed
	// here; a host-dependent argument (worker count) must be byte-neutral.
	MkfsArgs func(model, imgFile, modelDir string) []string
	// Doc is a one-line derivation summary for job logs.
	Doc string
}

// DefaultSchema is the schema used when a wrap request does not name one.
// It deliberately lags newly added schemas: it flips only as an explicit
// release decision once a newer schema is proven in production.
const DefaultSchema = 1

var packSchemas = map[int]PackSchema{
	1: {
		ID:         1,
		Stable:     true,
		ErofsUtils: "1.5-1",
		MkfsPath:   "/opt/modelwrap/schemas/v1/mkfs.erofs",
		MkfsArgs: func(model, imgFile, modelDir string) []string {
			return []string{
				"--all-root",
				"-T0",                            // Zero timestamps
				"-U" + UUIDv5URL(model+"-inner"), // Static filesystem UUID
				imgFile,
				modelDir,
			}
		},
		Doc: "alphabetical layout, single-threaded mkfs",
	},
	2: {
		ID:         2,
		Stable:     true,
		ErofsUtils: "1.8.6-1",
		MkfsPath:   "/opt/modelwrap/schemas/v2/mkfs.erofs",
		MkfsArgs: func(model, imgFile, modelDir string) []string {
			return []string{
				"--all-root",
				"-T0",
				"-U" + UUIDv5URL(model+"-inner"),
				// 1.8 defaults the block size to the host page size;
				// pin it to the frozen verity block size.
				"-b4096",
				// Worker count only sets parallelism: data placement is
				// ordered by the single writer thread, so output bytes
				// never depend on it.
				"--workers=" + strconv.Itoa(runtime.NumCPU()),
				imgFile,
				modelDir,
			}
		},
		Doc: "schema 1 layout, multithreaded mkfs",
	},
}

// SchemaByID returns the registered pack schema for id. Unknown ids are an
// error: a schema is never guessed or silently defaulted.
func SchemaByID(id int) (PackSchema, error) {
	s, ok := packSchemas[id]
	if !ok {
		return PackSchema{}, fmt.Errorf("unknown pack schema %d (supported: %v)", id, SchemaIDs())
	}
	return s, nil
}

// SchemaIDs returns all registered schema ids in ascending order.
func SchemaIDs() []int {
	ids := make([]int, 0, len(packSchemas))
	for id := range packSchemas {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
