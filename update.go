package denju

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"
)

// Update downloads, verifies and installs the binary described by req, then
// hands over to it.
//
// It does not return when it succeeds. On Unix the process image is replaced in
// place - same PID, same argv, same environment - so no supervisor observes a
// restart. On Windows the process exits so a detached helper can perform the
// swap and start the program again. A returned [Result] therefore always
// describes an update that did NOT happen, with the program still running
// exactly as it was.
//
// The order of what follows is load-bearing. Everything that can fail cheaply
// happens while the running program is completely untouched: the platform
// check, the cooldown, the preflight, the download, the digest, and a selftest
// that actually EXECUTES the new binary. Only once all of that has passed does
// the program drain, journal and swap. The common failures - wrong platform,
// corrupt transfer, unrunnable binary, no disk space, no write permission -
// therefore cost nothing but a log line and a recorded outcome.
//
// Update is not safe to call concurrently with itself.
func (u *Updater) Update(ctx context.Context, req Request, src Source) Result {
	if req.TargetVersion == "" || req.SHA256 == "" {
		return u.fail(req, "the update request is incomplete: TargetVersion and SHA256 are required")
	}
	if req.TargetOS != "" && req.TargetOS != runtime.GOOS ||
		req.TargetArch != "" && req.TargetArch != runtime.GOARCH {
		return u.fail(req, fmt.Sprintf("the update targets %s/%s but this program runs %s/%s",
			req.TargetOS, req.TargetArch, runtime.GOOS, runtime.GOARCH))
	}
	if req.TargetVersion == u.cfg.Version {
		return u.fail(req, "already running "+u.cfg.Version)
	}

	// The cooldown is checked before anything expensive. Where it matters, it is
	// the branch most requests take.
	inCooldown, left, err := u.records.inCooldown(time.Now())
	if err != nil {
		return u.fail(req, "could not read the update record: "+err.Error())
	}
	if inCooldown {
		msg := fmt.Sprintf("another update completed less than %s ago; refusing for another %s",
			u.cfg.Cooldown.Round(time.Minute), left.Round(time.Second))
		u.log(LevelWarn, "refused by cooldown",
			"target_version", req.TargetVersion,
			"current_version", u.cfg.Version,
			"remaining", left)
		return u.record(req, StatusRefusedCooldown, msg)
	}

	if err := preflightCheck(u.n, u.binaryPath); err != nil {
		return u.fail(req, err.Error())
	}

	// Windows resolves how it will restart BEFORE anything is disturbed: a
	// service-hosted program whose service name cannot be resolved has no way
	// back, and must abort now rather than after its binary is gone.
	serviceName, err := u.resolveServiceName()
	if err != nil {
		return u.fail(req, "could not determine how to restart: "+err.Error())
	}

	cwd, err := os.Getwd()
	if err != nil {
		return u.fail(req, "could not read the working directory: "+err.Error())
	}
	args := os.Args[1:]

	u.log(LevelInfo, "downloading",
		"from_version", u.cfg.Version,
		"target_version", req.TargetVersion,
		"id", req.ID)

	stagedPath, err := u.download(ctx, req, src)
	if err != nil {
		return u.fail(req, err.Error())
	}

	// Execute the new binary before trusting it. This is the last check that
	// costs nothing: the running program is still intact, so a binary that
	// cannot start is simply deleted.
	if err := u.runSelftest(stagedPath, args, cwd); err != nil {
		_ = os.Remove(stagedPath)
		return u.fail(req, err.Error())
	}

	j := &state{
		CommandID:     req.ID,
		TargetVersion: req.TargetVersion,
		OldVersion:    u.cfg.Version,
		BinaryPath:    u.binaryPath,
		NewBinaryPath: stagedPath,
		Args:          args,
		Cwd:           cwd,
		StartedAt:     time.Now(),
		NewSHA256:     req.SHA256,
		PID:           os.Getpid(),
		ServiceName:   serviceName,
	}

	if goos == "windows" {
		return u.windowsHandoff(j, stagedPath)
	}
	return u.swapAndExec(j, stagedPath)
}

// swapAndExec is the Unix path: drain, journal, swap, exec-in-place. It never
// returns on success.
func (u *Updater) swapAndExec(j *state, stagedPath string) Result {
	if err := u.drain(); err != nil {
		_ = os.Remove(stagedPath)
		return u.failJournal(j, "prepare-for-update failed: "+err.Error())
	}

	// Capture perm bits and the old digest BEFORE any rename. Once the old
	// binary has been linked aside and replaced there is nothing left to stat,
	// and a stat-after-swap would silently leave the new binary non-executable.
	info, err := os.Stat(u.binaryPath)
	if err != nil {
		_ = os.Remove(stagedPath)
		return u.failJournal(j, "could not inspect the binary: "+err.Error())
	}
	oldSHA, err := sha256File(u.binaryPath)
	if err != nil {
		_ = os.Remove(stagedPath)
		return u.failJournal(j, "could not hash the binary: "+err.Error())
	}
	j.OldSHA256 = oldSHA
	j.Phase = phaseSwapped

	if err := writeState(u.n, u.paths.State, j); err != nil {
		_ = os.Remove(stagedPath)
		return u.failJournal(j, "could not journal the swap: "+err.Error())
	}
	if err := swapBinary(u.binaryPath, stagedPath, info.Mode().Perm(), u.paths); err != nil {
		_ = os.Remove(u.paths.State)
		_ = os.Remove(stagedPath)
		return u.failJournal(j, err.Error())
	}

	j.Phase = phaseAttesting
	if err := writeState(u.n, u.paths.State, j); err != nil {
		u.log(LevelError, "could not journal the attesting phase (proceeding)", "err", err)
	}
	u.log(LevelInfo, "swapped in the new binary; exec-ing it in place",
		"target_version", j.TargetVersion,
		"pid", os.Getpid())

	u.beforeHandoff()

	if err := execSelf(u.binaryPath, os.Args, os.Environ()); err != nil {
		// Still running the old image, so undo the swap with a single atomic
		// rename-over and exec what we restored.
		u.log(LevelError, "exec of the new image failed; rolling back", "err", err)
		j.Phase = phaseRolledBack
		j.ErrorMessage = "exec failed: " + err.Error()
		if renErr := os.Rename(u.paths.RollbackBinary, u.binaryPath); renErr != nil {
			u.log(LevelError, "CRITICAL: could not restore the old binary after a failed exec", "err", renErr)
			return u.failJournal(j, "exec failed and the old binary could not be restored: "+renErr.Error())
		}
		if wErr := writeState(u.n, u.paths.State, j); wErr != nil {
			u.log(LevelError, "could not journal the rollback", "err", wErr)
		}
		if execErr := execSelf(u.binaryPath, os.Args, os.Environ()); execErr != nil {
			u.log(LevelError, "CRITICAL: could not exec the restored old binary", "err", execErr)
		}
		return u.record(Request{ID: j.CommandID, TargetVersion: j.TargetVersion}, StatusRolledBack, j.ErrorMessage)
	}
	// Unreachable: a successful exec never returns.
	return Result{Status: StatusSucceeded}
}

// windowsHandoff is the Windows path. Windows cannot overwrite a running .exe
// and has no exec, so the swap is performed by a detached COPY of this binary
// after this process has exited.
//
// The order differs from Unix on purpose: the helper is spawned and its
// handshake confirmed BEFORE the drain, so a helper that cannot start aborts the
// update without the program ever having stopped serving.
func (u *Updater) windowsHandoff(j *state, stagedPath string) Result {
	p := u.paths
	j.Phase = phaseStaged

	oldSHA, err := sha256File(u.binaryPath)
	if err != nil {
		_ = os.Remove(stagedPath)
		return u.failJournal(j, "could not hash the binary: "+err.Error())
	}
	j.OldSHA256 = oldSHA

	if err := writeState(u.n, p.State, j); err != nil {
		_ = os.Remove(stagedPath)
		return u.failJournal(j, "could not journal the update: "+err.Error())
	}
	if err := copyBinary(u.binaryPath, p.HelperCopy); err != nil {
		u.abortHandoff(p, stagedPath)
		return u.failJournal(j, err.Error())
	}

	// The helper is spawned with the program's own args, so it must survive
	// whatever main does with them before it reaches RunProcessRole.
	env := append(os.Environ(),
		u.n.envMode+"="+modeApply,
		u.n.envState+"="+p.State,
	)
	helperPID, err := spawnDetached(p.HelperCopy, j.Args, j.Cwd, env)
	if err != nil {
		u.abortHandoff(p, stagedPath)
		return u.failJournal(j, "could not start the update helper: "+err.Error())
	}

	// Handshake: the helper writes its PID into the journal as its first act.
	// Waiting for it proves the helper reached its updater role rather than
	// dying somewhere in normal program startup - checked before this process
	// commits to exiting.
	if !u.waitForHandshake(p.State) {
		_ = killPID(helperPID)
		u.abortHandoff(p, stagedPath)
		return u.failJournal(j, "the update helper did not start within "+u.cfg.HandshakeTimeout.String())
	}

	if err := u.drain(); err != nil {
		_ = killPID(helperPID)
		u.abortHandoff(p, stagedPath)
		return u.failJournal(j, "prepare-for-update failed: "+err.Error())
	}

	u.log(LevelInfo, "handing off to the update helper",
		"helper_pid", helperPID,
		"target_version", j.TargetVersion)

	u.beforeHandoff()

	osExit(0)
	// Unreachable in production; reached only through the test exit seam.
	return Result{Status: StatusSucceeded}
}

// abortHandoff undoes a Windows handoff that never got off the ground.
func (u *Updater) abortHandoff(p paths, stagedPath string) {
	_ = os.Remove(p.State)
	_ = os.Remove(p.HelperCopy)
	_ = os.Remove(stagedPath)
}

// waitForHandshake polls the journal for the helper's PID.
func (u *Updater) waitForHandshake(statePath string) bool {
	deadline := time.Now().Add(u.cfg.HandshakeTimeout)
	for {
		if j, err := readState(statePath); err == nil && j.HelperPID != 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(handshakePoll)
	}
}

// killPID terminates a process we spawned and then decided not to use.
func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// drain gives the program a bounded window to quiesce. An error from the
// callback aborts the update; a TIMEOUT does not.
//
// A stuck drain must not be able to pin a program on a broken version forever.
// Work abandoned by an overrunning drain is usually recoverable - retried,
// re-dispatched, or simply lost - whereas an update that can never be applied
// needs a human on the host.
func (u *Updater) drain() error {
	if u.cfg.Drain == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), u.cfg.DrainTimeout)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- u.cfg.Drain(ctx) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		u.log(LevelWarn, "drain did not finish in time; proceeding", "timeout", u.cfg.DrainTimeout)
		return nil
	}
}

// beforeHandoff runs the caller's last-word callback. It is past the point of no
// return, so a panic here must not leave the update half-applied: the binary is
// already swapped and the only sane continuation is to hand over anyway.
func (u *Updater) beforeHandoff() {
	if u.cfg.BeforeHandoff == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			u.log(LevelError, "BeforeHandoff panicked; handing over anyway", "panic", r)
		}
	}()
	u.cfg.BeforeHandoff()
}

// fail logs and records a failure that left the program untouched.
func (u *Updater) fail(req Request, msg string) Result {
	u.log(LevelError, "refused",
		"target_version", req.TargetVersion,
		"current_version", u.cfg.Version,
		"reason", msg)
	return u.record(req, StatusFailed, msg)
}

// failJournal is fail for a failure that happened after the journal existed; it
// clears the journal so the next start does not try to resolve a dead update.
func (u *Updater) failJournal(j *state, msg string) Result {
	_ = os.Remove(u.paths.State)
	return u.fail(Request{ID: j.CommandID, TargetVersion: j.TargetVersion}, msg)
}

// record persists the outcome so it survives a restart, and returns it. The
// store owns the cooldown-anchor rule, so a refusal recorded here leaves the
// cooldown exactly where the last completed update put it.
//
// A request with no ID records nothing: there is nobody to report to.
func (u *Updater) record(req Request, status Status, msg string) Result {
	if req.ID == "" {
		return Result{Status: status, Error: msg}
	}
	o := &Outcome{
		At:          time.Now(),
		ID:          req.ID,
		FromVersion: u.cfg.Version,
		ToVersion:   req.TargetVersion,
		Status:      status,
		Error:       msg,
	}
	if err := u.records.record(o); err != nil {
		u.log(LevelError, "could not write the update record", "err", err)
	}
	return Result{Status: status, Error: msg}
}
