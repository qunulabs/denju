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
		// stale restart journal, a helper log, or a partial download nothing has
		// written to for longer than PartialRetention.
		_ = os.Remove(p.Discard)
		_ = os.Remove(p.HelperCopy)
		_ = os.Remove(p.HelperLog)
		_ = os.Remove(p.Restart)
		// Partials only when nothing is in flight: with a journal present a
		// completed partial may be the journal's staged binary.
		u.discardStalePartials(time.Now())
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
			if u.outcomeRecorded(j, StatusSucceeded) {
				return u.reconcileCommitted(j)
			}
			return u.repairNewImage(j)
		}
		return u.repairInterruptedOldImage(j)
	case phaseCommitted:
		if !u.cfg.RetainPrevious {
			_, err := u.repairDecided(j, StatusSucceeded, "")
			return err
		}
		return u.repairCommittedRetaining(j)
	case phaseRolledBack:
		_, err := u.repairDecided(j, StatusRolledBack, j.ErrorMessage)
		return err
	default:
		return u.repairStale(j)
	}
}

// repairDecided handles a journal that already carries a verdict. The files are
// settled, so there is nothing to reconcile on disk and the journal is left for
// the reporter, which delivers the outcome and only then cleans up.
//
// The one thing it does is make sure the outcome was actually RECORDED. Whatever
// decided the update normally writes the record itself, and recordRepairOutcome
// leaves that record alone - so on the overwhelmingly common path this call does
// nothing at all. It matters when the two have come apart:
//
//   - The record was removed or is being looked for somewhere else - a state
//     directory cleaned between runs, or a Config.RecordPath that moved.
//   - The journal was written by an earlier version of the program whose update
//     mechanism was not denju, and which therefore kept no record. A program
//     adopting denju hits this on exactly the update that installs the adopting
//     build, which is the worst possible one to lose: the whole fleet reports
//     nothing for the rollout itself.
//
// Without this, a decided journal with no record is a dead end. PendingOutcome
// returns nothing, so the outcome is never reported and the journal is never
// cleaned up, leaving it and the rollback copy on disk for good.
//
// The status must match what the deciding path records for the same phase, or
// the idempotence guard stops matching and every start rewrites the record,
// pushing the cooldown anchor forward forever.
//
// A record written AFTER the journal took its verdict is left alone too, whatever
// it describes. The record holds only the last attempt, and the journal stays
// until its outcome is reported, so a later attempt - the refusal of a re-issued
// command, a failed download - can replace the decided outcome as the record
// while the journal is still on disk. That is not a missing record. Re-recording
// the verdict over it would restart the cooldown (and under
// CooldownRolledBackVersion the hold) at a full window, report the old outcome a
// second time, and overwrite the later attempt before it was reported. The
// journal's modification time is when it took the verdict: nothing rewrites a
// decided journal.
//
// The same rule keeps a dead Windows handoff a failure. repairStagedHandoff
// journals it rolledback and records it failed, straight after; without the
// rule the next start would find the status mismatch and re-record it as
// rolled_back, which under CooldownRolledBackVersion holds a version that never
// ran.
//
// settled reports whether the outcome is on record - recorded now, already, or
// superseded by a later attempt - so a caller may drop the journal.
func (u *Updater) repairDecided(j *state, status Status, cause string) (settled bool, err error) {
	if j.CommandID == "" {
		return true, nil // nothing to report against, and so nothing to hold
	}
	info, err := os.Stat(u.paths.State)
	if err != nil {
		return false, fmt.Errorf("inspect the update journal: %w", err)
	}
	prev, err := u.records.load()
	if err != nil {
		u.log(LevelError, "could not read the update record", "err", err)
		return false, nil
	}
	if prev != nil && prev.At.After(info.ModTime()) {
		return true, nil
	}
	return u.recordRepairOutcome(j, status, cause), nil
}

// repairCommittedRetaining resolves a committed journal under RetainPrevious.
//
// A retention the commit could not finish - interrupted by a crash, or failed -
// is finished here, on every start while the journal exists. Once it succeeds
// and the outcome is on record, the journal has nothing left to protect, and
// Repair removes it with the other leftovers (the Windows helper copy and its
// log among them). CleanupReported cannot be relied on for that: the documented
// wiring calls it only when PendingOutcome has something, and an outcome
// reported on the start whose retention failed is never pending again. While the
// journal stays, the stale-partial sweep below does not run either.
func (u *Updater) repairCommittedRetaining(j *state) error {
	if err := u.retainPrevious(j); err != nil {
		u.log(LevelError, "could not retain the previous binary; it stays as the rollback copy and is retried on the next start",
			"err", err)
		_, err := u.repairDecided(j, StatusSucceeded, "")
		return err
	}
	settled, err := u.repairDecided(j, StatusSucceeded, "")
	if err != nil || !settled {
		return err
	}
	u.removeLeftovers()
	return nil
}

// repairStagedHandoff resolves a journal stuck at phaseStaged - a Windows
// handoff whose original process exited but whose helper never reached the swap.
// A still-running helper is left in control (this start may be a fast manual
// restart racing it). A dead helper means the handoff is over: journal the
// rollback so the outcome is reported, and reclaim the staged download.
//
// The outcome is recorded as failed rather than rolled back: the helper died
// before the swap, so the binary on disk was never replaced and there was
// nothing to roll back. Under CooldownAnyUpdate a failure holds the cooldown
// like any completed attempt, so a handoff that cannot start is not retried in a
// tight loop. Under CooldownRolledBackVersion it holds nothing, on purpose: that
// scope holds a version that ran and failed, and this target never ran.
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

// outcomeRecorded reports whether the durable record already carries the given
// status for the update this journal describes.
func (u *Updater) outcomeRecorded(j *state, status Status) bool {
	if j.CommandID == "" {
		return false
	}
	prev, err := u.records.load()
	if err != nil {
		u.log(LevelError, "could not read the update record", "err", err)
		return false
	}
	return prev != nil && prev.ID == j.CommandID && prev.Status == status
}

// reconcileCommitted finishes a commit whose journal write did not land.
//
// The record is the verdict and the journal only follows it, so when the two
// disagree this way - record says succeeded, journal still says attesting - the
// update WAS accepted, by a process that then could not write the phase or died
// between the two writes. Without this the crash counter would climb on every
// start and eventually roll back a version that attested healthy.
//
// A journal that still cannot be written is logged and left. The point of this
// path is to stop the rollback, and that has already been achieved by getting
// here; failing startup over an unwritable file would take a healthy program
// down for bookkeeping.
func (u *Updater) reconcileCommitted(j *state) error {
	u.log(LevelWarn, "the journal does not record the commit that the outcome record does; reconciling",
		"target_version", j.TargetVersion,
		"phase", j.Phase)
	j.Phase = phaseCommitted
	if err := writeState(u.n, u.paths.State, j); err != nil {
		u.log(LevelError, "could not reconcile the committed journal", "err", err)
		return nil
	}
	u.releaseRollbackCopy(j)
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
	if err := u.rollbackAndRestart(j, cause); err != nil {
		u.correctFailedRollback(j, err)
		return err
	}
	return nil
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
//
// It reports whether the outcome is on record afterwards.
func (u *Updater) recordRepairOutcome(j *state, status Status, cause string) bool {
	if j.CommandID == "" {
		return true // nothing to report against, and so nothing to hold
	}
	prev, err := u.records.load()
	if err != nil {
		u.log(LevelError, "could not read the update record", "err", err)
		return false
	}
	if prev != nil && prev.ID == j.CommandID && prev.Status == status {
		return true
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
		return false
	}
	return true
}

// correctFailedRollback rewrites a rolled_back record once the rollback it
// describes turns out not to have happened.
//
// A rollback is recorded before it is attempted, because a successful one
// restarts the process and never comes back to record anything. That ordering is
// right and leaves exactly one thing to repair: when the restore fails, the
// record - and therefore the report - says the machine went back to the old
// version while it is in fact still running the new one. An operator chasing a
// bad rollout would be looking at the wrong host.
//
// It writes through the store directly rather than through recordRepairOutcome,
// whose idempotence guard exists to stop repeated starts pushing the cooldown
// anchor forward and would read this correction as a duplicate and drop it.
func (u *Updater) correctFailedRollback(j *state, cause error) {
	u.log(LevelError, "CRITICAL: the rollback was recorded but could not be carried out",
		"target_version", j.TargetVersion,
		"old_version", j.OldVersion,
		"err", cause)
	if j.CommandID == "" {
		return
	}
	if err := u.records.record(&Outcome{
		At:          time.Now(),
		ID:          j.CommandID,
		FromVersion: j.OldVersion,
		ToVersion:   j.TargetVersion,
		Status:      StatusFailed,
		Error:       "rollback failed: " + cause.Error(),
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
