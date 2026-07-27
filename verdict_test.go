package denju

import (
	"context"
	"errors"
	"os"
	"testing"
)

// These cover the three ways denju could tell a caller something the filesystem
// had not actually done. Each needs a specific write or rename to fail at a
// specific moment, which is why none of them is reachable from an end-to-end
// harness - the only lever is inside the package.

// withWriteStateFailing makes the journal write fail for the states matching
// when, and behave normally for the rest.
func withWriteStateFailing(t *testing.T, when func(*state) bool) {
	t.Helper()
	prev := writeState
	writeState = func(n names, path string, s *state) error {
		if s != nil && when(s) {
			return errors.New("no space left on device")
		}
		return prev(n, path, s)
	}
	t.Cleanup(func() { writeState = prev })
}

// ------------------------------------------------------------------ R1

// The worst case the update path has: the exec failed, so the program is still
// the old image, and the restore that would undo the swap failed too. The new
// binary is on disk, the drain has run and BeforeHandoff has already fired.
//
// Nothing about that is survivable, and Status cannot say so - a refused
// download is StatusFailed as well. ProgramIntact is what separates them.
func TestUpdate_ExecFailureWithFailedRestoreIsNotIntact(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)

	// Destroy the rollback copy from inside the failing exec, which is the only
	// point at which it exists and the restore has not yet been attempted.
	prev := execSelf
	execSelf = func(string, []string, []string) error {
		_ = os.Remove(r.u.paths.RollbackBinary)
		return errors.New("exec format error")
	}
	t.Cleanup(func() { execSelf = prev })

	got := r.u.Update(context.Background(), r.req(), r.src)

	isFalse(t, got.ProgramIntact, "a program that cannot go forwards or back is not intact")
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "could not be restored", "error")

	// The journal is the evidence. An ordinary refusal deletes it because there
	// is nothing to reconcile; here it is the only thing that tells the next
	// start which of the two binaries it is looking at.
	j, err := readState(r.u.paths.State)
	noErr(t, err, "the journal must survive so the next start can reconcile it")
	eq(t, j.Phase, phaseAttesting, "phase")
	eq(t, j.CommandID, "cmd-1", "command id")
	eq(t, r.binaryContent(t), string(r.payload), "the new binary really is the one on disk")
}

// The restore worked, so the binary is sound - but this process could not
// restart into it and is still the drained, handed-over image it became. A
// caller that carries on here is running a program whose shutdown has already
// happened.
func TestUpdate_ExecFailureWithFailedRestartIsNotIntact(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)

	prev := execSelf
	execSelf = func(string, []string, []string) error {
		return errors.New("exec format error")
	}
	t.Cleanup(func() { execSelf = prev })

	got := r.u.Update(context.Background(), r.req(), r.src)

	isFalse(t, got.ProgramIntact, "a process that could not restart into the restored binary is not intact")
	eq(t, got.Status, StatusRolledBack, "status")
	eq(t, r.binaryContent(t), "current", "the old binary is back in place")
}

// Everything that refuses BEFORE the point of no return leaves the program
// exactly as it was, and must say so - otherwise a caller wired to fail fast
// terminates on a corrupt download.
func TestUpdate_RefusalsLeaveTheProgramIntact(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) (*updateRig, Request)
	}{
		{"incomplete request", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t)
			return r, Request{ID: "cmd-1"}
		}},
		{"foreign platform", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t)
			req := r.req()
			req.TargetOS = "plan9"
			return r, req
		}},
		{"same version", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t)
			req := r.req()
			req.TargetVersion = "1.4.0"
			return r, req
		}},
		{"download failure", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t)
			r.src.err = errors.New("the connection dropped")
			return r, r.req()
		}},
		{"checksum mismatch", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t)
			req := r.req()
			req.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			return r, req
		}},
		{"selftest rejection", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t)
			r.u.runSelftest = func(string, []string, string) error {
				return errors.New("the new binary would not start")
			}
			return r, r.req()
		}},
		{"drain refusal", func(t *testing.T) (*updateRig, Request) {
			r := newUpdateRig(t, func(c *Config) {
				c.Drain = func(context.Context) error { return errors.New("still serving") }
			})
			return r, r.req()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withGOOS(t, "linux")
			r, req := tc.build(t)

			got := r.u.Update(context.Background(), req, r.src)

			isTrue(t, got.ProgramIntact, "a refusal must leave the program able to carry on")
			eq(t, got.Status, StatusFailed, "status")
			eq(t, r.binaryContent(t), "current", "the binary is untouched")
		})
	}
}

// ------------------------------------------------------------------ R2

// A commit that has been RECORDED has happened. Failing the whole verdict
// because the journal could not be updated used to leave the journal at
// attesting with no record, so every later start counted a crash against a
// version that had attested healthy - and the tolerance eventually rolled back
// a good binary over a transient write error.
func TestCommit_SurvivesAJournalWriteFailure(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)
	withWriteStateFailing(t, func(s *state) bool { return s.Phase == phaseCommitted })

	noErr(t, f.u.Commit(), "a commit whose journal write fails is still a commit")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("the commit must be recorded; the record is the verdict")
	}
	eq(t, rec.Status, StatusSucceeded, "status")
	isFalse(t, exists(f.u.paths.RollbackBinary), "the rollback copy is still released")

	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseAttesting, "the journal is left behind, which is what repair reconciles")
}

// The other half: a start that finds the journal still at attesting must consult
// the record before counting a crash. Without this the crash counter climbs on
// every start and rolls back a version that was committed.
func TestRepair_DoesNotRollBackACommittedUpdate(t *testing.T) {
	withGOOS(t, "linux")
	f := newRepairFixture(t, "new-binary", "1.5.0")
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	execed := withStubbedExec(t)

	stageJournal(t, f.u, &state{
		Phase:         phaseAttesting,
		CommandID:     "cmd-1",
		OldVersion:    "1.4.0",
		TargetVersion: "1.5.0",
		CrashCount:    defaultCrashTolerance + 5, // well past the point of rolling back
	})
	noErr(t, f.u.records.record(&Outcome{
		ID: "cmd-1", FromVersion: "1.4.0", ToVersion: "1.5.0", Status: StatusSucceeded,
	}), "record the commit")

	noErr(t, f.u.Repair(), "Repair")

	isFalse(t, *execed, "a committed update must not be rolled back, whatever the crash count says")
	eq(t, readFileString(t, f.bin), "new-binary", "the committed version stays")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "the journal is reconciled from the record")
	isFalse(t, exists(f.p.RollbackBinary), "and the rollback copy is finally released")
}

// ------------------------------------------------------------------ R9

// A rollback is recorded before it is attempted, because a successful one
// restarts the process and never returns to record anything. When the restore
// then fails, the record has to be corrected - otherwise the server is told the
// machine went back to the old version while it is still running the new one.
func TestRepair_FailedRollbackRecordsFailedNotRolledBack(t *testing.T) {
	withGOOS(t, "linux")
	f := newRepairFixture(t, "new-binary", "1.5.0")
	// No rollback copy on disk, so the restore rename has nothing to move.
	execed := withStubbedExec(t)

	stageJournal(t, f.u, &state{
		Phase:         phaseAttesting,
		CommandID:     "cmd-1",
		OldVersion:    "1.4.0",
		TargetVersion: "1.5.0",
		CrashCount:    defaultCrashTolerance,
	})

	err := wantErr(t, f.u.Repair(), "a rollback that cannot restore must fail loudly")
	hasSubstr(t, err.Error(), "restore the old binary", "error")

	isFalse(t, *execed, "nothing may be restarted when the restore failed")
	eq(t, readFileString(t, f.bin), "new-binary", "the new binary is still what is running")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("the attempt must still be recorded")
	}
	eq(t, rec.Status, StatusFailed, "a rollback that did not happen must not be reported as one")
	hasSubstr(t, rec.Error, "rollback failed", "recorded cause")
}

// The same correction on the attestation path, which records its verdict before
// acting for the same reason.
func TestRollback_FailedRestoreRecordsFailedNotRolledBack(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)
	noErr(t, os.Remove(f.u.paths.RollbackBinary), "remove the rollback copy")

	err := wantErr(t, f.u.Rollback("the health probe never passed"), "Rollback")
	hasSubstr(t, err.Error(), "restore the old binary", "error")

	eq(t, readFileString(t, f.bin), "new-binary", "the new binary is still what is running")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("the attempt must still be recorded")
	}
	eq(t, rec.Status, StatusFailed, "a rollback that did not happen must not be reported as one")
	hasSubstr(t, rec.Error, "rollback failed", "recorded cause")
	hasSubstr(t, rec.Error, "restore the old binary", "the recorded cause names what actually failed")
}
