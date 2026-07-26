package denju

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// preflightCheck verifies, without touching the running binary, that an update
// could be applied at all: the binary is a regular file and its directory is
// writable.
//
// The write check creates and removes a real temp file rather than inspecting
// permission bits, because the bits lie - a read-only mount, a full filesystem,
// and a container's immutable layer all pass a mode check and fail the actual
// write.
func preflightCheck(n names, binaryPath string) error {
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("cannot access the binary at %s: %w", binaryPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the binary path %s is not a regular file", binaryPath)
	}

	dir := filepath.Dir(binaryPath)
	probe, err := os.CreateTemp(dir, n.tmpPermCheck)
	if err != nil {
		return fmt.Errorf("no write permission in the program directory %s", dir)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("could not clean up a temporary file in %s: %w", dir, err)
	}
	return nil
}

// swapBinary installs the staged binary over binaryPath, with binaryPath never
// absent at any instant: it hardlinks binaryPath -> <binary>.old (the rollback
// artifact), chmods the staged file to mode, then replaces binaryPath with a
// SINGLE atomic rename.
//
// The hardlink is the crux. Renaming the old binary aside and then moving the
// new one in leaves a window where nothing exists at binaryPath - and a host
// that loses power in that window has no program and no way to repair itself,
// because repair runs from the very binary that is missing. A hardlink gives the
// old inode a second name without ever unlinking the first.
//
// Any failure before the rename removes the link and leaves the program
// untouched.
func swapBinary(binaryPath, stagedPath string, mode os.FileMode, p paths) error {
	_ = os.Remove(p.RollbackBinary) // clear a stale leftover so the link can be created
	if err := os.Link(binaryPath, p.RollbackBinary); err != nil {
		return fmt.Errorf("link the rollback copy of the old binary: %w", err)
	}
	if err := os.Chmod(stagedPath, mode); err != nil {
		_ = os.Remove(p.RollbackBinary)
		return fmt.Errorf("set permissions on the new binary: %w", err)
	}
	if err := os.Rename(stagedPath, binaryPath); err != nil {
		_ = os.Remove(p.RollbackBinary)
		return fmt.Errorf("swap in the new binary: %w", err)
	}
	return nil
}

// restoreOldBinary undoes a swap by putting <binary>.old back at binaryPath. On
// Unix that is a single atomic rename-over. On Windows the file at binaryPath
// may be a RUNNING .exe, which cannot be replaced or deleted - but can be
// renamed - so it is first moved aside to the discard path; a later start
// reclaims it.
func restoreOldBinary(binaryPath string, p paths) error {
	if goos == "windows" {
		_ = os.Remove(p.Discard) // best-effort: clear a stale discard so the rename can land
		if err := os.Rename(binaryPath, p.Discard); err != nil {
			return fmt.Errorf("move the new binary aside for rollback: %w", err)
		}
	}
	if err := os.Rename(p.RollbackBinary, binaryPath); err != nil {
		return fmt.Errorf("restore the old binary: %w", err)
	}
	return nil
}

// copyBinary copies the binary at src to dst, executable (perm bits | 0700).
func copyBinary(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("inspect the binary to copy: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open the binary to copy: %w", err)
	}
	defer func() { _ = in.Close() }()

	mode := info.Mode().Perm() | 0o700
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create the update helper copy: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("write the update helper copy: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("finalize the update helper copy: %w", err)
	}
	// OpenFile's mode is subject to umask; force the exec bits explicitly.
	if err := os.Chmod(dst, mode); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("set permissions on the update helper copy: %w", err)
	}
	return nil
}

// sha256File streams a file through SHA-256 and returns the hex digest. Used to
// identify a binary by content when version strings cannot tell two builds
// apart.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
