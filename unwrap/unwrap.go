// Package unwrap implements the consumer side of the Modelwrap protocol:
// opening dm-crypt and dm-verity mappings over MWP/EMWP artifacts and
// mounting the verified filesystem read-only.
//
// It shells out to cryptsetup, veritysetup, and mount, and is intended to
// run in environments that ship those tools (e.g. the cvmimage initramfs).
package unwrap

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/tinfoilsh/modelwrap"
)

// Filesystem and mount hardening parameters for model pack mounts.
const (
	FilesystemType = "erofs"
	MountOptions   = "ro,nodev,nosuid,noexec"
)

// OpenCrypt opens a read-only dm-crypt plain mapping over an EMWP payload
// device using the format's cipher parameters. The key file must contain
// the raw per-artifact key derived with modelwrap.DeriveKey.
func OpenCrypt(device, name, keyFile string) error {
	args := append([]string{"open"}, modelwrap.CryptsetupArgs()...)
	args = append(args,
		"--key-file", keyFile,
		"--readonly",
		device,
		name,
	)
	cmd := exec.Command("cryptsetup", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cryptsetup open: %w", err)
	}
	return nil
}

// CloseCrypt tears down a dm-crypt mapping, ignoring errors.
func CloseCrypt(name string) {
	_ = exec.Command("cryptsetup", "close", name).Run()
}

// OpenVerity opens a dm-verity mapping over a device that contains a
// filesystem followed by its hash tree at hashOffset.
//
// The mapping is always opened with --no-superblock and fully explicit,
// pinned format parameters, so veritysetup and the kernel never parse
// any on-disk metadata. The data size is derived from the attested
// hashOffset, and the salt (32 bytes) is re-derived from the attested
// model identity via modelwrap.VeritySalt. A wrong salt fails closed:
// nothing can verify against the attested root hash.
func OpenVerity(device, name, rootHash, hashOffset string, salt []byte) error {
	offset, err := strconv.ParseUint(hashOffset, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid hash offset %q: %w", hashOffset, err)
	}
	params, err := modelwrap.VerityParamsForArtifact(offset, salt)
	if err != nil {
		return fmt.Errorf("deriving verity params: %w", err)
	}

	args := append([]string{
		"open",
		device,
		name,
		device,
		rootHash,
	}, params.VeritysetupArgs()...)
	cmd := exec.Command("veritysetup", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("veritysetup open: %w", err)
	}
	return nil
}

// CloseVerity tears down a dm-verity mapping, ignoring errors.
func CloseVerity(name string) {
	_ = exec.Command("veritysetup", "close", name).Run()
}

// Mount mounts a verified model pack device read-only with hardened
// options at mountPoint, creating the mount point if needed.
func Mount(device, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("creating mount point: %w", err)
	}
	cmd := exec.Command(
		"mount",
		"-t", FilesystemType,
		"-o", MountOptions,
		device,
		mountPoint,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mounting verity device: %w", err)
	}
	return nil
}
