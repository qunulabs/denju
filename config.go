package denju

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config describes how denju should update one particular program.
//
// Only Namespace and Version are required. Every timeout has a default that
// matches long production use; override them only with a reason.
type Config struct {
	// Namespace prefixes every file denju writes beside the binary and every
	// environment variable it sets. Lowercase letters, digits and dashes; it
	// must start with a letter or digit.
	//
	// It is part of the on-disk contract between consecutive versions of a
	// program: the version being replaced writes the journal that the version
	// replacing it has to read. Changing the namespace of a deployed program
	// therefore orphans any update in flight during the changeover - the
	// successor finds no journal, never commits, never reports, and leaves the
	// rollback copy behind for good. Choose it once and leave it alone.
	Namespace string

	// Version is the running build's version string. It is recorded in the
	// journal and is how startup repair tells the new image from the old one.
	//
	// Two different builds may legitimately carry the same version string; denju
	// falls back to comparing SHA-256 digests when they do.
	Version string

	// BinaryPath is the executable to replace. Defaults to [ResolveBinary],
	// which is os.Executable with symlinks resolved.
	BinaryPath string

	// RecordPath is where the durable outcome record lives. Defaults to
	// <binary>.<namespace>-record.json.
	//
	// Point it at a state directory if the program has one. The record has to
	// outlive the journal - it is the cooldown's only anchor and the only memory
	// of an outcome that still has to be reported - so it must not sit anywhere
	// that gets cleaned up between runs.
	RecordPath string

	// Log receives everything denju has to say. nil means silence.
	Log Logger

	// Selftest runs in the freshly downloaded binary, as a child process, before
	// anything is swapped. Returning an error rejects the update while the
	// running program is still completely intact.
	//
	// It must be cheap and side-effect free: the current version is still
	// running and still owns whatever the program owns. Do not bind a port, take
	// a lock, open the program's database, or write to its state directory.
	// Loading and validating configuration is the canonical implementation.
	//
	// nil means the child only has to start and reach [Updater.RunProcessRole]
	// without crashing, which is still a real test of the binary.
	Selftest func() error

	// Drain is called once denju has committed to the update, to let the program
	// quiesce - finish or abandon in-flight work, close connections, release
	// what the successor will need.
	//
	// An error aborts the update. A timeout does NOT: a program wedged in its
	// own drain must still be able to receive a fix, and the alternative is a
	// host pinned on a broken version until a human intervenes.
	Drain func(context.Context) error

	// BeforeHandoff is called after the binary has been swapped, immediately
	// before the process execs into the new image or exits for a helper to take
	// over. It cannot abort anything - the point of no return is behind it.
	//
	// It exists for the announcement a successor cannot make for itself: telling
	// a control plane that this instance is going away on purpose, so its slot
	// is released before the successor claims one. Keep it fast; the update is
	// waiting on it.
	BeforeHandoff func()

	// ServiceName is the Windows service to restart through the service control
	// manager. Empty means denju resolves it itself, which is almost always what
	// you want; set it only when the program already knows.
	//
	// Ignored on Unix, where a restart is an exec in the same process.
	ServiceName string

	// Cooldown is how long denju refuses a new update after one completes.
	// Zero, the default, disables it.
	//
	// Enable it when a control plane can command a downgrade or can re-issue an
	// update to a program that just rolled one back. Without it those two
	// situations are unbounded loops: update, roll back, be told to update
	// again, forever, re-downloading every cycle. Thirty minutes is a sensible
	// starting value.
	Cooldown time.Duration

	// DrainTimeout bounds Drain. Default 2 minutes.
	DrainTimeout time.Duration
	// SelftestTimeout bounds the selftest child. Default 30 seconds.
	SelftestTimeout time.Duration
	// HandshakeTimeout bounds the wait for the Windows helper to prove it
	// started. Default 10 seconds.
	HandshakeTimeout time.Duration
	// DownloadTimeout bounds [Source.Fetch]. Default 15 minutes.
	//
	// It is deliberately independent of the context passed to [Updater.Update]:
	// a download must not be cancelled by the same shutdown signal that the
	// update is racing.
	DownloadTimeout time.Duration
	// CrashTolerance is how many times a new image may reach startup repair
	// still uncommitted before it is rolled back as a crash loop. Default 2,
	// meaning the third such start rolls back.
	CrashTolerance int
	// StaleThreshold is how old an unresolvable journal must be before startup
	// repair treats it as garbage. Default 1 hour.
	StaleThreshold time.Duration
}

// Defaults, applied by [New] to any zero-valued field.
const (
	defaultDrainTimeout     = 2 * time.Minute
	defaultSelftestTimeout  = 30 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
	defaultDownloadTimeout  = 15 * time.Minute
	defaultCrashTolerance   = 2
	defaultStaleThreshold   = 1 * time.Hour
)

// withDefaults returns cfg with every unset tunable filled in.
func (c Config) withDefaults() Config {
	if c.DrainTimeout == 0 {
		c.DrainTimeout = defaultDrainTimeout
	}
	if c.SelftestTimeout == 0 {
		c.SelftestTimeout = defaultSelftestTimeout
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = defaultHandshakeTimeout
	}
	if c.DownloadTimeout == 0 {
		c.DownloadTimeout = defaultDownloadTimeout
	}
	if c.CrashTolerance == 0 {
		c.CrashTolerance = defaultCrashTolerance
	}
	if c.StaleThreshold == 0 {
		c.StaleThreshold = defaultStaleThreshold
	}
	return c
}

// validateNamespace enforces the character set the derived names depend on.
//
// The namespace becomes both a filename fragment and an environment variable
// name, so anything that is not a plain lowercase identifier could produce a
// path that escapes the binary's directory or a variable name a shell cannot
// set.
func validateNamespace(ns string) error {
	if ns == "" {
		return errors.New("denju: Config.Namespace is required")
	}
	for i, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return fmt.Errorf("denju: Config.Namespace %q must be lowercase letters, digits and dashes, starting with a letter or digit", ns)
		}
	}
	return nil
}

// names holds every string denju derives from the namespace.
//
// They are computed once and passed around rather than rebuilt at each use, so
// that the derivation lives in exactly one place and the naming contract test
// can pin all of it at once.
type names struct {
	// Environment variables that put a spawned copy of the binary into a role.
	envMode     string // <NS>_UPDATE_MODE
	envState    string // <NS>_UPDATE_STATE
	envSelftest string // <NS>_SELFTEST

	// Suffixes appended to the binary path.
	sufState   string // .<ns>-update.state.json
	sufRestart string // .<ns>-restart.state.json
	sufHelper  string // .<ns>-updater
	sufDiscard string // .<ns>-update.discard
	sufLog     string // .<ns>-update.log
	sufRecord  string // .<ns>-record.json

	// Patterns for os.CreateTemp, all created in the binary's own directory so
	// the rename that follows is same-filesystem and therefore atomic.
	tmpState     string // .<ns>-state-*
	tmpDownload  string // .<ns>-update-*
	tmpPermCheck string // .<ns>-permcheck-*
	tmpRecord    string // .<ns>-record-*

	// logPrefix tags every line the detached Windows helper writes, which cannot
	// go through the caller's logger.
	logPrefix string // <ns>-updater:
}

// newNames derives the naming set for a namespace.
func newNames(ns string) names {
	up := strings.ToUpper(strings.ReplaceAll(ns, "-", "_"))
	return names{
		envMode:     up + "_UPDATE_MODE",
		envState:    up + "_UPDATE_STATE",
		envSelftest: up + "_SELFTEST",

		sufState:   "." + ns + "-update.state.json",
		sufRestart: "." + ns + "-restart.state.json",
		sufHelper:  "." + ns + "-updater",
		sufDiscard: "." + ns + "-update.discard",
		sufLog:     "." + ns + "-update.log",
		sufRecord:  "." + ns + "-record.json",

		tmpState:     "." + ns + "-state-*",
		tmpDownload:  "." + ns + "-update-*",
		tmpPermCheck: "." + ns + "-permcheck-*",
		tmpRecord:    "." + ns + "-record-*",

		logPrefix: ns + "-updater: ",
	}
}

// Helper role values carried in the <NS>_UPDATE_MODE variable.
const (
	// modeApply is the full swap-and-relaunch role, spawned by the process
	// being replaced.
	modeApply = "apply"
	// modeRelaunch waits for the journal's process to exit and starts the binary
	// again. Used for a plain restart and for a rollback, neither of which can
	// exec on Windows.
	modeRelaunch = "relaunch"
)
