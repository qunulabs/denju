package denju

import (
	"fmt"
	"os"
	"time"
)

// Repair resolves in-flight update FILE STATE at startup, before the program
// does anything else. It reads the journal and reconciles the on-disk binary; it
// never touches the network.
//
// It may restart the process as the previous binary - a crash-loop rollback - in
// which case it never returns. Otherwise it returns once the file state is
// consistent and normal startup may proceed.
//
// Call it BEFORE acquiring a single-instance lock or binding a port: a rollback
// restart hands off to a successor that needs both. A returned error means the
// on-disk state could not be made sense of, which is worth failing startup over
// - continuing would risk resolving it wrongly later.
//
// Outcomes concluded here are precisely the ones no running process could have
// recorded, because the process that started the update is gone: it crashed, or
// it exited to let a helper take over. Recording them is not bookkeeping - the
// record is the cooldown's only anchor and the only input a report has - so
// without it a version that crashes on startup is rolled back and then
// immediately reapplied, forever.
func (u *Updater) Repair() error {
	p := u.paths
	if !exists(p.State) {
		// No update in flight. Reclaim leftovers: a discard file (a rolled-back
		// Windows binary, undeletable while its process ran), a helper copy, a
		// stale restart journal, or a helper log.
		_ = os.Remove(p.Discard)
		_ = os.Remove(p.HelperCopy)
		_ = os.Remove(p.HelperLog)
		_ = os.Remove(p.Restart)
		return nil
	}
	j, err := readState(p.State)
	if err != nil {
		return u.repairCorruptJournal(err)
	}

	switch j.Phase {
	case phaseStaged:
		return u.repairStagedHandoff(j)
	case phaseSwapped, phaseAttesting:
		if u.selfIsNewVersion(j) {
			return u.repairNewImage(j)
		}
		return u.repairInterruptedOldImage(j)
	case phaseRolledBack, phaseCommitted:
		// A decided outcome: leave it for the reporter, which delivers it and
		// only then cleans up.
		return nil
	default:
		return u.repairStale(j)
	}
}

// repairStagedHandoff resolves a journal stuck at phaseStaged - a Windows
// handoff whose original process exited but whose helper never reached the swap.
// A still-running helper is left in control (this start may be a fast manual
// restart racing it). A dead helper means the handoff is over: journal the
// rollback so the outcome is reported, and reclaim the staged download.
//
// The outcome is recorded as failed rather than rolled back: the helper died
// before the swap, so the binary on disk was never replaced and there was
// nothing to roll back. Either way it holds the cooldown, so a handoff that
// cannot start is not retried in a tight loop.
func (u *Updater) repairStagedHandoff(j *state) error {
	if pidAlive(j.HelperPID) {
		u.log(LevelWarn, "an update helper is still running; leaving the journal to it",
			"helper_pid", j.HelperPID)
		return nil
	}
	j.Phase = phaseRolledBack
	if j.ErrorMessage == "" {
		j.ErrorMessage = "update interrupted before the swap"
	}
	if err := writeState(u.n, u.paths.State, j); err != nil {
		return fmt.Errorf("mark the interrupted handoff rolled back: %w", err)
	}
	_ = os.Remove(j.NewBinaryPath)
	u.recordRepairOutcome(j, StatusFailed, j.ErrorMessage)
	return nil
}

// repairNewImage handles startup as the freshly-swapped new image while the
// update is still uncommitted. It refreshes the journal's PID and increments the
// crash counter; once the count exceeds the tolerance it rolls back to the
// previous binary and restarts as it, never returning. Below the tolerance it
// returns so startup - and the attestation that follows - can proceed.
func (u *Updater) repairNewImage(j *state) error {
	j.CrashCount++
	j.PID = os.Getpid()
	if err := writeState(u.n, u.paths.State, j); err != nil {
		return fmt.Errorf("update the self-update crash counter: %w", err)
	}
	if j.CrashCount <= u.cfg.CrashTolerance {
		return nil // give the new image its startup + attestation window
	}

	cause := fmt.Sprintf("new version crashed on startup %d times", j.CrashCount)
	u.log(LevelError, "rolling back a crash-looping new version",
		"target_version", j.TargetVersion,
		"old_version", j.OldVersion,
		"crash_count", j.CrashCount)
	// Recorded BEFORE the rollback, because a successful rollback restarts the
	// process and never comes back here.
	u.recordRepairOutcome(j, StatusRolledBack, cause)
	return u.rollbackAndRestart(j, cause)
}

// repairInterruptedOldImage handles startup as the OLD image while the journal
// is still uncommitted - a rollback restore happened but the restart into the
// old image did not. It marks the update rolled back (preserving any existing
// cause) so the outcome is reported, and lets the old image carry on.
func (u *Updater) repairInterruptedOldImage(j *state) error {
	j.Phase = phaseRolledBack
	if j.ErrorMessage == "" {
		j.ErrorMessage = "update interrupted before completion"
	}
	if err := writeState(u.n, u.paths.State, j); err != nil {
		return fmt.Errorf("mark the interrupted update rolled back: %w", err)
	}
	u.recordRepairOutcome(j, StatusRolledBack, j.ErrorMessage)
	return nil
}

// recordRepairOutcome persists an outcome that startup repair decided.
//
// A failure here is logged and swallowed on purpose: the caller is either about
// to roll a binary back or has already reconciled the files, and neither should
// be abandoned because a record could not be written. Losing the record costs a
// cooldown and an outcome report; refusing to start, or leaving the binary
// half-resolved, costs the host its program.
//
// An outcome already recorded for this update is not overwritten. Repair runs on
// every start, and re-recording would keep pushing the cooldown anchor forward,
// turning a single failed update into an indefinite refusal.
func (u *Updater) recordRepairOutcome(j *state, status Status, cause string) {
	if j.CommandID == "" {
		return // nothing to report against, and so nothing to hold
	}
	prev, err := u.records.load()
	if err != nil {
		u.log(LevelError, "could not read the update record", "err", err)
		return
	}
	if prev != nil && prev.ID == j.CommandID && prev.Status == status {
		return
	}
	if err := u.records.record(&Outcome{
		At:          time.Now(),
		ID:          j.CommandID,
		FromVersion: j.OldVersion,
		ToVersion:   j.TargetVersion,
		Status:      status,
		Error:       cause,
	}); err != nil {
		u.log(LevelError, "could not write the update record", "err", err)
	}
}

// repairStale resolves a journal in an unrecognized phase. If it is older than
// the stale threshold and its recorded processes are gone, it resolves
// deterministically: an on-disk binary matching new_sha256 that never committed
// is put back to the rollback copy. A fresh or still-live journal is untouched.
func (u *Updater) repairStale(j *state) error {
	if !u.isStaleJournal(j) {
		return nil
	}
	if sum, err := sha256File(u.binaryPath); err == nil && sum == j.NewSHA256 && j.Phase != phaseCommitted {
		if exists(u.paths.RollbackBinary) {
			// Atomic rename-over: the uncommitted new binary is replaced in place.
			_ = os.Rename(u.paths.RollbackBinary, u.binaryPath)
		}
	}
	u.removeLeftovers()
	return nil
}

// repairCorruptJournal handles a journal that will not parse. If it is stale by
// age it is garbage-collected; otherwise the parse error is surfaced, because a
// fresh unexpected corruption is a real problem and must not be swallowed.
func (u *Updater) repairCorruptJournal(readErr error) error {
	info, err := os.Stat(u.paths.State)
	if err == nil && time.Since(info.ModTime()) > u.cfg.StaleThreshold {
		u.removeLeftovers()
		return nil
	}
	return readErr
}

// isStaleJournal reports whether the journal is older than the stale threshold
// AND both its recorded processes are dead - the signature of a crashed,
// orphaned update.
func (u *Updater) isStaleJournal(j *state) bool {
	info, err := os.Stat(u.paths.State)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) <= u.cfg.StaleThreshold {
		return false
	}
	return !pidAlive(j.PID) && !pidAlive(j.HelperPID)
}
