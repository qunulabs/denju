package denju

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Duration seams. Package vars purely so tests can shrink them.
var (
	// oldPIDPollInterval is how often the helper polls for the waited-on
	// process.
	oldPIDPollInterval = 200 * time.Millisecond
	// oldPIDPollBound bounds the wait for the old process to exit. It must
	// comfortably exceed everything the old process does after spawning the
	// helper - the handshake gate plus the full drain - because the helper's
	// clock starts before the drain does.
	oldPIDPollBound = 4 * time.Minute
	// handshakePoll is how often the spawning process checks whether the helper
	// has recorded its PID.
	handshakePoll = 200 * time.Millisecond
)

// Platform seams (real implementations in detach_windows.go, stubs in
// detach_other.go). Package vars so the whole Windows flow can be exercised on a
// Linux CI runner.
var (
	// spawnDetached starts path fully detached from this process (own process
	// group, no console, best-effort job-object breakaway) and returns its PID
	// without waiting.
	spawnDetached func(path string, args []string, cwd string, env []string) (int, error) = defaultSpawnDetached
	// startService asks the Windows service control manager to start the named
	// service.
	startService func(name string) error = defaultStartService
	// serviceIdentity reports the Windows service name this process runs as, or
	// "" for a console process. Always "" on Unix.
	serviceIdentity func() (string, error) = defaultServiceIdentity
)

// The helper is a copy of the program itself, not a separate shipped artifact -
// there is nothing extra to build, sign or install. Windows simply cannot
// overwrite a running .exe and has no exec(), so some other process has to do
// the swap once this one is gone, and the program is the only executable we know
// is present and trusted on the host. Unix never spawns a helper: exec-in-place
// needs none.

// helperLogFile receives the helper's log lines. The helper runs fully detached
// with no console, so stderr goes nowhere and a file beside the binary is the
// only place an operator can see what happened.
var (
	helperLogFile   *os.File
	helperLogPrefix string
)

// isHelper reports whether this process was spawned as an update helper.
func (u *Updater) isHelper() bool { return os.Getenv(u.n.envMode) != "" }

// isSelftest reports whether this process was spawned as a selftest child.
func (u *Updater) isSelftest() bool { return os.Getenv(u.n.envSelftest) == "1" }

// runHelper is the detached helper entry point. It returns the process exit
// code.
//
// The helper deliberately takes its bearings from the journal rather than from
// its own executable path: it IS a copy, living beside the real binary under a
// different name, so os.Executable would point at the copy.
func (u *Updater) runHelper() int {
	statePath := os.Getenv(u.n.envState)
	if statePath == "" {
		logf("no %s set in the environment; nothing to do", u.n.envState)
		return 1
	}
	j, err := readState(statePath)
	if err != nil {
		logf("cannot read the update journal at %s: %v", statePath, err)
		return 1
	}
	p := u.n.pathsFor(j.BinaryPath)
	openHelperLog(u.n, p)
	defer closeHelperLog()

	switch os.Getenv(u.n.envMode) {
	case modeApply:
		return u.runApply(j, p)
	case modeRelaunch:
		return u.runRelaunch(j, p, statePath)
	default:
		logf("unrecognized %s=%q; nothing to do", u.n.envMode, os.Getenv(u.n.envMode))
		return 1
	}
}

// runApply is the swap-and-relaunch role: handshake, wait for the old process to
// exit, swap the binary (write-ahead journaled), and restart into the new
// version. The NEW IMAGE owns attestation and the commit/rollback decision - the
// helper has no watchdog and exits as soon as it has launched something.
func (u *Updater) runApply(j *state, p paths) int {
	// Handshake: record our PID in the journal. The old process polls for it
	// before it drains and exits, which proves the helper actually reached this
	// code rather than dying in the program's own startup.
	j.HelperPID = os.Getpid()
	if err := writeState(u.n, p.State, j); err != nil {
		logf("could not write the handshake journal: %v", err)
		return 1
	}
	logf("handshake recorded; waiting for old process %d to exit", j.PID)

	if !waitForExit(j.PID) {
		logf("old process %d did not exit within %s; aborting update", j.PID, oldPIDPollBound)
		_ = os.Remove(p.State)
		_ = os.Remove(j.NewBinaryPath) // do not leak the staged download
		return 1
	}

	// Perm bits are captured BEFORE the swap replaces the file - once the old
	// binary is gone there is nothing left to stat, and a stat-after-swap would
	// silently leave the new binary non-executable.
	info, err := os.Stat(j.BinaryPath)
	if err != nil {
		return u.abortBeforeSwap(j, p, fmt.Sprintf("could not inspect the old binary: %v", err))
	}
	j.Phase = phaseSwapped
	if err := writeState(u.n, p.State, j); err != nil {
		return u.abortBeforeSwap(j, p, fmt.Sprintf("could not journal the swap: %v", err))
	}
	if err := swapBinary(j.BinaryPath, j.NewBinaryPath, info.Mode().Perm(), p); err != nil {
		return u.abortBeforeSwap(j, p, err.Error())
	}
	logf("new binary in place; relaunching")

	j.Phase = phaseAttesting
	if err := writeState(u.n, p.State, j); err != nil {
		logf("could not journal the attesting phase (proceeding): %v", err)
	}

	if err := u.relaunch(j); err != nil {
		// Undo the swap and bring the old version back. Nothing is running the
		// binary at this point, so the restore is a plain atomic rename-over.
		logf("could not launch the new version: %v - rolling back", err)
		if renErr := os.Rename(p.RollbackBinary, j.BinaryPath); renErr != nil {
			logf("CRITICAL: could not restore the old binary after a failed relaunch: %v", renErr)
			return 1
		}
		j.Phase = phaseRolledBack
		j.ErrorMessage = "could not launch the new version: " + err.Error()
		if err := writeState(u.n, p.State, j); err != nil {
			logf("could not journal the rollback: %v", err)
		}
		if err := u.relaunch(j); err != nil {
			logf("CRITICAL: could not relaunch the old version after rollback: %v", err)
			return 1
		}
		logf("old version relaunched; rollback recorded for reporting")
		return 0
	}
	logf("new version launched; it owns attestation and commit/rollback")
	return 0
}

// runRelaunch waits for the journal's process to exit and then restarts the
// binary - the Windows stand-in for exec-in-place, used by a plain restart and
// by a rollback. statePath is removed afterwards when it is the restart journal,
// which carries nothing anyone needs to read later; an update journal is left
// alone, because its outcome still has to be reported.
func (u *Updater) runRelaunch(j *state, p paths, statePath string) int {
	logf("relaunch requested; waiting for process %d to exit", j.PID)
	if !waitForExit(j.PID) {
		logf("process %d did not exit within %s; aborting relaunch", j.PID, oldPIDPollBound)
		return 1
	}
	if err := u.relaunch(j); err != nil {
		logf("CRITICAL: could not relaunch the binary: %v", err)
		return 1
	}
	if statePath == p.Restart {
		_ = os.Remove(statePath)
	}
	logf("binary relaunched")
	return 0
}

// abortBeforeSwap records a rolled-back journal after the old process has
// already exited but the swap could not be applied. The old binary is intact on
// disk but nothing is running it; the journal survives so the next start
// (manual, or through a service recovery action) reports the failure with its
// cause.
func (u *Updater) abortBeforeSwap(j *state, p paths, cause string) int {
	logf("swap failed: %s", cause)
	j.Phase = phaseRolledBack
	j.ErrorMessage = cause
	if err := writeState(u.n, p.State, j); err != nil {
		logf("could not journal the swap failure: %v", err)
	}
	_ = os.Remove(j.NewBinaryPath)
	return 1
}

// relaunch starts the binary recorded in the journal: through the service
// control manager when a service name is recorded (so the successor IS the
// service, with the right status, recovery actions and shutdown handling), or as
// a detached console process otherwise. The console spawn inherits this helper's
// environment minus the two control variables, so the child starts normally
// rather than re-entering helper mode.
func (u *Updater) relaunch(j *state) error {
	if j.ServiceName != "" {
		return startService(j.ServiceName)
	}
	_, err := spawnDetached(j.BinaryPath, j.Args, j.Cwd, childEnv(u.n, os.Environ()))
	return err
}

// waitForExit polls until the PID is gone or the bound elapses. Polling is valid
// here because the waited-on process is never the helper's own child, so there
// is no zombie to reap.
func waitForExit(pid int) bool {
	deadline := time.Now().Add(oldPIDPollBound)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(oldPIDPollInterval)
	}
	return !pidAlive(pid)
}

// childEnv returns environ with the update-control variables removed, so a
// relaunched program starts normally instead of re-entering a helper or
// selftest role.
func childEnv(n names, environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, n.envMode+"=") ||
			strings.HasPrefix(kv, n.envState+"=") ||
			strings.HasPrefix(kv, n.envSelftest+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// openHelperLog directs helper logging to the file beside the binary.
func openHelperLog(n names, p paths) {
	helperLogPrefix = n.logPrefix
	f, err := os.OpenFile(p.HelperLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		helperLogFile = f
	}
}

func closeHelperLog() {
	if helperLogFile != nil {
		_ = helperLogFile.Close()
		helperLogFile = nil
	}
}

// logf writes a helper log line to stderr (a no-op when detached) and to the
// helper log file when open.
//
// The helper cannot use the caller's Logger: it runs before any of the program's
// own initialisation, in a process that has no console and shares nothing with
// the configured logging setup.
func logf(format string, args ...any) {
	line := fmt.Sprintf(helperLogPrefix+format+"\n", args...)
	fmt.Fprint(os.Stderr, line)
	if helperLogFile != nil {
		fmt.Fprintf(helperLogFile, "%s %s", time.Now().Format(time.RFC3339), line)
	}
}
