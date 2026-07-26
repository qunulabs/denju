package denju

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spawnCall records one detached-spawn attempt made through the seam.
type spawnCall struct {
	path string
	args []string
	cwd  string
	env  []string
}

// helperSeams replaces the platform seams for one test and reports what the
// helper did. This is how the Windows flow - the one hardest to reach in CI and
// the most important to get right - is exercised on any host.
type helperSeams struct {
	spawns   []spawnCall
	services []string
	spawnErr error
	startErr error
}

func withHelperSeams(t *testing.T, s *helperSeams) *helperSeams {
	t.Helper()
	prevSpawn, prevStart := spawnDetached, startService
	spawnDetached = func(path string, args []string, cwd string, env []string) (int, error) {
		s.spawns = append(s.spawns, spawnCall{path: path, args: args, cwd: cwd, env: env})
		if s.spawnErr != nil {
			return 0, s.spawnErr
		}
		return 5555, nil
	}
	startService = func(name string) error {
		s.services = append(s.services, name)
		return s.startErr
	}
	t.Cleanup(func() { spawnDetached, startService = prevSpawn, prevStart })
	return s
}

// withFastPolling shrinks the helper's wait loop so tests do not sit for
// minutes.
func withFastPolling(t *testing.T) {
	t.Helper()
	prevInterval, prevBound := oldPIDPollInterval, oldPIDPollBound
	oldPIDPollInterval = time.Millisecond
	oldPIDPollBound = 100 * time.Millisecond
	t.Cleanup(func() { oldPIDPollInterval, oldPIDPollBound = prevInterval, prevBound })
}

// helperUpdater builds an Updater over a binary in a fresh directory. goos must
// already be forced, because the helper copy's name depends on it.
func helperUpdater(t *testing.T, binName, content string) (*Updater, string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, binName)
	writeBinary(t, bin, content)
	u, err := New(Config{Namespace: "app", Version: "1.3.0", BinaryPath: bin})
	noErr(t, err, "New")
	return u, dir, bin
}

// withHelperEnv sets the helper-role environment for one test.
func withHelperEnv(t *testing.T, u *Updater, mode, statePath string) {
	t.Helper()
	t.Setenv(u.n.envMode, mode)
	t.Setenv(u.n.envState, statePath)
}

func TestIsHelper(t *testing.T) {
	u := testUpdater(t)
	isFalse(t, u.isHelper(), "isHelper with no environment set")
	t.Setenv(u.n.envMode, modeApply)
	isTrue(t, u.isHelper(), "isHelper with the mode variable set")
}

func TestIsSelftest(t *testing.T) {
	u := testUpdater(t)
	isFalse(t, u.isSelftest(), "isSelftest with no environment set")
	t.Setenv(u.n.envSelftest, "1")
	isTrue(t, u.isSelftest(), "isSelftest with the variable set")
}

// RunProcessRole must report false for an ordinary start, or every program using
// denju would exit instead of running.
func TestRunProcessRole_OrdinaryStartIsNotARole(t *testing.T) {
	u := testUpdater(t)
	code, isRole := u.RunProcessRole()
	isFalse(t, isRole, "an ordinary start is not a role")
	eq(t, code, 0, "exit code")
}

// The child must start as a NORMAL program, so the control variables have to be
// stripped - otherwise it would re-enter a role and never serve.
func TestChildEnv_StripsTheControlVariables(t *testing.T) {
	n := newNames("app")
	got := childEnv(n, []string{
		"PATH=/usr/bin",
		n.envMode + "=apply",
		n.envState + "=/tmp/journal.json",
		n.envSelftest + "=1",
		"APP_BACKEND_URL=https://example.test",
	})
	eq(t, len(got), 2, "surviving variables")
	eq(t, got[0], "PATH=/usr/bin", "got[0]")
	eq(t, got[1], "APP_BACKEND_URL=https://example.test", "got[1]")
}

func TestRunHelper_NoStatePathFails(t *testing.T) {
	u := testUpdater(t)
	t.Setenv(u.n.envMode, modeApply)
	t.Setenv(u.n.envState, "")
	eq(t, u.runHelper(), 1, "exit code")
}

func TestRunHelper_UnreadableJournalFails(t *testing.T) {
	u := testUpdater(t)
	withHelperEnv(t, u, modeApply, filepath.Join(t.TempDir(), "missing.json"))
	eq(t, u.runHelper(), 1, "exit code")
}

func TestRunHelper_UnknownModeFails(t *testing.T) {
	u := testUpdater(t)
	dir := t.TempDir()
	statePath := filepath.Join(dir, "journal.json")
	noErr(t, writeState(u.n, statePath, &state{BinaryPath: filepath.Join(dir, "prog")}), "write the journal")
	withHelperEnv(t, u, "nonsense", statePath)

	eq(t, u.runHelper(), 1, "exit code")
}

// The full Windows apply role: handshake, wait for the old process, swap, and
// relaunch. The service path is what production uses, so the successor IS the
// service rather than an orphaned console process.
func TestRunHelper_ApplySwapsAndRestartsTheService(t *testing.T) {
	withGOOS(t, "windows")
	withFastPolling(t)
	withPIDAlive(t, noProcessAlive)
	seams := withHelperSeams(t, &helperSeams{})

	u, dir, bin := helperUpdater(t, "prog.exe", "old-binary")
	staged := filepath.Join(dir, ".app-update-staged")
	writeBinary(t, staged, "new-binary")
	p := u.paths
	noErr(t, writeState(u.n, p.State, &state{
		CommandID: "cmd-1", BinaryPath: bin, NewBinaryPath: staged,
		OldVersion: "1.3.0", TargetVersion: "1.4.0",
		PID: 4242, ServiceName: "prog", Phase: phaseStaged,
	}), "write the journal")
	withHelperEnv(t, u, modeApply, p.State)

	eq(t, u.runHelper(), 0, "exit code")

	eq(t, readFileString(t, bin), "new-binary", "the installed binary")
	eq(t, readFileString(t, p.RollbackBinary), "old-binary", "the rollback copy must survive the swap")

	eq(t, len(seams.services), 1, "service starts")
	eq(t, seams.services[0], "prog", "service started")
	eq(t, len(seams.spawns), 0, "a service install must restart through the SCM, not spawn a stray process")

	// The new image, not the helper, owns attestation - so the journal must be
	// left at attesting for it to resolve.
	j, err := readState(p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseAttesting, "phase")
	isTrue(t, j.HelperPID != 0, "the handshake PID must be recorded")
}

// A console-run program has no service to start, so the helper spawns it
// detached with the control variables stripped.
func TestRunHelper_ApplyConsoleSpawnsDetached(t *testing.T) {
	withGOOS(t, "windows")
	withFastPolling(t)
	withPIDAlive(t, noProcessAlive)
	seams := withHelperSeams(t, &helperSeams{})

	u, dir, bin := helperUpdater(t, "prog.exe", "old-binary")
	staged := filepath.Join(dir, ".app-update-staged")
	writeBinary(t, staged, "new-binary")
	noErr(t, writeState(u.n, u.paths.State, &state{
		BinaryPath: bin, NewBinaryPath: staged, PID: 4242,
		Args: []string{"--verbose"}, Cwd: dir, Phase: phaseStaged,
	}), "write the journal")
	withHelperEnv(t, u, modeApply, u.paths.State)

	eq(t, u.runHelper(), 0, "exit code")

	eq(t, len(seams.spawns), 1, "spawns")
	eq(t, seams.spawns[0].path, bin, "spawned path")
	eq(t, len(seams.spawns[0].args), 1, "spawned args")
	eq(t, seams.spawns[0].args[0], "--verbose", "spawned args[0]")
	eq(t, seams.spawns[0].cwd, dir, "spawned cwd")
	for _, kv := range seams.spawns[0].env {
		if strings.HasPrefix(kv, u.n.envMode+"=") {
			t.Fatal("the child must not re-enter helper mode")
		}
	}
}

// If the old process never exits the helper must NOT swap: the running binary
// would be replaced under a live program, and on Windows the rename would fail
// anyway. It aborts and reclaims the staged download.
func TestRunHelper_ApplyAbortsWhenTheOldProcessLingers(t *testing.T) {
	withGOOS(t, "windows")
	withFastPolling(t)
	withPIDAlive(t, func(int) bool { return true })
	withHelperSeams(t, &helperSeams{})

	u, dir, bin := helperUpdater(t, "prog.exe", "old-binary")
	staged := filepath.Join(dir, ".app-update-staged")
	writeBinary(t, staged, "new-binary")
	noErr(t, writeState(u.n, u.paths.State, &state{
		BinaryPath: bin, NewBinaryPath: staged, PID: 4242, Phase: phaseStaged,
	}), "write the journal")
	withHelperEnv(t, u, modeApply, u.paths.State)

	eq(t, u.runHelper(), 1, "exit code")

	eq(t, readFileString(t, bin), "old-binary", "the running binary must not be replaced")
	isFalse(t, exists(staged), "the staged download must not be leaked")
	isFalse(t, exists(u.paths.State), "an aborted handoff clears its journal")
}

// A new binary that cannot be launched is worse than no update: the helper puts
// the old one back, journals the rollback so it gets reported, and starts it.
func TestRunHelper_ApplyRollsBackWhenTheNewVersionWillNotLaunch(t *testing.T) {
	withGOOS(t, "windows")
	withFastPolling(t)
	withPIDAlive(t, noProcessAlive)
	seams := withHelperSeams(t, &helperSeams{startErr: errStartService})

	u, dir, bin := helperUpdater(t, "prog.exe", "old-binary")
	staged := filepath.Join(dir, ".app-update-staged")
	writeBinary(t, staged, "new-binary")
	noErr(t, writeState(u.n, u.paths.State, &state{
		BinaryPath: bin, NewBinaryPath: staged, PID: 4242,
		ServiceName: "prog", Phase: phaseStaged,
	}), "write the journal")
	withHelperEnv(t, u, modeApply, u.paths.State)

	// Both the new-version start and the rollback start fail here, so the helper
	// reports failure - but the binary on disk must be the OLD one either way.
	eq(t, u.runHelper(), 1, "exit code")

	eq(t, readFileString(t, bin), "old-binary", "the binary on disk")
	eq(t, len(seams.services), 2, "one start for the new version, one for the restored old one")

	j, err := readState(u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	hasSubstr(t, j.ErrorMessage, "could not launch the new version", "journal error message")
}

// The relaunch role is the Windows stand-in for exec-in-place: wait for the old
// process, start the binary again, and clean up the restart journal.
func TestRunHelper_RelaunchRestartsAndClearsItsJournal(t *testing.T) {
	withGOOS(t, "windows")
	withFastPolling(t)
	withPIDAlive(t, noProcessAlive)
	seams := withHelperSeams(t, &helperSeams{})

	u, dir, bin := helperUpdater(t, "prog.exe", "program")
	noErr(t, writeState(u.n, u.paths.Restart, &state{
		BinaryPath: bin, PID: 4242, Cwd: dir, ServiceName: "prog",
	}), "write the restart journal")
	withHelperEnv(t, u, modeRelaunch, u.paths.Restart)

	eq(t, u.runHelper(), 0, "exit code")

	eq(t, len(seams.services), 1, "service starts")
	eq(t, seams.services[0], "prog", "service started")
	isFalse(t, exists(u.paths.Restart), "the restart journal carries nothing worth keeping")
}

// A relaunch whose target never exits must not start a second instance
// alongside the first.
func TestRunHelper_RelaunchAbortsWhenTheProcessLingers(t *testing.T) {
	withFastPolling(t)
	withPIDAlive(t, func(int) bool { return true })
	seams := withHelperSeams(t, &helperSeams{})

	u, dir, bin := helperUpdater(t, "prog", "program")
	noErr(t, writeState(u.n, u.paths.Restart, &state{BinaryPath: bin, PID: 4242, Cwd: dir}),
		"write the restart journal")
	withHelperEnv(t, u, modeRelaunch, u.paths.Restart)

	eq(t, u.runHelper(), 1, "exit code")
	eq(t, len(seams.spawns), 0, "spawns")
	eq(t, len(seams.services), 0, "service starts")
}

// The update journal must NOT be deleted by a relaunch: a rollback relaunch
// reads it, and the outcome still has to be reported.
func TestRunHelper_RelaunchKeepsAnUpdateJournal(t *testing.T) {
	withFastPolling(t)
	withPIDAlive(t, noProcessAlive)
	withHelperSeams(t, &helperSeams{})

	u, dir, bin := helperUpdater(t, "prog", "program")
	noErr(t, writeState(u.n, u.paths.State, &state{
		BinaryPath: bin, PID: 4242, Cwd: dir, Phase: phaseRolledBack, CommandID: "cmd-1",
	}), "write the journal")
	withHelperEnv(t, u, modeRelaunch, u.paths.State)

	eq(t, u.runHelper(), 0, "exit code")
	isTrue(t, exists(u.paths.State), "the rolled-back outcome must survive to be reported")
}

func TestWaitForExit(t *testing.T) {
	withFastPolling(t)

	withPIDAlive(t, noProcessAlive)
	isTrue(t, waitForExit(4242), "a dead pid is waited out immediately")

	withPIDAlive(t, func(int) bool { return true })
	isFalse(t, waitForExit(4242), "a live pid times out")
}

// errStartService stands in for a service-control-manager start failure.
var errStartService = errors.New("the service could not be started")
