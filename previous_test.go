package denju

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retaining(c *Config) { c.RetainPrevious = true }

// seedPrevious puts a retained binary and its record on disk, as an earlier
// commit would have.
func seedPrevious(t *testing.T, u *Updater, content, version string) {
	t.Helper()
	writeBinary(t, u.paths.Previous, content)
	noErr(t, u.writePreviousRecord(version, sha256Hex([]byte(content))), "seed the retained record")
}

func TestCommit_RetainPreviousKeepsTheReplacedBinary(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting, retaining)

	noErr(t, f.u.Commit(), "Commit")

	isFalse(t, exists(f.u.paths.RollbackBinary), "the rollback copy is moved, not left behind")
	eq(t, readFileString(t, f.u.paths.Previous), "old-binary", "the replaced binary is retained")

	path, sum, version, ok := f.u.PreviousBinary()
	isTrue(t, ok, "PreviousBinary reports it")
	eq(t, path, f.u.paths.Previous, "path")
	eq(t, sum, sha256Hex([]byte("old-binary")), "sha256")
	eq(t, version, "1.4.0", "the version the update moved AWAY from")
}

// Without the option nothing changes: the rollback copy is deleted as before.
func TestCommit_WithoutRetainPreviousNothingIsKept(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting)

	noErr(t, f.u.Commit(), "Commit")

	isFalse(t, exists(f.u.paths.RollbackBinary), "the rollback copy is deleted")
	isFalse(t, exists(f.u.paths.Previous), "nothing is retained")
	isFalse(t, exists(f.u.paths.PreviousRecord), "no record is written")
	_, _, _, ok := f.u.PreviousBinary()
	isFalse(t, ok, "PreviousBinary reports nothing")
}

// One generation (piece C §10): the next commit replaces the retained binary.
func TestCommit_RetainPreviousKeepsOneGeneration(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting, retaining)
	seedPrevious(t, f.u, "gen-0", "0.9.0")

	noErr(t, f.u.Commit(), "Commit")

	eq(t, readFileString(t, f.u.paths.Previous), "old-binary", "the older generation is replaced")
	_, _, version, ok := f.u.PreviousBinary()
	isTrue(t, ok, "reported")
	eq(t, version, "1.4.0", "the record describes the new retained binary")

	entries, err := os.ReadDir(filepath.Dir(f.bin))
	noErr(t, err, "read the install directory")
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(f.bin)+".app-previous") {
			n++
		}
	}
	eq(t, n, 2, "exactly one retained binary and one record")
}

// A rollback puts the program back on the binary the update replaced; the
// retained one is still the one before that, and stays.
func TestRollback_LeavesTheRetainedPreviousAlone(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting, retaining)
	seedPrevious(t, f.u, "gen-0", "0.9.0")

	noErr(t, f.u.Rollback("the probe failed"), "Rollback")

	eq(t, readFileString(t, f.bin), "old-binary", "the replaced binary is restored")
	eq(t, readFileString(t, f.u.paths.Previous), "gen-0", "the retained binary is untouched")
	_, _, version, ok := f.u.PreviousBinary()
	isTrue(t, ok, "still reported")
	eq(t, version, "0.9.0", "still the older generation")
}

func TestCleanupReported_SparesTheRetainedPrevious(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting, retaining)
	noErr(t, f.u.Commit(), "Commit")
	noErr(t, f.u.MarkReported(), "MarkReported")

	f.u.CleanupReported()

	isFalse(t, exists(f.u.paths.State), "the journal is cleaned up")
	isTrue(t, exists(f.u.paths.Previous), "the retained binary is not a leftover")
	isTrue(t, exists(f.u.paths.PreviousRecord), "nor is its record")
}

func TestRepair_SparesTheRetainedPrevious(t *testing.T) {
	t.Run("no journal", func(t *testing.T) {
		f := newRepairFixture(t, "program", "1.0.0")
		seedPrevious(t, f.u, "gen-0", "0.9.0")

		noErr(t, f.u.Repair(), "Repair")

		isTrue(t, exists(f.p.Previous), "retained binary kept")
		isTrue(t, exists(f.p.PreviousRecord), "record kept")
	})

	t.Run("stale journal", func(t *testing.T) {
		withPIDAlive(t, noProcessAlive)
		f := newRepairFixture(t, "program", "1.0.0")
		f.u.cfg.StaleThreshold = time.Nanosecond
		stageJournal(t, f.u, &state{Phase: "bogus"})
		backdate(t, f.p.State, time.Hour)
		seedPrevious(t, f.u, "gen-0", "0.9.0")

		noErr(t, f.u.Repair(), "Repair")

		isFalse(t, exists(f.p.State), "the stale journal is collected")
		isTrue(t, exists(f.p.Previous), "retained binary kept")
		isTrue(t, exists(f.p.PreviousRecord), "record kept")
	})

	t.Run("stale corrupt journal", func(t *testing.T) {
		f := newRepairFixture(t, "program", "1.0.0")
		noErr(t, os.WriteFile(f.p.State, []byte("{"), 0o600), "corrupt journal")
		backdate(t, f.p.State, 2*time.Hour)
		seedPrevious(t, f.u, "gen-0", "0.9.0")

		noErr(t, f.u.Repair(), "Repair")

		isTrue(t, exists(f.p.Previous), "retained binary kept")
		isTrue(t, exists(f.p.PreviousRecord), "record kept")
	})
}

// A process can stop anywhere inside a retention. Every start with the
// committed journal still on disk finishes it, and no state ever leaves a
// record describing a file other than the one beside it.
func TestRetainPrevious_FinishesAnInterruptedRetention(t *testing.T) {
	oldSum := sha256Hex([]byte("old-binary"))
	for _, tc := range []struct {
		name        string
		arrange     func(t *testing.T, u *Updater)
		wantContent string // "" = no retained binary
		wantVersion string // "" = PreviousBinary reports nothing
	}{
		{
			name: "stopped before the rename",
			arrange: func(t *testing.T, u *Updater) {
				writeBinary(t, u.paths.RollbackBinary, "old-binary")
				seedPrevious(t, u, "gen-0", "0.9.0")
			},
			wantContent: "old-binary", wantVersion: "1.4.0",
		},
		{
			name: "stopped between the rename and the record",
			arrange: func(t *testing.T, u *Updater) {
				writeBinary(t, u.paths.Previous, "old-binary")
			},
			wantContent: "old-binary", wantVersion: "1.4.0",
		},
		{
			name: "an undescribed binary that is not the replaced one",
			arrange: func(t *testing.T, u *Updater) {
				writeBinary(t, u.paths.Previous, "something else")
			},
		},
		{
			name: "already finished",
			arrange: func(t *testing.T, u *Updater) {
				seedPrevious(t, u, "old-binary", "1.4.0")
			},
			wantContent: "old-binary", wantVersion: "1.4.0",
		},
		{
			name: "a recorded older generation with no rollback copy is left as it is",
			arrange: func(t *testing.T, u *Updater) {
				seedPrevious(t, u, "gen-0", "0.9.0")
			},
			wantContent: "gen-0", wantVersion: "0.9.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t, "new-binary", "1.5.0")
			f.u.cfg.RetainPrevious = true
			stageJournal(t, f.u, &state{
				Phase: phaseCommitted, CommandID: "cmd-1",
				OldVersion: "1.4.0", TargetVersion: "1.5.0", OldSHA256: oldSum,
			})
			tc.arrange(t, f.u)

			noErr(t, f.u.Repair(), "Repair")

			isFalse(t, exists(f.p.RollbackBinary), "no rollback copy remains")
			if tc.wantContent == "" {
				isFalse(t, exists(f.p.Previous), "an undescribed retained binary is removed")
			} else {
				eq(t, readFileString(t, f.p.Previous), tc.wantContent, "retained content")
			}
			_, _, version, ok := f.u.PreviousBinary()
			eq(t, ok, tc.wantVersion != "", "PreviousBinary ok")
			eq(t, version, tc.wantVersion, "PreviousBinary version")
		})
	}
}

// The commit is the verdict; failing to retain afterwards must not reverse it,
// and must not lose the replaced binary either. CleanupReported keeps the
// journal until retention succeeds, and the next start's Repair finishes the
// retention and removes the journal - the wiring calls CleanupReported only for
// a pending outcome, so no second call is assumed.
func TestCommit_RetentionFailureDoesNotReverseTheCommit(t *testing.T) {
	log := &captureLog{}
	f := newAttestFixture(t, phaseAttesting, func(c *Config) {
		c.RetainPrevious = true
		c.Log = log.Logger()
	})
	// A non-empty directory where the retained binary goes: no platform renames
	// a file over one.
	noErr(t, os.MkdirAll(filepath.Join(f.u.paths.Previous, "blocker"), 0o755), "block the retained path")

	noErr(t, f.u.Commit(), "the commit itself stands")
	j, err := readState(f.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "journal phase")
	rec, err := f.u.records.load()
	noErr(t, err, "load the record")
	eq(t, rec.Status, StatusSucceeded, "recorded status")
	isTrue(t, exists(f.u.paths.RollbackBinary), "the replaced binary is not lost")
	isTrue(t, log.contains("could not retain the previous binary"), "the failure is logged")
	_, _, _, ok := f.u.PreviousBinary()
	isFalse(t, ok, "nothing is reported as retained")

	noErr(t, f.u.MarkReported(), "MarkReported")
	f.u.CleanupReported()
	isTrue(t, exists(f.u.paths.State), "the journal is kept so the retention can finish")
	isTrue(t, exists(f.u.paths.RollbackBinary), "the replaced binary is still kept")

	pending, err := f.u.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if pending != nil {
		t.Fatal("the outcome was reported, so the documented wiring never calls CleanupReported again")
	}

	noErr(t, os.RemoveAll(f.u.paths.Previous), "unblock")
	noErr(t, f.u.Repair(), "the next start's Repair")
	isFalse(t, exists(f.u.paths.State), "the journal goes once retention succeeds")
	eq(t, readFileString(t, f.u.paths.Previous), "old-binary", "retained at last")
	_, _, version, ok := f.u.PreviousBinary()
	isTrue(t, ok, "PreviousBinary reports it")
	eq(t, version, "1.4.0", "the version the update moved away from")
}

// The old record goes BEFORE the binary it describes is replaced. When it cannot
// be removed, the older generation must stay exactly as it is: replacing the
// binary first would leave the old record describing the new bytes, and the
// next retention would accept that pairing as already finished.
func TestCommit_RetentionReplacesNothingWhileTheOldRecordCannotBeRemoved(t *testing.T) {
	f := newAttestFixture(t, phaseAttesting, retaining)
	writeBinary(t, f.u.paths.Previous, "gen-0")
	// A non-empty directory where the record goes: no platform removes one with
	// a plain remove.
	noErr(t, os.MkdirAll(filepath.Join(f.u.paths.PreviousRecord, "blocker"), 0o755), "block record removal")

	noErr(t, f.u.Commit(), "Commit")

	isTrue(t, exists(f.u.paths.RollbackBinary), "rollback copy kept")
	eq(t, readFileString(t, f.u.paths.Previous), "gen-0", "older generation not replaced while its record could not be removed")
}

// A commit whose journal write did not land is reconciled by Repair from the
// outcome record. That path releases the rollback copy too, so it retains too.
func TestRepair_ReconciledCommitRetainsThePrevious(t *testing.T) {
	f := newRepairFixture(t, "new-binary", "1.5.0")
	f.u.cfg.RetainPrevious = true
	noErr(t, f.u.records.record(&Outcome{
		At: time.Now(), ID: "cmd-1", FromVersion: "1.4.0", ToVersion: "1.5.0", Status: StatusSucceeded,
	}), "seed the succeeded record")
	stageJournal(t, f.u, &state{
		Phase: phaseAttesting, CommandID: "cmd-1",
		OldVersion: "1.4.0", TargetVersion: "1.5.0", OldSHA256: sha256Hex([]byte("old-binary")),
	})
	writeBinary(t, f.p.RollbackBinary, "old-binary")

	noErr(t, f.u.Repair(), "Repair")

	j, err := readState(f.p.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "the journal is reconciled")
	isFalse(t, exists(f.p.RollbackBinary), "the rollback copy is moved")
	eq(t, readFileString(t, f.p.Previous), "old-binary", "the replaced binary is retained")
	_, _, version, ok := f.u.PreviousBinary()
	isTrue(t, ok, "PreviousBinary reports it")
	eq(t, version, "1.4.0", "the version the update moved away from")
}

// Repair finishing a retention cannot fail startup over it, but it must say so.
func TestRepair_CommittedRetentionFailureIsLogged(t *testing.T) {
	log := &captureLog{}
	f := newRepairFixture(t, "new-binary", "1.5.0")
	f.u.cfg.RetainPrevious = true
	f.u.cfg.Log = log.Logger()
	stageJournal(t, f.u, &state{
		Phase: phaseCommitted, CommandID: "cmd-1",
		OldVersion: "1.4.0", TargetVersion: "1.5.0", OldSHA256: sha256Hex([]byte("old-binary")),
	})
	writeBinary(t, f.p.RollbackBinary, "old-binary")
	noErr(t, os.MkdirAll(filepath.Join(f.p.Previous, "blocker"), 0o755), "block the retained path")

	noErr(t, f.u.Repair(), "a retention failure does not fail startup")

	isTrue(t, exists(f.p.RollbackBinary), "the replaced binary is not lost")
	isTrue(t, log.contains("error self-update: could not retain the previous binary"), "the failure is logged at ERROR")
}

// A retained binary whose record cannot be read and whose bytes are not the
// journal's old binary is deleted - and so is the unreadable record, or
// PreviousBinary would log about it on every call for good.
func TestRetainPrevious_DeletesAnUnreadableRecordWithItsUndescribedBinary(t *testing.T) {
	log := &captureLog{}
	f := newRepairFixture(t, "new-binary", "1.5.0")
	f.u.cfg.RetainPrevious = true
	f.u.cfg.Log = log.Logger()
	stageJournal(t, f.u, &state{
		Phase: phaseCommitted, CommandID: "cmd-1",
		OldVersion: "1.4.0", TargetVersion: "1.5.0", OldSHA256: sha256Hex([]byte("old-binary")),
	})
	writeBinary(t, f.p.Previous, "something else")
	noErr(t, os.WriteFile(f.p.PreviousRecord, []byte("{"), 0o600), "corrupt the record")

	noErr(t, f.u.Repair(), "Repair")

	isFalse(t, exists(f.p.Previous), "the undescribed binary is deleted")
	isFalse(t, exists(f.p.PreviousRecord), "the unreadable record is deleted with it")
	_, _, _, ok := f.u.PreviousBinary()
	isFalse(t, ok, "nothing is reported")
	isFalse(t, log.contains("could not read the record of the retained previous binary"), "no ERROR left behind")
}

// The unreadable record goes before the binary. When it cannot be removed the
// binary stays too: deleting the binary first would strand a record with no
// binary, which retention never revisits.
func TestRetainPrevious_KeepsTheBinaryWhileItsUnreadableRecordCannotBeRemoved(t *testing.T) {
	log := &captureLog{}
	f := newRepairFixture(t, "new-binary", "1.5.0")
	f.u.cfg.RetainPrevious = true
	f.u.cfg.Log = log.Logger()
	stageJournal(t, f.u, &state{
		Phase: phaseCommitted, CommandID: "cmd-1",
		OldVersion: "1.4.0", TargetVersion: "1.5.0", OldSHA256: sha256Hex([]byte("old-binary")),
	})
	writeBinary(t, f.p.Previous, "something else")
	// A non-empty directory where the record goes: unreadable as a record, and
	// no plain remove deletes it.
	noErr(t, os.MkdirAll(filepath.Join(f.p.PreviousRecord, "blocker"), 0o755), "block the record")

	noErr(t, f.u.Repair(), "Repair")

	eq(t, readFileString(t, f.p.Previous), "something else", "the binary stays while its record cannot be removed")
	isTrue(t, log.contains("error self-update: could not retain the previous binary"), "the failure is logged at ERROR")
}

// PreviousBinary has no error return, so anything wrong is logged and reported
// as absent - never as present.
func TestPreviousBinary_ReportsOnlyWhatIsReallyThere(t *testing.T) {
	t.Run("nothing retained", func(t *testing.T) {
		log := &captureLog{}
		u := testUpdater(t, func(c *Config) { c.Log = log.Logger() })
		_, _, _, ok := u.PreviousBinary()
		isFalse(t, ok, "ok")
		isFalse(t, log.contains("retained previous binary"), "absence is normal and not logged")
	})

	t.Run("corrupt record", func(t *testing.T) {
		log := &captureLog{}
		u := testUpdater(t, func(c *Config) { c.Log = log.Logger() })
		writeBinary(t, u.paths.Previous, "gen-0")
		noErr(t, os.WriteFile(u.paths.PreviousRecord, []byte("{"), 0o600), "corrupt the record")
		_, _, _, ok := u.PreviousBinary()
		isFalse(t, ok, "ok")
		isTrue(t, log.contains("could not read the record of the retained previous binary"), "logged")
	})

	// A record that parses but names no version or no digest describes nothing
	// an update could be matched against.
	for name, content := range map[string]string{
		"empty object": `{}`,
		"null":         `null`,
		"no digest":    `{"version": "0.9.0"}`,
		"no version":   `{"sha256": "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e"}`,
	} {
		t.Run("incomplete record: "+name, func(t *testing.T) {
			log := &captureLog{}
			u := testUpdater(t, func(c *Config) { c.Log = log.Logger() })
			writeBinary(t, u.paths.Previous, "gen-0")
			noErr(t, os.WriteFile(u.paths.PreviousRecord, []byte(content), 0o600), "write the record")
			_, _, _, ok := u.PreviousBinary()
			isFalse(t, ok, "ok")
			isTrue(t, log.contains("error self-update: could not read the record of the retained previous binary"), "logged at ERROR")
		})
	}

	t.Run("record without its binary", func(t *testing.T) {
		log := &captureLog{}
		u := testUpdater(t, func(c *Config) { c.Log = log.Logger() })
		noErr(t, u.writePreviousRecord("0.9.0", sha256Hex([]byte("gen-0"))), "write a record")
		_, _, _, ok := u.PreviousBinary()
		isFalse(t, ok, "ok")
		isTrue(t, log.contains("the retained previous binary is missing"), "logged")
	})
}

// The record is read by the NEXT version of a program, so its field names are an
// on-disk contract.
func TestPreviousRecordWireCompatibility(t *testing.T) {
	const fixture = `{
  "version": "1.4.0",
  "sha256": "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e",
  "retained_at": "2026-09-17T08:00:00Z"
}`
	path := filepath.Join(t.TempDir(), "prog.app-previous.json")
	noErr(t, os.WriteFile(path, []byte(fixture), 0o600), "write the fixture")

	rec, err := readPreviousRecord(path)
	noErr(t, err, "parse")
	eq(t, rec.Version, "1.4.0", "version")
	eq(t, rec.SHA256, "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e", "sha256")
	eq(t, rec.RetainedAt.UTC().Format(time.RFC3339), "2026-09-17T08:00:00Z", "retained_at")
}

// The opt-in guard at CleanupReported: a program that never set RetainPrevious
// gets v0.3.0's cleanup, which deletes a rollback copy a crash left behind -
// never a retained binary and its record.
func TestCleanupReported_WithoutRetainPreviousRetainsNothing(t *testing.T) {
	f := newRepairFixture(t, "new-binary", "1.5.0")
	stageJournal(t, f.u, &state{
		Phase: phaseCommitted, CommandID: "cmd-1",
		OldVersion: "1.4.0", TargetVersion: "1.5.0", OldSHA256: sha256Hex([]byte("old-binary")),
	})
	writeBinary(t, f.p.RollbackBinary, "old-binary")

	f.u.CleanupReported()

	isFalse(t, exists(f.p.Previous), "nothing is retained")
	isFalse(t, exists(f.p.PreviousRecord), "no record is written")
	isFalse(t, exists(f.p.RollbackBinary), "the rollback copy is deleted, as in v0.3.0")
	isFalse(t, exists(f.p.State), "the journal is removed")
}

// The rollback copy is released only once the commit is RECORDED. A commit whose
// record cannot be written is refused and the update is still undecided, so the
// crash counter or a later Rollback may yet need the copy to restore from.
func TestCommit_ARefusedCommitKeepsTheRollbackCopy(t *testing.T) {
	// A non-empty directory where the record goes: no platform renames a file
	// over one, so the record write fails.
	blockRecord := func(t *testing.T, u *Updater) {
		noErr(t, os.MkdirAll(filepath.Join(u.paths.Record, "blocker"), 0o755), "block the record")
	}
	// Under CooldownRolledBackVersion every write reads the previous record
	// first, so a corrupt one refuses the commit by design.
	corruptRecord := func(t *testing.T, u *Updater) {
		noErr(t, os.WriteFile(u.paths.Record, []byte("{"), 0o600), "corrupt the record")
	}
	scoped := func(c *Config) { c.CooldownScope = CooldownRolledBackVersion }
	for _, tc := range []struct {
		name    string
		apply   []func(*Config)
		arrange func(t *testing.T, u *Updater)
	}{
		{name: "without RetainPrevious", arrange: blockRecord},
		{name: "with RetainPrevious", apply: []func(*Config){retaining}, arrange: blockRecord},
		{name: "a corrupt record under the scoped cooldown", apply: []func(*Config){retaining, scoped}, arrange: corruptRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttestFixture(t, phaseAttesting, tc.apply...)
			tc.arrange(t, f.u)

			wantErr(t, f.u.Commit(), "a commit that cannot be recorded")

			eq(t, readFileString(t, f.u.paths.RollbackBinary), "old-binary", "the rollback copy is kept")
			isFalse(t, exists(f.u.paths.Previous), "nothing is retained")
			j, err := readState(f.u.paths.State)
			noErr(t, err, "readState")
			eq(t, j.Phase, phaseAttesting, "the update is still undecided")
		})
	}
}
