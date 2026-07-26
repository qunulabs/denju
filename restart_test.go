package denju

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func envHasKV(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// restartUpdater builds an Updater over a binary with the given content. goos
// must already be forced.
func restartUpdater(t *testing.T, binName, content string) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, binName)
	writeBinary(t, bin, content)
	u, err := New(Config{Namespace: "app", Version: "1.4.0", BinaryPath: bin})
	noErr(t, err, "New")
	return u, bin
}

func TestSwapBinary_HardlinkAndAtomicReplacePreservesPerms(t *testing.T) {
	withGOOS(t, "linux")
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	p := newNames("app").pathsFor(bin)
	noErr(t, os.WriteFile(bin, []byte("old"), 0o755), "write the binary")
	staged := filepath.Join(dir, ".staged")
	noErr(t, os.WriteFile(staged, []byte("new"), 0o600), "write the staged file")

	noErr(t, swapBinary(bin, staged, 0o755, p), "swapBinary")
	eq(t, readFileString(t, bin), "new", "the installed binary")
	eq(t, readFileString(t, p.RollbackBinary), "old", "the rollback hardlink")
	isFalse(t, exists(staged), "the staged file is consumed by the rename")

	// Perm bits are only faithful on Unix hosts; Windows collapses them to the
	// read-only bit, so the chmod result cannot be asserted there.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(bin)
		noErr(t, err, "stat the binary")
		eq(t, info.Mode().Perm(), os.FileMode(0o755), "perm bits carried over from the old binary")
	}
}

func TestRestart_UnixExecs(t *testing.T) {
	withGOOS(t, "linux")
	u, bin := restartUpdater(t, "prog", "program")

	var gotPath string
	var gotArgv []string
	prev := execSelf
	execSelf = func(path string, argv []string, _ []string) error {
		gotPath, gotArgv = path, argv
		return nil
	}
	t.Cleanup(func() { execSelf = prev })

	noErr(t, u.Restart(), "Restart")
	eq(t, gotPath, bin, "exec path")
	isTrue(t, len(gotArgv) > 0, "argv is replayed")
	eq(t, gotArgv[0], os.Args[0], "argv[0] is replayed verbatim")
}

// Windows has no exec, so a restart hands off to a detached helper that waits
// for this process to die. The helper copy is recreated if it is missing.
func TestRestart_WindowsSpawnsRelaunchHelperAndExits(t *testing.T) {
	withGOOS(t, "windows")
	u, _ := restartUpdater(t, "prog.exe", "program")
	// No helper copy on disk: Restart must recreate it from the binary.

	var spawnPath string
	var spawnEnv []string
	prevSpawn := spawnDetached
	spawnDetached = func(path string, _ []string, _ string, env []string) (int, error) {
		spawnPath, spawnEnv = path, env
		return 90001, nil
	}
	exitCode := -1
	prevExit := osExit
	osExit = func(code int) { exitCode = code }
	t.Cleanup(func() { spawnDetached = prevSpawn; osExit = prevExit })

	noErr(t, u.Restart(), "Restart")

	isTrue(t, exists(u.paths.HelperCopy), "Restart must recreate a missing helper copy")
	eq(t, spawnPath, u.paths.HelperCopy, "the helper copy is what gets spawned")
	isTrue(t, envHasKV(spawnEnv, u.n.envMode+"="+modeRelaunch), "relaunch mode must be set")
	isTrue(t, envHasKV(spawnEnv, u.n.envState+"="+u.paths.Restart), "the restart journal must be named")
	eq(t, exitCode, 0, "the process must exit so the helper's wait can complete")

	// The restart journal is separate from the update journal, so a restart can
	// never destroy an in-flight update's write-ahead record.
	isTrue(t, exists(u.paths.Restart), "the restart journal is written")
	isFalse(t, exists(u.paths.State), "a restart must not touch the update journal")
}

// A caller-requested restart must not clobber an update that is mid-flight.
func TestRestart_DoesNotDisturbAnInFlightUpdateJournal(t *testing.T) {
	withGOOS(t, "windows")
	u, bin := restartUpdater(t, "prog.exe", "program")
	stageJournal(t, u, &state{
		Phase: phaseAttesting, CommandID: "cmd-5", BinaryPath: bin,
		OldVersion: "1.3.0", TargetVersion: "1.4.0",
	})

	prevSpawn := spawnDetached
	spawnDetached = func(string, []string, string, []string) (int, error) { return 90001, nil }
	prevExit := osExit
	osExit = func(int) {}
	t.Cleanup(func() { spawnDetached = prevSpawn; osExit = prevExit })

	noErr(t, u.Restart(), "Restart")

	j, err := readState(u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseAttesting, "the update journal is untouched")
	eq(t, j.CommandID, "cmd-5", "the update journal is untouched")
}

// The restore must happen BEFORE the journal write. Journaling first would, if
// the restore then failed, leave a journal claiming a rollback that never
// happened - and cleanup would delete the only copy of the old version.
func TestRollbackAndRestart_Unix(t *testing.T) {
	withGOOS(t, "linux")
	u, bin := restartUpdater(t, "prog", "new")
	writeBinary(t, u.paths.RollbackBinary, "old")

	execCalled := false
	prev := execSelf
	execSelf = func(path string, _ []string, _ []string) error {
		execCalled = true
		eq(t, readFileString(t, path), "old", "the old binary must be restored before the exec")
		return nil
	}
	t.Cleanup(func() { execSelf = prev })

	noErr(t, u.rollbackAndRestart(&state{CommandID: "c", BinaryPath: bin}, "it broke"), "rollbackAndRestart")
	isTrue(t, execCalled, "the restored old binary must be exec'd")

	j, err := readState(u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	eq(t, j.ErrorMessage, "it broke", "the cause is recorded for reporting")
	eq(t, j.PID, os.Getpid(), "the journal pid is refreshed to whoever rolled back")
}

// A rollback with no rollback copy must fail loudly rather than leave the new
// binary in place under a journal that claims it was undone.
func TestRollbackAndRestart_NoRollbackCopyFails(t *testing.T) {
	withGOOS(t, "linux")
	u, bin := restartUpdater(t, "prog", "new")

	err := u.rollbackAndRestart(&state{BinaryPath: bin}, "it broke")
	wantErrContaining(t, err, "restore the old binary", "rollbackAndRestart with no rollback copy")
	eq(t, readFileString(t, bin), "new", "nothing was changed")
}

// On Windows the rollback relaunch reads the SAME update journal, so the
// rolledback record survives for the successor to report.
func TestRollbackAndRestart_WindowsKeepsTheUpdateJournal(t *testing.T) {
	withGOOS(t, "windows")
	u, bin := restartUpdater(t, "prog.exe", "new")
	writeBinary(t, u.paths.RollbackBinary, "old")

	var spawnEnv []string
	prevSpawn := spawnDetached
	spawnDetached = func(_ string, _ []string, _ string, env []string) (int, error) {
		spawnEnv = env
		return 90002, nil
	}
	prevExit := osExit
	osExit = func(int) {}
	t.Cleanup(func() { spawnDetached = prevSpawn; osExit = prevExit })

	noErr(t, u.rollbackAndRestart(&state{CommandID: "c", BinaryPath: bin}, "it broke"), "rollbackAndRestart")

	eq(t, readFileString(t, bin), "old", "the old binary is back in place")
	eq(t, readFileString(t, u.paths.Discard), "new", "the running new binary is kept aside, not lost")
	isTrue(t, envHasKV(spawnEnv, u.n.envState+"="+u.paths.State),
		"the relaunch helper must read the update journal, not the restart journal")

	j, err := readState(u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	eq(t, j.ErrorMessage, "it broke", "the cause survives for reporting")
}
