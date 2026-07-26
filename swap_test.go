package denju

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPreflightCheck_OK(t *testing.T) {
	n := newNames("app")
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "old")

	noErr(t, preflightCheck(n, bin), "preflightCheck")

	// The probe file must be cleaned up.
	entries, err := os.ReadDir(dir)
	noErr(t, err, "read the directory")
	eq(t, len(entries), 1, "files left in the directory")
}

func TestPreflightCheck_MissingBinary(t *testing.T) {
	err := preflightCheck(newNames("app"), filepath.Join(t.TempDir(), "nope"))
	wantErrContaining(t, err, "cannot access", "preflightCheck on a missing binary")
}

// The write check creates and removes a real file rather than inspecting
// permission bits, because the bits lie: a read-only mount, a full filesystem
// and a container's immutable layer all pass a mode check and fail the write.
func TestPreflightCheck_UnwritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits do not govern writability on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission bits this test relies on")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "old")
	noErr(t, os.Chmod(dir, 0o500), "make the directory read-only")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := preflightCheck(newNames("app"), bin)
	wantErrContaining(t, err, "no write permission", "preflightCheck in a read-only directory")
}

func TestPreflightCheck_NotARegularFile(t *testing.T) {
	dir := t.TempDir()
	err := preflightCheck(newNames("app"), dir)
	wantErrContaining(t, err, "not a regular file", "preflightCheck on a directory")
}

// The swap must leave the binary present at every instant AND produce a
// rollback copy of the old content. This is the property the whole design is
// built around: a power cut must never leave a host with no binary.
func TestSwapBinary_InstallsNewAndKeepsRollback(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	staged := filepath.Join(dir, ".app-update-staged")
	writeBinary(t, bin, "old-binary")
	writeBinary(t, staged, "new-binary")
	p := newNames("app").pathsFor(bin)

	noErr(t, swapBinary(bin, staged, 0o755, p), "swapBinary")

	eq(t, readFileString(t, bin), "new-binary", "the installed binary")
	eq(t, readFileString(t, p.RollbackBinary), "old-binary", "the rollback copy")
	isFalse(t, exists(staged), "the staged file was renamed, not copied")
}

// A leftover .old from an earlier update must not block the hardlink.
func TestSwapBinary_ClearsStaleRollback(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	staged := filepath.Join(dir, ".app-update-staged")
	writeBinary(t, bin, "old-binary")
	writeBinary(t, staged, "new-binary")
	p := newNames("app").pathsFor(bin)
	writeBinary(t, p.RollbackBinary, "ancient-leftover")

	noErr(t, swapBinary(bin, staged, 0o755, p), "swapBinary")
	eq(t, readFileString(t, p.RollbackBinary), "old-binary", "the rollback copy")
}

// A failure before the rename must leave the program exactly as it was, with no
// half-made rollback link lying around to confuse repair.
func TestSwapBinary_MissingStagedLeavesProgramUntouched(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "old-binary")
	p := newNames("app").pathsFor(bin)

	err := swapBinary(bin, filepath.Join(dir, "does-not-exist"), 0o755, p)
	wantErr(t, err, "swapBinary with a missing staged file")

	eq(t, readFileString(t, bin), "old-binary", "the binary is untouched")
	isFalse(t, exists(p.RollbackBinary), "the rollback link must be removed on failure")
}

func TestRestoreOldBinary_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the unix restore path renames over a file, which Windows forbids for a running exe")
	}
	withGOOS(t, "linux")
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "new-binary")
	p := newNames("app").pathsFor(bin)
	writeBinary(t, p.RollbackBinary, "old-binary")

	noErr(t, restoreOldBinary(bin, p), "restoreOldBinary")

	eq(t, readFileString(t, bin), "old-binary", "the restored binary")
	isFalse(t, exists(p.RollbackBinary), "the rollback copy is consumed")
}

// On Windows the new binary may be a RUNNING exe, which cannot be replaced or
// deleted but can be renamed - so the restore moves it to the discard path
// first, and a later start reclaims that.
func TestRestoreOldBinary_WindowsMovesNewAside(t *testing.T) {
	withGOOS(t, "windows")
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog.exe")
	writeBinary(t, bin, "new-binary")
	p := newNames("app").pathsFor(bin)
	writeBinary(t, p.RollbackBinary, "old-binary")

	noErr(t, restoreOldBinary(bin, p), "restoreOldBinary")

	eq(t, readFileString(t, bin), "old-binary", "the restored binary")
	eq(t, readFileString(t, p.Discard), "new-binary", "the new binary must be kept aside, not lost")
}

func TestRestoreOldBinary_NoRollbackCopy(t *testing.T) {
	withGOOS(t, "linux")
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "new-binary")

	err := restoreOldBinary(bin, newNames("app").pathsFor(bin))
	wantErrContaining(t, err, "restore the old binary", "restoreOldBinary with no rollback copy")
}

func TestCopyBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "prog")
	dst := filepath.Join(dir, "prog.app-updater")
	writeBinary(t, src, "program-bytes")

	noErr(t, copyBinary(src, dst), "copyBinary")
	eq(t, readFileString(t, dst), "program-bytes", "the copied bytes")

	info, err := os.Stat(dst)
	noErr(t, err, "stat the copy")
	if runtime.GOOS != "windows" {
		isTrue(t, info.Mode().Perm()&0o100 != 0, "the helper copy must be executable")
	}
}

func TestCopyBinary_MissingSource(t *testing.T) {
	dir := t.TempDir()
	err := copyBinary(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	wantErr(t, err, "copyBinary with a missing source")
}

func TestSHA256File(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	writeBinary(t, a, "same")
	writeBinary(t, b, "same")

	sumA, err := sha256File(a)
	noErr(t, err, "hash a")
	sumB, err := sha256File(b)
	noErr(t, err, "hash b")
	eq(t, sumA, sumB, "identical content hashes identically")
	eq(t, len(sumA), 64, "hex digest length")

	_, err = sha256File(filepath.Join(dir, "missing"))
	wantErr(t, err, "hash a missing file")
}
