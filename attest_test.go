package denju

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// attestFixture is a new image running under an uncommitted journal - the exact
// situation attestation exists to resolve.
type attestFixture struct {
	u      *Updater
	bin    string
	execCh chan struct{}
}

func newAttestFixture(t *testing.T, phase string) *attestFixture {
	t.Helper()
	withGOOS(t, "linux")

	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "new-binary")

	u, err := New(Config{
		Namespace:  "app",
		Version:    "1.5.0", // the TARGET version: this process is the new image
		BinaryPath: bin,
		RecordPath: filepath.Join(t.TempDir(), "last-update.json"),
		Cooldown:   testCooldown,
	})
	noErr(t, err, "New")
	writeBinary(t, u.paths.RollbackBinary, "old-binary")

	stageJournal(t, u, &state{
		Phase:         phase,
		CommandID:     "cmd-1",
		OldVersion:    "1.4.0",
		TargetVersion: "1.5.0",
	})

	execCh := make(chan struct{}, 1)
	prev := execSelf
	execSelf = func(string, []string, []string) error {
		select {
		case execCh <- struct{}{}:
		default:
		}
		return nil
	}
	t.Cleanup(func() { execSelf = prev })

	return &attestFixture{u: u, bin: bin, execCh: execCh}
}

func TestCommit_NoUpdateInFlightIsANoOp(t *testing.T) {
	u := testUpdater(t)
	noErr(t, u.Commit(), "Commit with no journal")
	noErr(t, u.Rollback("nothing to undo"), "Rollback with no journal")
}

// Committing releases the rollback copy - the one point at which the new version
// has actually been proven - and records the success for reporting.
func TestCommit_MarksCommittedAndReleasesTheRollbackCopy(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)

	noErr(t, f.u.Commit(), "Commit")

	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "phase")
	isFalse(t, exists(f.u.paths.RollbackBinary), "the rollback copy is released on commit")
	eq(t, readFileString(t, f.bin), "new-binary", "the new binary stays in place")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("a commit must be recorded so it can be reported")
	}
	eq(t, rec.Status, StatusSucceeded, "status")
	eq(t, rec.ID, "cmd-1", "id")
	eq(t, rec.FromVersion, "1.4.0", "from version")
	eq(t, rec.ToVersion, "1.5.0", "to version")
}

// A commit from the OLD image would confirm an update that never took effect.
// Startup repair has already dealt with that case; attestation must not undo it.
func TestCommit_FromTheOldImageIsARefusedNoOp(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)
	f.u.cfg.Version = "1.4.0" // we are the old image

	noErr(t, f.u.Commit(), "Commit")

	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseAttesting, "the journal must be left for repair to resolve")
	isTrue(t, exists(f.u.paths.RollbackBinary), "the rollback copy must survive")
}

func TestCommit_AlreadyDecidedIsANoOp(t *testing.T) {
	for _, phase := range []string{phaseCommitted, phaseRolledBack} {
		f := newAttestFixture(t, phase)
		noErr(t, f.u.Commit(), "Commit")

		j, err := readState(f.u.paths.State)
		noErr(t, err, "readState")
		eq(t, j.Phase, phase, "a decided outcome is not re-decided")
	}
}

// Rolling back restores the previous binary, records why, and restarts into it.
func TestRollback_RestoresAndRestarts(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)

	noErr(t, f.u.Rollback("the health probe never passed"), "Rollback")

	select {
	case <-f.execCh:
	default:
		t.Fatal("a rollback must restart into the restored binary")
	}
	eq(t, readFileString(t, f.bin), "old-binary", "the old binary is back")

	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	eq(t, j.ErrorMessage, "the health probe never passed", "the cause is journaled")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("a rollback must be recorded so it can be reported")
	}
	eq(t, rec.Status, StatusRolledBack, "status")
	hasSubstr(t, rec.Error, "health probe", "recorded cause")

	// The record must hold the cooldown, so a rejected version is not
	// immediately handed back and reapplied.
	in, _, err := f.u.InCooldown(time.Now())
	noErr(t, err, "InCooldown")
	isTrue(t, in, "a rollback holds the cooldown")
}

// Attest with no update in flight must do nothing at all - it is called on every
// ordinary start.
func TestAttest_NoUpdateInFlightDoesNothing(t *testing.T) {
	u := testUpdater(t)
	u.Attest(context.Background(), time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	isFalse(t, exists(u.paths.State), "no journal is created")
	rec, err := u.records.load()
	noErr(t, err, "load the record")
	if rec != nil {
		t.Fatal("nothing may be recorded for a start with no update in flight")
	}
}

// The deadline is the whole point: a new version that never says it is healthy
// must not be left running forever.
func TestAttest_DeadlineRollsBack(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)

	f.u.Attest(context.Background(), 20*time.Millisecond)

	select {
	case <-f.execCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the attestation deadline must roll back")
	}
	eq(t, readFileString(t, f.bin), "old-binary", "the old binary is restored")

	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	hasSubstr(t, j.ErrorMessage, "did not attest healthy within", "the cause names the window")
}

// Committing before the deadline must disarm it, or a timer firing afterwards
// would roll back an update that has already been accepted.
func TestAttest_CommitDisarmsTheDeadline(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)

	f.u.Attest(context.Background(), 30*time.Millisecond)
	noErr(t, f.u.Commit(), "Commit")

	time.Sleep(120 * time.Millisecond)

	select {
	case <-f.execCh:
		t.Fatal("a committed update must never be rolled back by a stale timer")
	default:
	}
	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "phase")
	eq(t, readFileString(t, f.bin), "new-binary", "the new binary stays")
}

// A caller with its own health policy needs to know whether to run it at all.
// Health checks cost something, and running one on every ordinary start just in
// case would be both wasteful and a surprise to whoever wrote it.
func TestPendingAttestation(t *testing.T) {
	for _, phase := range []string{phaseSwapped, phaseAttesting} {
		t.Run(phase, func(t *testing.T) {
			f := newAttestFixture(t, phase)

			got, ok := f.u.PendingAttestation()
			isTrue(t, ok, "an installed but unjudged update is pending")
			eq(t, got.ID, "cmd-1", "id")
			eq(t, got.FromVersion, "1.4.0", "from version")
			eq(t, got.ToVersion, "1.5.0", "to version")

			// A query changes nothing: asking twice gives the same answer, and
			// the journal is untouched.
			_, again := f.u.PendingAttestation()
			isTrue(t, again, "the query must not consume the pending update")
			j, err := readState(f.u.paths.State)
			noErr(t, err, "readState")
			eq(t, j.Phase, phase, "phase untouched")
		})
	}
}

func TestPendingAttestation_NothingInFlight(t *testing.T) {
	u := testUpdater(t)
	_, ok := u.PendingAttestation()
	isFalse(t, ok, "no journal means nothing to attest")
}

func TestPendingAttestation_AlreadyDecided(t *testing.T) {
	for _, phase := range []string{phaseCommitted, phaseRolledBack} {
		f := newAttestFixture(t, phase)
		_, ok := f.u.PendingAttestation()
		isFalse(t, ok, "a decided update is not awaiting a verdict")
	}
}

// The OLD image must never be told an update is awaiting its verdict. Repair has
// already dealt with it, and attesting from here would confirm an update that
// never took effect.
func TestPendingAttestation_RefusedFromTheOldImage(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)
	f.u.cfg.Version = "1.4.0" // the OLD version, not the journal's target

	_, ok := f.u.PendingAttestation()
	isFalse(t, ok, "the old image has nothing to attest")
}

// Commit and the deadline's Rollback must not both take effect. Disarming the
// timer narrows that window but cannot close it: the timer goroutine may already
// be past its select and inside Rollback when Commit cancels the context.
//
// The contended state is the filesystem, so the race detector is blind to this
// and no interleaving can be forced deterministically. What the test can do is
// collide the two repeatedly and assert the result is always SELF-CONSISTENT -
// an update decided one way, with the binary, the rollback copy and the record
// all agreeing. Unserialised, the losing outcome is a journal saying "committed"
// over a binary that was restored, which is exactly what is checked here.
func TestAttest_CommitRacingTheDeadlineDecidesOnlyOnce(t *testing.T) {
	for i := range 40 {
		t.Run(fmt.Sprintf("collision-%d", i), func(t *testing.T) {
			f := newAttestFixture(t, phaseAttesting)

			start := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-start
				_ = f.u.Commit()
			}()

			// A deadline of zero fires immediately, so both verdicts are in
			// flight at once.
			f.u.Attest(context.Background(), 0)
			close(start)
			<-done

			// Let a rollback that lost the lock finish attempting whatever it
			// was going to attempt.
			time.Sleep(20 * time.Millisecond)

			j, err := readState(f.u.paths.State)
			noErr(t, err, "readState")
			rec, err := f.u.PendingOutcome()
			noErr(t, err, "PendingOutcome")
			if rec == nil {
				t.Fatal("a decided update must always leave a record")
			}

			switch j.Phase {
			case phaseCommitted:
				eq(t, readFileString(t, f.bin), "new-binary",
					"a committed update must not have had its binary restored")
				isFalse(t, exists(f.u.paths.RollbackBinary),
					"a commit releases the rollback copy")
				eq(t, rec.Status, StatusSucceeded, "record status")
			case phaseRolledBack:
				eq(t, readFileString(t, f.bin), "old-binary",
					"a rolled-back update must have had its binary restored")
				eq(t, rec.Status, StatusRolledBack, "record status")
			default:
				t.Fatalf("the update was left undecided at phase %q", j.Phase)
			}
		})
	}
}

// Cancelling the context disarms the window WITHOUT deciding. A program shutting
// down mid-window has not failed, and rolling it back on the way out would
// replace a working binary during an operation nobody is watching.
func TestAttest_ContextCancellationLeavesTheUpdateUndecided(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)

	ctx, cancel := context.WithCancel(context.Background())
	f.u.Attest(ctx, 30*time.Millisecond)
	cancel()

	time.Sleep(120 * time.Millisecond)

	select {
	case <-f.execCh:
		t.Fatal("a shutdown during attestation must not trigger a rollback")
	default:
	}
	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseAttesting, "the journal is left for the next start to resume")
	eq(t, readFileString(t, f.bin), "new-binary", "nothing was restored")
}

// A journal at phaseSwapped is equally in need of a verdict: the swap happened
// but the process never got as far as recording that it was attesting.
func TestAttest_SwappedPhaseIsAlsoAttestable(t *testing.T) {
	f := newAttestFixture(t, phaseSwapped)

	noErr(t, f.u.Commit(), "Commit")

	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "phase")
}

// CleanupReported is the last step of the report cycle, and it must only fire
// once an outcome has actually been decided.
func TestCleanupReported(t *testing.T) {
	t.Run("removes a decided journal", func(t *testing.T) {
		f := newAttestFixture(t, phaseAttesting)
		noErr(t, f.u.Commit(), "Commit")
		writeBinary(t, f.u.paths.HelperLog, "log")

		f.u.CleanupReported()

		isFalse(t, exists(f.u.paths.State), "the journal is removed")
		isFalse(t, exists(f.u.paths.HelperLog), "leftovers are removed")
		isTrue(t, exists(f.bin), "the binary is never touched")
	})

	t.Run("leaves an undecided journal alone", func(t *testing.T) {
		f := newAttestFixture(t, phaseAttesting)
		f.u.CleanupReported()
		isTrue(t, exists(f.u.paths.State), "an undecided update must not be cleaned up")
	})

	t.Run("keeps the outcome record", func(t *testing.T) {
		f := newAttestFixture(t, phaseAttesting)
		noErr(t, f.u.Commit(), "Commit")
		f.u.CleanupReported()

		rec, err := f.u.records.load()
		noErr(t, err, "load the record")
		if rec == nil {
			t.Fatal("the record outlives the journal - the cooldown reads it")
		}
	})
}

// The full report cycle a caller is expected to run on startup.
func TestReportCycle(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)
	noErr(t, f.u.Commit(), "Commit")

	pending, err := f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if pending == nil {
		t.Fatal("a committed update is owed to the caller")
	}
	eq(t, pending.Status, StatusSucceeded, "status")

	noErr(t, f.u.MarkReported(), "MarkReported")
	f.u.CleanupReported()

	pending, err = f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome after reporting")
	if pending != nil {
		t.Fatal("a reported outcome must not be sent twice")
	}
}
