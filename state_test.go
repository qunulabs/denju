package denju

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadState_RoundTrip(t *testing.T) {
	n := newNames("app")
	path := filepath.Join(t.TempDir(), "journal.json")

	want := &state{
		CommandID:     "cmd-1",
		TargetVersion: "1.4.0",
		OldVersion:    "1.3.0",
		BinaryPath:    "/opt/prog/prog",
		NewBinaryPath: "/opt/prog/.app-update-123",
		Args:          []string{"--flag"},
		Cwd:           "/opt/prog",
		StartedAt:     time.Now().UTC().Truncate(time.Second),
		Phase:         phaseSwapped,
		CrashCount:    1,
		OldSHA256:     "aaa",
		NewSHA256:     "bbb",
		PID:           4242,
		HelperPID:     4343,
		ServiceName:   "prog",
		ErrorMessage:  "",
	}
	noErr(t, writeState(n, path, want), "writeState")

	got, err := readState(path)
	noErr(t, err, "readState")

	eq(t, got.CommandID, want.CommandID, "CommandID")
	eq(t, got.TargetVersion, want.TargetVersion, "TargetVersion")
	eq(t, got.OldVersion, want.OldVersion, "OldVersion")
	eq(t, got.BinaryPath, want.BinaryPath, "BinaryPath")
	eq(t, got.NewBinaryPath, want.NewBinaryPath, "NewBinaryPath")
	eq(t, len(got.Args), 1, "len(Args)")
	eq(t, got.Args[0], "--flag", "Args[0]")
	eq(t, got.Cwd, want.Cwd, "Cwd")
	isTrue(t, got.StartedAt.Equal(want.StartedAt), "StartedAt round-trips")
	eq(t, got.Phase, want.Phase, "Phase")
	eq(t, got.CrashCount, want.CrashCount, "CrashCount")
	eq(t, got.OldSHA256, want.OldSHA256, "OldSHA256")
	eq(t, got.NewSHA256, want.NewSHA256, "NewSHA256")
	eq(t, got.PID, want.PID, "PID")
	eq(t, got.HelperPID, want.HelperPID, "HelperPID")
	eq(t, got.ServiceName, want.ServiceName, "ServiceName")
}

// writeState replaces the journal atomically, so a reader can never observe a
// half-written file and no temp file is left behind.
func TestWriteState_LeavesNoTempFiles(t *testing.T) {
	n := newNames("app")
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")

	noErr(t, writeState(n, path, &state{Phase: phaseStaged}), "first write")
	noErr(t, writeState(n, path, &state{Phase: phaseSwapped}), "second write")

	entries, err := os.ReadDir(dir)
	noErr(t, err, "read the directory")
	eq(t, len(entries), 1, "only the journal itself should remain")
	eq(t, entries[0].Name(), "journal.json", "remaining file")

	got, err := readState(path)
	noErr(t, err, "readState")
	eq(t, got.Phase, phaseSwapped, "phase after the second write")
}

func TestWriteState_NilIsRejected(t *testing.T) {
	err := writeState(newNames("app"), filepath.Join(t.TempDir(), "j.json"), nil)
	wantErr(t, err, "writeState(nil)")
}

// A journal that will not parse is an error, never an empty state: silently
// treating corruption as "no update in flight" would strand a swapped binary.
func TestReadState_CorruptIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	noErr(t, os.WriteFile(path, []byte("{not json"), 0o600), "write a corrupt journal")

	_, err := readState(path)
	wantErrContaining(t, err, "corrupt", "readState on a corrupt journal")
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	isFalse(t, exists(path), "exists before creation")
	noErr(t, os.WriteFile(path, []byte("x"), 0o600), "create the file")
	isTrue(t, exists(path), "exists after creation")
}

func TestDefaultPIDAlive(t *testing.T) {
	isTrue(t, defaultPIDAlive(os.Getpid()), "our own pid is alive")
	isFalse(t, defaultPIDAlive(0), "pid 0")
	isFalse(t, defaultPIDAlive(-1), "pid -1")
}

// TestJournalWireCompatibility pins the JSON the journal is written as.
//
// This is the second half of the naming contract. A program upgrading to a
// denju-based build has its update driven by the PREVIOUS binary, which wrote
// this exact shape; the successor has to read it back. Renaming a field, or
// changing how one is encoded, silently strands any update in flight across the
// changeover - the successor sees a journal it cannot make sense of, and
// resolves an update that actually succeeded as an interrupted one.
//
// The fixture below is a journal as produced by the implementations denju was
// extracted from. It must keep parsing, field for field, forever.
func TestJournalWireCompatibility(t *testing.T) {
	const fixture = `{
  "command_id": "d3b0c442-98fc-4e1b-9a2f-000000000001",
  "target_version": "1.4.0",
  "old_version": "1.3.2",
  "binary_path": "/opt/prog/prog",
  "new_binary_path": "/opt/prog/.app-update-2417583991",
  "args": [
    "--config",
    "/etc/prog.yaml"
  ],
  "cwd": "/opt/prog",
  "started_at": "2026-07-26T18:04:11.523481Z",
  "phase": "attesting",
  "crash_count": 1,
  "old_sha256": "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e",
  "new_sha256": "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
  "pid": 30412,
  "helper_pid": 30455,
  "service_name": "prog",
  "error_message": "the new version did not attest healthy within 5m0s"
}`

	path := filepath.Join(t.TempDir(), "journal.json")
	noErr(t, os.WriteFile(path, []byte(fixture), 0o600), "write the fixture")

	j, err := readState(path)
	noErr(t, err, "parse a journal written by a pre-denju build")

	eq(t, j.CommandID, "d3b0c442-98fc-4e1b-9a2f-000000000001", "command_id")
	eq(t, j.TargetVersion, "1.4.0", "target_version")
	eq(t, j.OldVersion, "1.3.2", "old_version")
	eq(t, j.BinaryPath, "/opt/prog/prog", "binary_path")
	eq(t, j.NewBinaryPath, "/opt/prog/.app-update-2417583991", "new_binary_path")
	eq(t, len(j.Args), 2, "len(args)")
	eq(t, j.Args[0], "--config", "args[0]")
	eq(t, j.Args[1], "/etc/prog.yaml", "args[1]")
	eq(t, j.Cwd, "/opt/prog", "cwd")
	eq(t, j.StartedAt.UTC().Format(time.RFC3339), "2026-07-26T18:04:11Z", "started_at")
	eq(t, j.Phase, phaseAttesting, "phase")
	eq(t, j.CrashCount, 1, "crash_count")
	eq(t, j.OldSHA256, "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e", "old_sha256")
	eq(t, j.NewSHA256, "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809", "new_sha256")
	eq(t, j.PID, 30412, "pid")
	eq(t, j.HelperPID, 30455, "helper_pid")
	eq(t, j.ServiceName, "prog", "service_name")
	eq(t, j.ErrorMessage, "the new version did not attest healthy within 5m0s", "error_message")
}

// TestPhaseWireValues pins the phase strings. They are persisted, and a
// successor written against different spellings would misread every journal.
func TestPhaseWireValues(t *testing.T) {
	eq(t, phaseStaged, "staged", "phaseStaged")
	eq(t, phaseSwapped, "swapped", "phaseSwapped")
	eq(t, phaseAttesting, "attesting", "phaseAttesting")
	eq(t, phaseCommitted, "committed", "phaseCommitted")
	eq(t, phaseRolledBack, "rolledback", "phaseRolledBack")
}
