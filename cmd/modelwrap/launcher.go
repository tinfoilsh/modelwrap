package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Fixed container-side paths the launcher rewrites host paths to. /cache
// and /output match the image's CACHE_DIR and OUTPUT_DIR environment.
const (
	containerModelDir = "/model"
	containerKeyFile  = "/run/modelwrap-key"
	containerCacheDir = "/cache"
	containerOutput   = "/output"
)

// Secrets passed through to the container by name only, so values never
// appear in the docker command line.
var passthroughEnv = []string{"HF_TOKEN", "PRIVATE_MODEL_KEY_B64"}

// launch re-executes the CLI inside the packer container image and
// returns the process exit code.
func launch(opts cliOptions) int {
	args, err := dockerRunArgs(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}

	cmd := exec.Command("docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "Error: running docker:", err)
		return 1
	}
	return 0
}

// dockerRunArgs translates host-side options into a docker run invocation
// of the same CLI inside the packer image.
func dockerRunArgs(opts cliOptions) ([]string, error) {
	args := []string{"run", "--rm"}

	hostDir := func(path, fallback string) (string, error) {
		if path == "" {
			path = fallback
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		// Pre-create so docker does not create it root-owned.
		if err := os.MkdirAll(abs, 0755); err != nil {
			return "", err
		}
		return abs, nil
	}

	outputDir, err := hostDir(opts.OutputDir, "output")
	if err != nil {
		return nil, err
	}
	cacheDir, err := hostDir(opts.CacheDir, "cache")
	if err != nil {
		return nil, err
	}
	args = append(args,
		"-v", outputDir+":"+containerOutput,
		"-v", cacheDir+":"+containerCacheDir,
	)

	if opts.ModelDir != "" {
		abs, err := filepath.Abs(opts.ModelDir)
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", abs+":"+containerModelDir+":ro")
	}

	keyFile := opts.KeyFile
	if keyFile == "" {
		keyFile = os.Getenv("PRIVATE_MODEL_KEY_FILE")
	}
	if keyFile != "" {
		abs, err := filepath.Abs(keyFile)
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", abs+":"+containerKeyFile+":ro")
	}

	for _, name := range passthroughEnv {
		if os.Getenv(name) != "" {
			args = append(args, "-e", name)
		}
	}

	args = append(args, opts.image)

	if opts.delete {
		args = append(args, "--delete")
	}
	if opts.ModelDir != "" {
		args = append(args, "--model-dir", containerModelDir)
	}
	if keyFile != "" {
		args = append(args, "--key-file", containerKeyFile)
	}
	if opts.Encrypt {
		args = append(args, "--encrypt")
	}
	if opts.Verify {
		args = append(args, "--verify")
	}
	if opts.Schema != 0 {
		args = append(args, "--schema", strconv.Itoa(opts.Schema))
	}
	if opts.Model != "" {
		args = append(args, opts.Model)
	}
	return args, nil
}
