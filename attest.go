package denju

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Attest arms a deadline for the update this process is the result of. If
// neither [Updater.Commit] nor [Updater.Rollback] is called within d, denju
// restores the previous binary and restarts into it.
//
// Call it once the program is up but before it is trusted - typically at the end
// of startup. It returns immediately; the deadline runs in the background. It
// does nothing at all when no update is in flight, which is the overwhelmingly
// common case, so it is safe to call unconditionally on every start.
//
// Cancelling ctx disarms the deadline WITHOUT deciding the update. That is
// deliberate: a program shutting down mid-window has not failed, and rolling it
// back on the way out would replace a working binary during an operation nobody
// is watching. The journal survives, and the next start resumes the window.
//
// What counts as proof is the caller's to define, because only the caller knows.
// Registering with a control plane, passing a health probe, completing one real
// request - any of these is a better signal than "the process is still alive".
func (u *Updater) Attest(ctx context.Context, d time.Duration) {
	j, ok := u.pendingAttestation()
	if !ok {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	u.mu.Lock()
	if u.attestCancel != nil {
		u.attestCancel() // replace any window already armed
	}
	u.attestCancel = cancel
	u.mu.Unlock()

	u.log(LevelInfo, "attestation window armed",
		"target_version", j.TargetVersion,
		"previous_version", j.OldVersion,
		"window", d)

	go func() {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cause := fmt.Sprintf("the new version did not attest healthy within %s", d)
			if err := u.Rollback(cause); err != nil {
				u.log(LevelError, "CRITICAL: could not roll back after a failed attestation", "err", err)
			}
		}
	}()
}

// Pending describes an update that has been installed but not yet judged: this
// process is running the new version, and nothing has committed or rolled it
// back. Returned by [Updater.PendingAttestation].
type Pending struct {
	// ID echoes [Request.ID], so a verdict can be tied back to what asked for
	// the update.
	ID string
	// FromVersion and ToVersion describe the move that was made.
	FromVersion string
	ToVersion   string
}

// PendingAttestation reports whether an update is waiting for a verdict from
// THIS process, and what it was.
//
// [Updater.Attest] is the usual way to resolve one and needs no such check: it
// arms a deadline and rolls back if nothing decides in time, doing nothing at
// all when no update is in flight. This is for a caller that would rather judge
// health its own way — a registration accepted, a probe that has to pass several
// times, a real request served end to end — and so needs to know whether to
// start that work at all.
//
// The distinction matters because health checks are rarely free. Running one on
// every ordinary start, just in case this start happens to follow an update, is
// both wasteful and a surprise to whoever wrote the check.
//
// It is a query and changes nothing. False means there is no journal, the update
// has already been decided, or this process is the OLD version rather than the
// new one — an old image reaching here has already been handled by
// [Updater.Repair], and letting it attest would confirm an update that never
// took effect.
func (u *Updater) PendingAttestation() (Pending, bool) {
	j, ok := u.pendingAttestation()
	if !ok {
		return Pending{}, false
	}
	return Pending{
		ID:          j.CommandID,
		FromVersion: j.OldVersion,
		ToVersion:   j.TargetVersion,
	}, true
}

// Commit accepts the update this process is the result of. The rollback copy of
// the previous binary is released and the outcome is recorded as succeeded,
// ready for [Updater.PendingOutcome] to report.
//
// It is a no-op when no update is in flight, so a caller can wire it to a
// "we're healthy" signal that also fires on ordinary starts.
func (u *Updater) Commit() error { return u.decide(true, "") }

// Rollback rejects the update this process is the result of: the previous
// binary is restored and the process restarts into it. It does not return on
// success.
//
// Rolling back is the right answer even when the cause is ambiguous - an
// unreachable control plane, a dependency that is down. The previous version is
// known to have worked and this one has not been shown to, and guessing which of
// the two a transient failure implicates would be exactly that, a guess. Enable
// [Config.Cooldown] so a rejected version is not immediately reapplied.
//
// It is a no-op when no update is in flight.
func (u *Updater) Rollback(cause string) error { return u.decide(false, cause) }

// pendingAttestation returns the journal of an update awaiting a verdict from
// THIS process, and whether there is one.
//
// Three conditions have to hold. There must be a journal; it must be at a phase
// that has not yet been decided; and this process must be the new image rather
// than the old one - an old image reaching here has already been dealt with by
// startup repair, and letting it "commit" would confirm an update that never
// took effect.
func (u *Updater) pendingAttestation() (*state, bool) {
	if !exists(u.paths.State) {
		return nil, false
	}
	j, err := readState(u.paths.State)
	if err != nil {
		u.log(LevelError, "could not read the update journal", "err", err)
		return nil, false
	}
	if j.Phase != phaseSwapped && j.Phase != phaseAttesting {
		return nil, false
	}
	if !u.selfIsNewVersion(j) {
		return nil, false
	}
	return j, true
}

// decide resolves an in-flight update one way or the other.
//
// The WHOLE verdict is serialised, not just the disarm below. Cancelling the
// deadline narrows the race between a caller's Commit and the deadline
// goroutine's Rollback but cannot close it: the timer may already be past its
// select and inside Rollback by the time Commit cancels the context. Without
// this lock both would clear the pendingAttestation gate and then act on
// opposite verdicts - committing the journal and then restoring the old binary,
// or recording succeeded and rolled_back for one command.
//
// The contended state is the FILESYSTEM, which the race detector cannot see, so
// this has to be reasoned about rather than tested into existence. Whichever
// verdict takes the lock first wins; the loser finds the update already decided
// and returns the documented no-op.
func (u *Updater) decide(ok bool, cause string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Disarm the deadline first: whichever way this goes, the window is over,
	// and a timer that fires afterwards would try to roll back an update that
	// has already been committed.
	if u.attestCancel != nil {
		u.attestCancel()
		u.attestCancel = nil
	}

	j, pending := u.pendingAttestation()
	if !pending {
		return nil
	}

	if ok {
		j.Phase = phaseCommitted
		if err := writeState(u.n, u.paths.State, j); err != nil {
			return fmt.Errorf("journal the commit: %w", err)
		}
		// The rollback copy is released only here - the one point at which the
		// new version has actually been proven.
		_ = os.Remove(u.paths.RollbackBinary)
		u.log(LevelInfo, "new version attested healthy; committed",
			"version", j.TargetVersion,
			"previous_version", j.OldVersion)
		return u.records.record(&Outcome{
			At:          time.Now(),
			ID:          j.CommandID,
			FromVersion: j.OldVersion,
			ToVersion:   j.TargetVersion,
			Status:      StatusSucceeded,
		})
	}

	u.log(LevelError, "new version failed attestation; rolling back",
		"version", j.TargetVersion,
		"cause", cause)
	// Recorded BEFORE the rollback, because a successful rollback restarts the
	// process and never comes back here. This write is what stops a loop: the
	// restored version reads it, holds the cooldown, and can say why it is still
	// on the old version instead of silently accepting the same binary again.
	if err := u.records.record(&Outcome{
		At:          time.Now(),
		ID:          j.CommandID,
		FromVersion: j.OldVersion,
		ToVersion:   j.TargetVersion,
		Status:      StatusRolledBack,
		Error:       cause,
	}); err != nil {
		u.log(LevelError, "could not write the update record", "err", err)
	}
	return u.rollbackAndRestart(j, cause)
}
