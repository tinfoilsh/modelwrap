// Command modelwrap packs model weights into MWP/EMWP artifacts.
//
// The same binary runs in two contexts. Inside the packer container
// (marked by MODELWRAP_IN_CONTAINER=1) it runs the packer directly. On a
// host it acts as a launcher: it re-executes itself inside the
// digest-pinned packer image via docker, translating host paths into
// container mounts, so artifact bytes are always produced by the pinned
// toolchain.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tinfoilsh/modelwrap/wrap"
)

// defaultImage is the packer image used by the launcher. Release builds
// override it with a digest-pinned reference via
// -ldflags "-X main.defaultImage=ghcr.io/tinfoilsh/modelwrap@sha256:...".
var defaultImage = "ghcr.io/tinfoilsh/modelwrap:latest"

const usage = `Usage: modelwrap [flags] [model[@revision]]

Packs a Hugging Face model or local directory into a reproducible
dm-verity EROFS image (MWP), optionally encrypted (EMWP).

Run on a host, modelwrap re-executes itself inside the pinned packer
container image (requires docker). Inside the container it packs directly.

Flags:
  --model-dir <path>  pack a local model directory instead of downloading
  --encrypt           emit encrypted EMWP output (requires a master key)
  --key-file <path>   file containing the base64-encoded 64-byte EMWP master key
  --verify            verify artifacts after packing
  --output <path>     output directory (default ./output)
  --cache <path>      download cache directory (default ./cache)
  --image <ref>       packer container image to launch (default release-pinned)
  --local             run the packer directly instead of in a container
  -h, --help          show this help

Environment fallbacks: MODEL, MODEL_DIR, VERIFY=1, ENCRYPTION=1, HF_TOKEN,
PRIVATE_MODEL_KEY_FILE, PRIVATE_MODEL_KEY_B64, CACHE_DIR, OUTPUT_DIR,
MODELWRAP_IMAGE.`

// cliOptions extends the packer options with launcher-only settings.
type cliOptions struct {
	wrap.Options
	image string
	local bool
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}

	if opts.local || os.Getenv("MODELWRAP_IN_CONTAINER") == "1" {
		ref, err := wrap.Pack(opts.Options)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Println(ref)
		return
	}

	os.Exit(launch(opts))
}

// parseArgs parses flags (before the optional positional model argument)
// with environment variable fallbacks for container-style invocation.
func parseArgs(args []string) (cliOptions, error) {
	opts := cliOptions{
		Options: wrap.Options{
			Model:     os.Getenv("MODEL"),
			ModelDir:  os.Getenv("MODEL_DIR"),
			CacheDir:  os.Getenv("CACHE_DIR"),
			OutputDir: os.Getenv("OUTPUT_DIR"),
			Verify:    os.Getenv("VERIFY") == "1",
			Encrypt:   os.Getenv("ENCRYPTION") == "1",
			HFToken:   os.Getenv("HF_TOKEN"),
		},
		image: defaultImage,
	}
	if image := os.Getenv("MODELWRAP_IMAGE"); image != "" {
		opts.image = image
	}

	fs := flag.NewFlagSet("modelwrap", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	fs.StringVar(&opts.ModelDir, "model-dir", opts.ModelDir, "")
	fs.StringVar(&opts.KeyFile, "key-file", "", "")
	fs.StringVar(&opts.OutputDir, "output", opts.OutputDir, "")
	fs.StringVar(&opts.CacheDir, "cache", opts.CacheDir, "")
	fs.StringVar(&opts.image, "image", opts.image, "")
	fs.BoolVar(&opts.Verify, "verify", opts.Verify, "")
	fs.BoolVar(&opts.Encrypt, "encrypt", opts.Encrypt, "")
	fs.BoolVar(&opts.local, "local", false, "")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	rest := fs.Args()
	if len(rest) > 1 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v (flags must come before the model argument)\n", rest[1:])
		fs.Usage()
		return opts, fmt.Errorf("unexpected arguments")
	}
	if len(rest) == 1 {
		opts.Model = rest[0]
	}
	return opts, nil
}
