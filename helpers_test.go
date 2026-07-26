package denju

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// denju has no test dependencies on purpose: a library this small, sitting this
// close to a program's ability to start at all, should not drag anything into a
// consumer's module graph. These are the handful of assertions the ported suite
// needs.

func noErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", what, err)
	}
}

func wantErr(t *testing.T, err error, what string) error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error, got nil", what)
	}
	return err
}

func wantErrContaining(t *testing.T, err error, substr, what string) {
	t.Helper()
	wantErr(t, err, what)
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("%s: error %q does not contain %q", what, err.Error(), substr)
	}
}

// hasSubstr asserts on a plain string rather than an error - journal and record
// messages are read by operators, so their wording is worth pinning.
func hasSubstr(t *testing.T, got, substr, what string) {
	t.Helper()
	if !strings.Contains(got, substr) {
		t.Errorf("%s = %q, does not contain %q", what, got, substr)
	}
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func neq[T comparable](t *testing.T, got, unwanted T, what string) {
	t.Helper()
	if got == unwanted {
		t.Errorf("%s = %v, want anything else", what, got)
	}
}

func isTrue(t *testing.T, got bool, what string) {
	t.Helper()
	if !got {
		t.Errorf("%s = false, want true", what)
	}
}

func isFalse(t *testing.T, got bool, what string) {
	t.Helper()
	if got {
		t.Errorf("%s = true, want false", what)
	}
}

// withGOOS forces the package's effective OS for one test and restores it after,
// so the Windows-only paths are exercised on any host.
func withGOOS(t *testing.T, os string) {
	t.Helper()
	prev := goos
	goos = os
	t.Cleanup(func() { goos = prev })
}

// withPIDAlive makes liveness deterministic for one test.
func withPIDAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	prev := pidAlive
	pidAlive = fn
	t.Cleanup(func() { pidAlive = prev })
}

// noProcessAlive is the common case for repair tests: everything the journal
// remembers is gone.
func noProcessAlive(int) bool { return false }

// writeBinary creates a fake "binary" with executable permissions.
func writeBinary(t *testing.T, path, content string) {
	t.Helper()
	noErr(t, os.WriteFile(path, []byte(content), 0o755), "write "+path)
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	noErr(t, err, "read "+path)
	return string(data)
}

// testUpdater builds an Updater over a throwaway binary in its own directory.
//
// The namespace is deliberately NOT one of the real ones from the naming
// contract: nothing here should be able to pass by accidentally matching a
// production name.
func testUpdater(t *testing.T, apply ...func(*Config)) *Updater {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "old-binary")

	cfg := Config{
		Namespace:  "app",
		Version:    "1.0.0",
		BinaryPath: bin,
	}
	for _, fn := range apply {
		fn(&cfg)
	}
	u, err := New(cfg)
	noErr(t, err, "New")
	return u
}

// captureLog collects everything the Updater logs, so a test can assert on what
// an operator would have been told.
//
// It is locked because a Logger genuinely is called from more than one
// goroutine - the attestation deadline goroutine logs, and so does a Drain
// callback that overran its timeout and was abandoned. Consumers face the same
// requirement; this is the test suite holding itself to the contract it
// documents.
type captureLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLog) Logger() Logger {
	return func(level Level, msg string, attrs ...any) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, level.String()+" "+msg)
	}
}

func (c *captureLog) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// stageJournal writes a journal for the updater's binary in the given phase.
func stageJournal(t *testing.T, u *Updater, j *state) {
	t.Helper()
	if j.BinaryPath == "" {
		j.BinaryPath = u.binaryPath
	}
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now()
	}
	noErr(t, writeState(u.n, u.paths.State, j), "stage the journal")
}

// backdate makes a file look older than it is, for staleness tests.
func backdate(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	noErr(t, os.Chtimes(path, old, old), "backdate "+path)
}
