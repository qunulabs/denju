package denju

import (
	"fmt"
	"os"
)

// osExit terminates the process. A seam so tests of the Windows restart path can
// observe the exit instead of dying.
//
// This is the one place denju ends a process, and only ever on Windows, which
// has no exec: a handover there is necessarily "this process stops so the helper
// can continue". On Unix nothing here exits - the image is replaced in place.
var osExit func(code int) = os.Exit

// Restart restarts the program as the binary it is already running, without
// changing that binary.
//
// On Unix it is exec-in-place: the same PID, argv and env replayed verbatim,
// never returning on success, with no supervisor involved. On Windows - which
// has no exec - it hands off to a detached helper that waits for this process to
// exit and then brings the program back, through the service control manager
// when it runs as a service. It returns only on failure.
//
// Call it for a caller-requested restart. An update restarts on its own.
func (u *Updater) Restart() error {
	if goos != "windows" {
		return execSelf(u.binaryPath, os.Args, os.Environ())
	}

	serviceName, err := u.resolveServiceName()
	if err != nil {
		return fmt.Errorf("determine how to restart: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("read the working directory: %w", err)
	}

	// The helper needs to know which PID to wait for and how to bring the
	// program back. This journal is separate from the update journal on purpose:
	// a restart has no phase, no rollback copy and no outcome, and must never
	// overwrite an in-flight update's write-ahead record.
	j := &state{
		BinaryPath:  u.binaryPath,
		Args:        os.Args[1:],
		Cwd:         cwd,
		PID:         os.Getpid(),
		ServiceName: serviceName,
	}
	return u.relaunchViaHelper(u.paths.Restart, j)
}

// rollbackAndRestart restores the old binary, journals phaseRolledBack with the
// cause, and restarts into it. It never returns on success; an error means the
// process is still running the new image and the caller must fail loudly.
//
// The restore deliberately precedes the journal write. Journaling rolledback
// first would, if the restore then failed, leave a journal claiming a rollback
// that never happened - the next start would report a rollback while the NEW
// binary is still running, and cleanup would delete <binary>.old, destroying the
// only copy of the old version. Restore-first is also crash-safe: a crash
// between the restore and the journal write leaves the old binary on disk under
// an uncommitted journal, which repair resolves to rolledback on the next start.
func (u *Updater) rollbackAndRestart(j *state, cause string) error {
	if err := restoreOldBinary(u.binaryPath, u.paths); err != nil {
		return err
	}
	j.Phase = phaseRolledBack
	j.ErrorMessage = cause
	j.PID = os.Getpid()
	if err := writeState(u.n, u.paths.State, j); err != nil {
		return fmt.Errorf("journal the rollback: %w", err)
	}
	if goos != "windows" {
		return execSelf(u.binaryPath, os.Args, os.Environ())
	}
	// The helper reads the SAME update journal, so the rolledback record stays
	// intact for the successor to report.
	return u.relaunchViaHelper(u.paths.State, j)
}

// relaunchViaHelper is the Windows restart primitive: ensure a helper copy
// exists, write the journal the helper will read, spawn the helper detached in
// relaunch mode, and exit so the helper's wait can complete. Never returns on
// success.
func (u *Updater) relaunchViaHelper(journalPath string, j *state) error {
	if !exists(u.paths.HelperCopy) {
		// The helper copy normally survives from an apply handoff; recreate it
		// for a plain restart, or if something reclaimed it.
		if err := copyBinary(u.binaryPath, u.paths.HelperCopy); err != nil {
			return err
		}
	}
	if err := writeState(u.n, journalPath, j); err != nil {
		return fmt.Errorf("journal the relaunch: %w", err)
	}
	env := append(childEnv(u.n, os.Environ()),
		u.n.envMode+"="+modeRelaunch,
		u.n.envState+"="+journalPath,
	)
	if _, err := spawnDetached(u.paths.HelperCopy, j.Args, j.Cwd, env); err != nil {
		return fmt.Errorf("launch the relaunch helper: %w", err)
	}
	osExit(0)
	return nil // unreachable in production; reached only through the test exit seam
}
