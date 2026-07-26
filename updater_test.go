package denju

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_RequiresANamespace(t *testing.T) {
	_, err := New(Config{Version: "1.0.0", BinaryPath: "/opt/prog/prog"})
	wantErrContaining(t, err, "Namespace", "New with no namespace")
}

// Without a version, startup repair cannot tell the new image from the old one,
// so every start after a swap would look like an interrupted update and undo a
// perfectly good one.
func TestNew_RequiresAVersion(t *testing.T) {
	_, err := New(Config{Namespace: "app", BinaryPath: "/opt/prog/prog"})
	wantErrContaining(t, err, "Version", "New with no version")
}

func TestNew_RejectsAnInvalidNamespace(t *testing.T) {
	_, err := New(Config{Namespace: "Bad Namespace", Version: "1.0.0", BinaryPath: "/x"})
	wantErrContaining(t, err, "Namespace", "New with an invalid namespace")
}

func TestNew_AppliesDefaults(t *testing.T) {
	u := testUpdater(t)
	eq(t, u.cfg.DrainTimeout, defaultDrainTimeout, "DrainTimeout")
	eq(t, u.cfg.SelftestTimeout, defaultSelftestTimeout, "SelftestTimeout")
	eq(t, u.cfg.HandshakeTimeout, defaultHandshakeTimeout, "HandshakeTimeout")
	eq(t, u.cfg.DownloadTimeout, defaultDownloadTimeout, "DownloadTimeout")
	eq(t, u.cfg.CrashTolerance, defaultCrashTolerance, "CrashTolerance")
	eq(t, u.cfg.StaleThreshold, defaultStaleThreshold, "StaleThreshold")
	eq(t, u.cfg.Cooldown, time.Duration(0), "Cooldown defaults to disabled")
}

func TestNew_KeepsExplicitTunables(t *testing.T) {
	u := testUpdater(t, func(c *Config) {
		c.DrainTimeout = time.Second
		c.CrashTolerance = 5
		c.Cooldown = time.Hour
	})
	eq(t, u.cfg.DrainTimeout, time.Second, "DrainTimeout")
	eq(t, u.cfg.CrashTolerance, 5, "CrashTolerance")
	eq(t, u.cfg.Cooldown, time.Hour, "Cooldown")
}

// The record defaults beside the binary, but a program with a state directory
// should point it there - it has to outlive the binary being replaced.
func TestNew_RecordPath(t *testing.T) {
	t.Run("defaults beside the binary", func(t *testing.T) {
		u := testUpdater(t)
		eq(t, u.records.path, u.binaryPath+".app-record.json", "default record path")
	})

	t.Run("honours an override", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "state", "last-update.json")
		u := testUpdater(t, func(c *Config) { c.RecordPath = custom })
		eq(t, u.records.path, custom, "overridden record path")
		eq(t, u.paths.Record, custom, "paths.Record follows the override")
	})
}

func TestBinaryPath(t *testing.T) {
	u := testUpdater(t)
	eq(t, u.BinaryPath(), u.binaryPath, "BinaryPath")
}

func TestResolveBinary(t *testing.T) {
	got, err := ResolveBinary()
	noErr(t, err, "ResolveBinary")
	isTrue(t, filepath.IsAbs(got), "the resolved path is absolute")
}

// A nil Logger means silence, not a panic. It is the default, so every code path
// has to tolerate it.
func TestLog_NilLoggerIsSilent(t *testing.T) {
	u := testUpdater(t)
	u.cfg.Log = nil
	u.log(LevelError, "this must not panic", "key", "value")
}

func TestSlogLogger(t *testing.T) {
	var got []string
	h := slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		got = append(got, string(p))
		return len(p), nil
	}), &slog.HandlerOptions{Level: slog.LevelDebug})

	logger := SlogLogger(slog.New(h))
	for _, lvl := range []Level{LevelDebug, LevelInfo, LevelWarn, LevelError} {
		logger(lvl, "message", "k", "v")
	}
	eq(t, len(got), 4, "every level is forwarded")

	if SlogLogger(nil) != nil {
		t.Fatal("SlogLogger(nil) must be nil so it means silence")
	}
}

func TestLevelString(t *testing.T) {
	eq(t, LevelDebug.String(), "debug", "LevelDebug")
	eq(t, LevelInfo.String(), "info", "LevelInfo")
	eq(t, LevelWarn.String(), "warn", "LevelWarn")
	eq(t, LevelError.String(), "error", "LevelError")
	eq(t, Level(99).String(), "unknown", "an out-of-range level")
}

// Every line denju emits is prefixed, so a program merging denju's output into
// its own log can still tell where a line came from.
func TestLog_PrefixesEveryLine(t *testing.T) {
	c := &captureLog{}
	u := testUpdater(t, func(cfg *Config) { cfg.Log = c.Logger() })
	u.log(LevelInfo, "something happened")
	isTrue(t, c.contains("self-update: something happened"), "the prefix is applied")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
