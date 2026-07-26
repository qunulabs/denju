package denju

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// repairFixture is a fake install dir with a binary and its journal paths.
//
// The outcome record lives in its OWN directory, mirroring production: it is
// usually a state dir rather than the install dir, because it has to survive the
// binary being replaced.
type repairFixture struct {
	u   *Updater
	dir string
	bin string
	p   paths
}

const testCooldown = 30 * time.Minute

func newRepairFixture(t *testing.T, binContent, selfVersion string) repairFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, binContent)

	u, err := New(Config{
		Namespace:  "app",
		Version:    selfVersion,
		BinaryPath: bin,
		RecordPath: filepath.Join(t.TempDir(), "last-update.json"),
		Cooldown:   testCooldown,
	})
	noErr(t, err, "New")
	return repairFixture{u: u, dir: dir, bin: bin, p: u.paths}
}

// withStubbedExec replaces the image swap so a rollback restart can be observed
// instead of ending the test binary. It reports whether the exec was reached.
func withStubbedExec(t *testing.T) *bool {
	t.Helper()
	var execed bool
	prev := execSelf
	execSelf = func(string, []string, []string) error { execed = true; return nil }
	t.Cleanup(func() { execSelf = prev })
	return &execed
}

// With no journal, repair reclaims every leftover artifact so a later update
// starts from a clean directory.
func TestRepair_NoJournalReclaimsLeftovers(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	writeBinary(t, f.p.Discard, "a rolled-back binary")
	writeBinary(t, f.p.HelperCopy, "helper")
	writeBinary(t, f.p.HelperLog, "log")
	writeBinary(t, f.p.Restart, "restart journal")

	noErr(t, f.u.Repair(), "Repair")

	isFalse(t, exists(f.p.Discard), "discard reclaimed")
	isFalse(t, exists(f.p.HelperCopy), "helper copy reclaimed")
	isFalse(t, exists(f.p.HelperLog), "helper log reclaimed")
	isFalse(t, exists(f.p.Restart), "restart journal reclaimed")
	isTrue(t, exists(f.bin), "the binary itself is never touched")
}

// A staged handoff whose helper is still running is left alone: this start may
// be a fast manual restart racing the helper, and two processes doing the swap
// is far worse than waiting.
func TestRepair_StagedWithLiveHelperIsLeftAlone(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	staged := filepath.Join(f.dir, ".app-update-staged")
	writeBinary(t, staged, "new")
	stageJournal(t, f.u, &state{Phase: phaseStaged, HelperPID: 999, NewBinaryPath: staged})
	withPIDAlive(t, func(int) bool { return true })

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseStaged, "phase")
	isTrue(t, exists(staged), "the staged download is left for the helper")
}

// A staged handoff whose helper is gone is over: journal the rollback so the
// outcome gets reported, and reclaim the staged download.
func TestRepair_StagedWithDeadHelperRollsBack(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	staged := filepath.Join(f.dir, ".app-update-staged")
	writeBinary(t, staged, "new")
	stageJournal(t, f.u, &state{Phase: phaseStaged, HelperPID: 999, NewBinaryPath: staged})
	withPIDAlive(t, noProcessAlive)

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	hasSubstr(t, j.ErrorMessage, "interrupted", "journal error message")
	isFalse(t, exists(staged), "the staged download must be reclaimed")
}

// The new image starting up increments the crash counter and carries on, so it
// gets its startup and attestation window.
func TestRepair_NewImageBelowCrashLimitProceeds(t *testing.T) {
	f := newRepairFixture(t, "new-binary", "1.4.0")
	stageJournal(t, f.u, &state{Phase: phaseSwapped, OldVersion: "1.3.0", TargetVersion: "1.4.0"})

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.CrashCount, 1, "crash count")
	eq(t, j.PID, os.Getpid(), "pid refreshed to this process")
	eq(t, j.Phase, phaseSwapped, "phase")
}

// A new image that keeps reaching startup repair uncommitted is crash-looping;
// past the tolerance it is rolled back and restarted as the old binary.
func TestRepair_NewImageCrashLoopRollsBackAndRestarts(t *testing.T) {
	withGOOS(t, "linux")
	f := newRepairFixture(t, "new-binary", "1.4.0")
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	stageJournal(t, f.u, &state{
		Phase:         phaseAttesting,
		OldVersion:    "1.3.0",
		TargetVersion: "1.4.0",
		CrashCount:    defaultCrashTolerance,
	})
	execed := withStubbedExec(t)

	noErr(t, f.u.Repair(), "Repair")
	isTrue(t, *execed, "a crash-looping new image must restart as the old one")
	eq(t, readFileString(t, f.bin), "old-binary", "the restored binary")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	hasSubstr(t, j.ErrorMessage, "crashed on startup", "journal error message")
}

// Starting as the OLD image with an uncommitted journal means the restore
// happened but the restart into it did not. Mark it rolled back so the outcome
// is reported, and let the old image carry on.
func TestRepair_OldImageMarksRolledBack(t *testing.T) {
	f := newRepairFixture(t, "old-binary", "1.3.0")
	stageJournal(t, f.u, &state{Phase: phaseSwapped, OldVersion: "1.3.0", TargetVersion: "1.4.0"})

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
}

// A cause already in the journal is the specific reason the update failed, and
// it is what an operator will be shown. Repair must not overwrite it with its
// own generic wording.
func TestRepair_PreservesExistingRollbackCause(t *testing.T) {
	f := newRepairFixture(t, "old-binary", "1.3.0")
	stageJournal(t, f.u, &state{
		Phase: phaseSwapped, CommandID: "cmd-3",
		OldVersion: "1.3.0", TargetVersion: "1.4.0",
		ErrorMessage: "the new version could not open its database",
	})

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	eq(t, j.ErrorMessage, "the new version could not open its database",
		"the specific cause must survive")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("expected an outcome")
	}
	eq(t, rec.Error, "the new version could not open its database", "the reported cause")
}

// A stale journal whose new_sha256 does not match the binary on disk describes
// an update that never landed - or a binary replaced by something else since.
// Either way the rollback copy must NOT be moved over it: restoring would
// silently undo whatever is actually installed.
func TestRepair_StaleCleansUpWhenTheBinaryIsUnrelated(t *testing.T) {
	withPIDAlive(t, noProcessAlive)
	f := newRepairFixture(t, "some-other-binary", "1.0.0")
	f.u.cfg.StaleThreshold = time.Nanosecond
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	stageJournal(t, f.u, &state{Phase: "bogus", NewSHA256: "0000000000000000"})
	backdate(t, f.p.State, time.Hour)

	noErr(t, f.u.Repair(), "Repair")

	eq(t, readFileString(t, f.bin), "some-other-binary",
		"an unrelated binary must not be replaced by the rollback copy")
	isFalse(t, exists(f.p.State), "the stale journal is still collected")
	isFalse(t, exists(f.p.RollbackBinary), "and so is the rollback copy")
}

// The crash-loop rollback on Windows cannot exec, so it hands off to a detached
// relaunch helper and exits instead.
func TestRepair_NewImageCrashLoopWindowsRelaunch(t *testing.T) {
	withGOOS(t, "windows")
	f := newRepairFixture(t, "new-binary", "1.4.0")
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	stageJournal(t, f.u, &state{
		Phase:         phaseAttesting,
		CommandID:     "cmd-6",
		OldVersion:    "1.3.0",
		TargetVersion: "1.4.0",
		CrashCount:    defaultCrashTolerance,
	})

	var spawnEnv []string
	prevSpawn := spawnDetached
	spawnDetached = func(_ string, _ []string, _ string, env []string) (int, error) {
		spawnEnv = env
		return 4242, nil
	}
	exitCode := -1
	prevExit := osExit
	osExit = func(code int) { exitCode = code }
	t.Cleanup(func() { spawnDetached = prevSpawn; osExit = prevExit })

	noErr(t, f.u.Repair(), "Repair")

	eq(t, readFileString(t, f.bin), "old-binary", "the old binary is restored")
	eq(t, readFileString(t, f.p.Discard), "new-binary",
		"the running new binary is renamed aside, since Windows cannot delete it")
	isTrue(t, envHasKV(spawnEnv, f.u.n.envMode+"="+modeRelaunch), "the helper runs in relaunch mode")
	isTrue(t, envHasKV(spawnEnv, f.u.n.envState+"="+f.p.State),
		"the helper reads the update journal, so the outcome survives to be reported")
	eq(t, exitCode, 0, "the process exits so the helper's wait can complete")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
	hasSubstr(t, j.ErrorMessage, "crashed on startup", "cause")
}

// When both builds carry the SAME version string, the version cannot say which
// image is running - the binary's own SHA-256 has to.
func TestRepair_SameVersionTiebreaksOnSHA256(t *testing.T) {
	f := newRepairFixture(t, "new-binary", "dev")
	newSum, err := sha256File(f.bin)
	noErr(t, err, "hash the binary")
	stageJournal(t, f.u, &state{
		Phase: phaseSwapped, OldVersion: "dev", TargetVersion: "dev", NewSHA256: newSum,
	})

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.CrashCount, 1, "the running binary hashes to new_sha256, so this IS the new image")
	eq(t, j.Phase, phaseSwapped, "phase")
}

// A decided outcome is left for the reporter, which delivers it and only then
// cleans up. Repair must not delete it first.
func TestRepair_DecidedOutcomeIsPreserved(t *testing.T) {
	for _, phase := range []string{phaseCommitted, phaseRolledBack} {
		f := newRepairFixture(t, "program", "1.0.0")
		stageJournal(t, f.u, &state{Phase: phase, CommandID: "cmd-1"})

		noErr(t, f.u.Repair(), "Repair")

		j, err := readState(f.p.State)
		noErr(t, err, "readState")
		eq(t, j.Phase, phase, "phase preserved")
	}
}

// A decided journal with no matching record is a dead end: PendingOutcome finds
// nothing, so the outcome is never reported and the journal is never cleaned up.
// It arises when the record has been removed or relocated, and for any program
// whose PREVIOUS version updated itself by some other means and so kept no
// record at all. Repair mints the missing record rather than leaving it lost.
func TestRepair_DecidedJournalWithoutARecordIsRecorded(t *testing.T) {
	cases := []struct {
		phase string
		cause string
		want  Status
	}{
		{phaseCommitted, "", StatusSucceeded},
		{phaseRolledBack, "the new version was no good", StatusRolledBack},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			f := newRepairFixture(t, "program", "1.0.0")
			stageJournal(t, f.u, &state{
				Phase: tc.phase, CommandID: "cmd-1",
				OldVersion: "1.0.0", TargetVersion: "1.1.0",
				ErrorMessage: tc.cause,
			})

			noErr(t, f.u.Repair(), "Repair")

			got, err := f.u.PendingOutcome()
			noErr(t, err, "PendingOutcome")
			if got == nil {
				t.Fatal("a decided update must leave a reportable outcome")
			}
			eq(t, got.Status, tc.want, "status")
			eq(t, got.ID, "cmd-1", "id")
			eq(t, got.ToVersion, "1.1.0", "to version")
			eq(t, got.Error, tc.cause, "cause")

			j, err := readState(f.p.State)
			noErr(t, err, "readState")
			eq(t, j.Phase, tc.phase, "the journal is left for the reporter")
		})
	}
}

// Repair runs on EVERY start, so the common case - a journal this same program
// decided, whose record was written at the same moment - must not be rewritten.
// Rewriting would push the cooldown anchor forward on every restart, turning one
// finished update into an indefinite refusal of the next.
func TestRepair_DecidedJournalDoesNotRewriteAnExistingRecord(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	stageJournal(t, f.u, &state{
		Phase: phaseCommitted, CommandID: "cmd-1",
		OldVersion: "1.0.0", TargetVersion: "1.1.0",
	})

	noErr(t, f.u.Repair(), "the first Repair")
	first, err := f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if first == nil {
		t.Fatal("the first Repair must record the outcome")
	}

	for range 3 {
		noErr(t, f.u.Repair(), "a later Repair")
	}

	again, err := f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	eq(t, again.At, first.At, "the outcome must not be rewritten")
	eq(t, again.CooldownAt, first.CooldownAt, "the cooldown anchor must not move")
}

// An outcome already delivered must stay delivered. Repair sees the journal on
// every start until the reporter cleans it up, and resurrecting a reported
// outcome would report the same update again on each one.
func TestRepair_DecidedJournalDoesNotResurrectAReportedOutcome(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	stageJournal(t, f.u, &state{
		Phase: phaseCommitted, CommandID: "cmd-1",
		OldVersion: "1.0.0", TargetVersion: "1.1.0",
	})
	noErr(t, f.u.Repair(), "Repair")
	noErr(t, f.u.MarkReported(), "MarkReported")

	noErr(t, f.u.Repair(), "the next start's Repair")

	got, err := f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if got != nil {
		t.Fatalf("a reported outcome must not become pending again (got %+v)", got)
	}
}

// An old journal in an unrecognized phase whose processes are gone is a crashed,
// orphaned update: restore the old binary and garbage-collect.
func TestRepair_StaleJournalRestoresAndCleansUp(t *testing.T) {
	withPIDAlive(t, noProcessAlive)
	f := newRepairFixture(t, "new-binary", "1.0.0")
	f.u.cfg.StaleThreshold = time.Nanosecond
	newSum, err := sha256File(f.bin)
	noErr(t, err, "hash the binary")
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	stageJournal(t, f.u, &state{Phase: "bogus", NewSHA256: newSum})
	backdate(t, f.p.State, time.Hour)

	noErr(t, f.u.Repair(), "Repair")

	eq(t, readFileString(t, f.bin), "old-binary", "the restored binary")
	isFalse(t, exists(f.p.State), "the journal is collected")
	isFalse(t, exists(f.p.RollbackBinary), "the rollback copy is collected")
}

// A journal in an unrecognized phase that is still FRESH is left alone: another
// process may be mid-update.
func TestRepair_FreshUnknownPhaseIsLeftAlone(t *testing.T) {
	withPIDAlive(t, noProcessAlive)
	f := newRepairFixture(t, "new-binary", "1.0.0")
	stageJournal(t, f.u, &state{Phase: "bogus"})

	noErr(t, f.u.Repair(), "Repair")
	isTrue(t, exists(f.p.State), "a fresh journal survives")
}

// A fresh unparseable journal is a real, unexpected problem: surface it rather
// than silently discarding a record that may describe a swapped binary.
func TestRepair_FreshCorruptJournalIsAnError(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	noErr(t, os.WriteFile(f.p.State, []byte("{not json"), 0o600), "write a corrupt journal")

	wantErr(t, f.u.Repair(), "Repair on a fresh corrupt journal")
}

// An OLD unparseable journal is garbage from a crash long past; collect it.
func TestRepair_StaleCorruptJournalIsCollected(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	f.u.cfg.StaleThreshold = time.Nanosecond
	noErr(t, os.WriteFile(f.p.State, []byte("{not json"), 0o600), "write a corrupt journal")
	backdate(t, f.p.State, time.Hour)

	noErr(t, f.u.Repair(), "Repair")
	isFalse(t, exists(f.p.State), "a stale corrupt journal is collected")
}

// The crash-loop rollback must RECORD itself, and this is the test that matters
// most in this file.
//
// Rolling back is only half the job. The record is the cooldown's only anchor
// and the outcome report's only input, and the process that started the update
// is gone, so nothing else can write it. Observed live without it: the restored
// old version came up, found no record, accepted the same broken binary again,
// and looped - five rollbacks and six swaps in forty seconds, each one a full
// binary download, with the control plane never told a thing.
func TestRepair_CrashLoopRollbackRecordsTheOutcome(t *testing.T) {
	withGOOS(t, "linux")
	f := newRepairFixture(t, "new-binary", "1.4.0")
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	stageJournal(t, f.u, &state{
		Phase:         phaseAttesting,
		CommandID:     "cmd-7",
		OldVersion:    "1.3.0",
		TargetVersion: "1.4.0",
		CrashCount:    defaultCrashTolerance,
	})
	withStubbedExec(t)

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("a crash-loop rollback with no record is an infinite update loop")
	}
	eq(t, rec.Status, StatusRolledBack, "status")
	eq(t, rec.ID, "cmd-7", "id")
	eq(t, rec.FromVersion, "1.3.0", "from version")
	eq(t, rec.ToVersion, "1.4.0", "to version")
	hasSubstr(t, rec.Error, "crashed on startup", "recorded error")

	// The cooldown must actually hold, which is what stops the next command from
	// reapplying the same binary.
	in, left, err := f.u.InCooldown(time.Now())
	noErr(t, err, "InCooldown")
	isTrue(t, in, "the rollback must hold the cooldown")
	isTrue(t, math.Abs(left.Seconds()-testCooldown.Seconds()) < 5, "the full cooldown window remains")

	// And it must be owed to the caller, so an operator learns why this host is
	// still on the old version.
	pending, err := f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if pending == nil {
		t.Fatal("the rollback must be reportable")
	}
	eq(t, pending.ID, "cmd-7", "pending id")
}

// Repair runs on EVERY start, so a decided outcome must not be re-recorded: each
// rewrite would push the cooldown anchor forward and turn one failed update into
// an indefinite refusal.
func TestRepair_DoesNotRewriteAnAlreadyRecordedOutcome(t *testing.T) {
	f := newRepairFixture(t, "old-binary", "1.3.0")
	anchor := time.Now().Add(-20 * time.Minute)
	noErr(t, f.u.records.record(&Outcome{
		At: anchor, ID: "cmd-8", FromVersion: "1.3.0", ToVersion: "1.4.0",
		Status: StatusRolledBack, Error: "update interrupted before completion",
	}), "seed the record")
	stageJournal(t, f.u, &state{
		Phase: phaseSwapped, CommandID: "cmd-8", OldVersion: "1.3.0", TargetVersion: "1.4.0",
	})

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	drift := rec.CooldownAt.Sub(anchor)
	isTrue(t, drift > -time.Second && drift < time.Second,
		"re-recording the same outcome would extend the cooldown on every restart")
}

// Starting as the old image under an uncommitted journal means the swap was
// undone; that is a completed attempt, so it is recorded like one.
func TestRepair_OldImageRecordsTheRollback(t *testing.T) {
	f := newRepairFixture(t, "old-binary", "1.3.0")
	stageJournal(t, f.u, &state{
		Phase: phaseSwapped, CommandID: "cmd-9", OldVersion: "1.3.0", TargetVersion: "1.4.0",
	})

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("an undone swap must be recorded")
	}
	eq(t, rec.Status, StatusRolledBack, "status")
	hasSubstr(t, rec.Error, "interrupted", "recorded error")
}

// A dead Windows helper never reached the swap, so the binary was never
// replaced: the honest outcome is failed, not rolled back. It holds the cooldown
// either way.
func TestRepair_DeadHelperRecordsAFailure(t *testing.T) {
	f := newRepairFixture(t, "program", "1.3.0")
	staged := filepath.Join(f.dir, ".app-update-staged")
	writeBinary(t, staged, "new")
	stageJournal(t, f.u, &state{
		Phase: phaseStaged, CommandID: "cmd-10", HelperPID: 999,
		OldVersion: "1.3.0", TargetVersion: "1.4.0", NewBinaryPath: staged,
	})
	withPIDAlive(t, noProcessAlive)

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec == nil {
		t.Fatal("a dead handoff must be recorded")
	}
	eq(t, rec.Status, StatusFailed, "status")

	in, _, err := f.u.InCooldown(time.Now())
	noErr(t, err, "InCooldown")
	isTrue(t, in, "a handoff that cannot start must not be retried in a tight loop")
}

// A live helper is still in charge, so repair concluded nothing and must record
// nothing - a record here would hold the cooldown against an update that is
// still going to succeed.
func TestRepair_LiveHelperRecordsNothing(t *testing.T) {
	f := newRepairFixture(t, "program", "1.3.0")
	stageJournal(t, f.u, &state{Phase: phaseStaged, CommandID: "cmd-11", HelperPID: 999})
	withPIDAlive(t, func(int) bool { return true })

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec != nil {
		t.Fatalf("expected no record, got %+v", rec)
	}
}

// The new image below the crash limit is still in play: no outcome yet, so no
// record and no cooldown.
func TestRepair_NewImageBelowCrashLimitRecordsNothing(t *testing.T) {
	f := newRepairFixture(t, "new-binary", "1.4.0")
	stageJournal(t, f.u, &state{
		Phase: phaseSwapped, CommandID: "cmd-12", OldVersion: "1.3.0", TargetVersion: "1.4.0",
	})

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec != nil {
		t.Fatal("the new version has not failed yet; recording now would pre-empt attestation")
	}
}

// An update that carried no request id has nothing to report against, and
// holding the cooldown for it would hold it for an id nobody can act on.
func TestRepair_NoRequestIDRecordsNothing(t *testing.T) {
	f := newRepairFixture(t, "old-binary", "1.3.0")
	stageJournal(t, f.u, &state{
		Phase: phaseSwapped, OldVersion: "1.3.0", TargetVersion: "1.4.0",
	})

	noErr(t, f.u.Repair(), "Repair")

	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	if rec != nil {
		t.Fatalf("expected no record, got %+v", rec)
	}
}
