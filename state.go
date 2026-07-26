package denju

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// Journal phases. An update is a write-ahead journal: the process records
// phaseSwapped before it moves the binary aside, phaseAttesting before it
// restarts into the new image, and the new image records phaseCommitted or
// phaseRolledBack once it has decided. Startup repair reads the phase to
// resolve an interrupted update deterministically.
const (
	// phaseStaged means the update is staged and a Windows handoff is in
	// progress, but nothing has been swapped yet.
	phaseStaged = "staged"
	// phaseSwapped means the new binary is in place but the process has not
	// restarted into it yet.
	phaseSwapped = "swapped"
	// phaseAttesting means the new image is running (or about to) and is proving
	// that it works.
	phaseAttesting = "attesting"
	// phaseCommitted means the new image attested healthy; the update is
	// permanent and the rollback copy has been discarded.
	phaseCommitted = "committed"
	// phaseRolledBack means the update was undone; the cause is in ErrorMessage.
	phaseRolledBack = "rolledback"
)

// goos is the effective target OS, seeded from runtime.GOOS. It is a package var
// so tests can drive the Windows-only paths on any host - the Windows flow is
// the one that is hardest to reach in CI and the most important to get right.
var goos = runtime.GOOS

// pidAlive reports whether a PID is currently running. A package var so tests
// can control liveness deterministically.
var pidAlive = defaultPIDAlive

// state is the update journal, written beside the binary. Exactly one process
// reads or writes it at a time.
//
// The process ENVIRONMENT is deliberately never persisted. It routinely carries
// credentials, and a journal is a plain world-readable file that outlives the
// process. The environment reaches the successor by inheritance instead - the
// exec on Unix, the detached spawn's explicit env on Windows - and never touches
// disk.
//
// The JSON field names below are an on-disk contract between consecutive
// versions of a program. Renaming one silently breaks any update in flight
// across the changeover.
type state struct {
	// CommandID ties an outcome report back to the request that caused it.
	CommandID string `json:"command_id"`
	// TargetVersion is the version being moved to; OldVersion the one replaced.
	// Either may be numerically lower than the other - a deliberate downgrade is
	// a first-class outcome, not an error.
	TargetVersion string `json:"target_version"`
	OldVersion    string `json:"old_version"`
	// BinaryPath is the absolute path of the binary being replaced.
	BinaryPath string `json:"binary_path"`
	// NewBinaryPath is the verified download's temp path, staged for the swap.
	NewBinaryPath string `json:"new_binary_path"`
	// Args and Cwd let a Windows relaunch reproduce the original invocation. On
	// Unix the exec replays os.Args directly, so they are informational.
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`

	StartedAt time.Time `json:"started_at"`
	Phase     string    `json:"phase"`

	// CrashCount counts how many times a new image has reached startup repair
	// still uncommitted. A rising count is how a crash loop is detected.
	CrashCount int `json:"crash_count"`
	// OldSHA256 and NewSHA256 identify the two binaries by content, so repair
	// can tell which one it is running even when the version strings cannot.
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
	// PID is the process currently responsible for the update. Unix preserves it
	// across the exec; on Windows each role refreshes it to its own.
	PID int `json:"pid"`
	// HelperPID is the detached Windows helper's PID, written by the helper
	// itself as its first act - the handshake the original waits for. 0 on Unix.
	HelperPID int `json:"helper_pid,omitempty"`
	// ServiceName is the Windows service this process runs as ("" = console).
	// The helper restarts through the service control manager when it is set, so
	// the successor IS the service. Always "" on Unix.
	ServiceName string `json:"service_name,omitempty"`
	// ErrorMessage carries the cause when Phase is phaseRolledBack.
	ErrorMessage string `json:"error_message,omitempty"`
}

// paths holds every file an update uses beside a given binary. These names are
// operator-visible: they appear in the program's install directory.
type paths struct {
	// State is the update journal.
	State string
	// Restart is a SEPARATE journal used only to drive a Windows relaunch for a
	// plain caller-requested restart. It is deliberately not the update journal:
	// a restart carries no phase, no rollback copy and no outcome to report, and
	// letting the two share a file would mean a restart could overwrite an
	// in-flight update's write-ahead record.
	Restart string
	// RollbackBinary is <binary>.old - a HARDLINK to the old binary made before
	// the swap. A link rather than a rename-aside is what lets the swap be a
	// single atomic rename with the binary never missing.
	RollbackBinary string
	// HelperCopy is the self-copy that runs as the detached update helper
	// (Windows only; .exe suffixed so it is executable).
	HelperCopy string
	// Discard is where a running new binary is renamed aside during a Windows
	// rollback - a running .exe can be renamed but neither deleted nor replaced.
	// A later start reclaims it.
	Discard string
	// HelperLog is the detached helper's log file. The helper has no console, so
	// this is the only place its output can go.
	HelperLog string
	// Record is the durable outcome record. Unlike everything else here it may
	// be relocated by Config.RecordPath, because it has to outlive the journal.
	Record string
}

// pathsFor derives the update file paths for the binary at binaryPath.
func (n names) pathsFor(binaryPath string) paths {
	helper := binaryPath + n.sufHelper
	if goos == "windows" {
		helper += ".exe"
	}
	return paths{
		State:          binaryPath + n.sufState,
		Restart:        binaryPath + n.sufRestart,
		RollbackBinary: binaryPath + ".old",
		HelperCopy:     helper,
		Discard:        binaryPath + n.sufDiscard,
		HelperLog:      binaryPath + n.sufLog,
		Record:         binaryPath + n.sufRecord,
	}
}

// writeState atomically writes s as JSON to path: a temp file in the same
// directory is written and fsync'd, then renamed over path, and the directory is
// fsync'd best-effort. A reader never observes a half-written journal, and the
// write-ahead ordering survives a power cut.
func writeState(n names, path string, s *state) error {
	if s == nil {
		return errors.New("cannot write a nil update state")
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, n.tmpState)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	// fsync before the rename so the bytes are durable ahead of the rename
	// becoming visible. Repair's determinism depends on that ordering.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	syncDir(dir)
	return nil
}

// readState reads and parses the journal at path.
func readState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the update journal: %w", err)
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("the update journal is corrupt: %w", err)
	}
	return &s, nil
}

// exists reports whether a file exists at path.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// syncDir fsyncs a directory so a rename into it is durable. Best-effort:
// Windows does not permit opening a directory for sync, and the rename is atomic
// either way.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// defaultPIDAlive reports whether the process with the given PID is running. On
// Windows a successfully opened handle means alive; on Unix a signal-0 probe
// distinguishes live (nil, or EPERM meaning it exists but belongs to someone
// else) from dead.
func defaultPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if goos == "windows" {
		_ = proc.Release()
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
