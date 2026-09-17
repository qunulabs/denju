package denju

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CooldownScope selects what [Config.Cooldown] holds back.
type CooldownScope int

const (
	// CooldownAnyUpdate refuses every update for Cooldown after one completes -
	// succeeded, failed or rolled back. The default, and the only behaviour
	// before scopes existed. It also stops a control plane flip-flopping a
	// program between two versions that both work.
	CooldownAnyUpdate CooldownScope = iota
	// CooldownRolledBackVersion refuses only the version most recently rolled
	// back, for Cooldown after that rollback, and admits every other target at
	// once. It closes the update-rollback-update loop without also blocking the
	// corrective update that should follow a bad one, or a move back to the
	// version the program just left.
	//
	// Versions are matched by string.
	//
	// Under this scope every write to the outcome record reads the previous
	// record first, so that a record that cannot be parsed never silently drops
	// the hold. The consequence of a corrupt record is severe, and is accepted
	// because only damage to the disk produces one: every write fails, so every
	// later update is refused - the corrective one included - and nothing can be
	// recorded or reported. A [Updater.Commit] is refused as well, which leaves the
	// update undecided: the new version keeps running, but every start of it
	// counts against [Config.CrashTolerance], the one that attested included, and
	// on start CrashTolerance+1 startup repair rolls back a version that was
	// healthy. Nothing remote can recover the host; the record has to be removed
	// or repaired on it.
	//
	// The hold needs denju v0.4.0 or later with this scope on BOTH images of an
	// update: the image rolled back FROM normally writes it, and the image rolled
	// back TO enforces it and carries it forward. See the README.
	//
	// It needs a positive Cooldown: [New] rejects it with a zero or negative one.
	CooldownRolledBackVersion
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
	//
	// It must be safe for concurrent use: denju logs from the attestation
	// deadline goroutine and from an abandoned Drain as well as from the
	// caller's own goroutine. [SlogLogger] already is; a closure appending to a
	// bare slice is not.
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
	//
	// That has a consequence worth designing for. A callback that overruns
	// DrainTimeout is abandoned, not stopped - it keeps running, and from that
	// moment it is CONCURRENT with the swap, with BeforeHandoff, and with the
	// handover itself. So it has to be safe to run alongside them: treat ctx
	// being cancelled as the signal to stop touching shared state, and do not
	// assume the program still owns the binary that is on disk.
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
	//
	// [Config.CooldownScope] decides what is held: every update (the default) or
	// only the version most recently rolled back.
	Cooldown time.Duration

	// CooldownScope is what Cooldown holds back. The zero value,
	// [CooldownAnyUpdate], is the behaviour Cooldown has always had.
	// [CooldownRolledBackVersion] requires a positive Cooldown.
	CooldownScope CooldownScope

	// RetainPrevious keeps the binary an update replaced once that update is
	// committed, as <binary>.<namespace>-previous, instead of deleting it.
	// [Updater.PreviousBinary] reports it and [FileSource] installs it, so moving
	// back to the version the program just left reads nothing from any network.
	//
	// One generation is kept: the next committed update replaces it. A rollback
	// does not touch it - the program is back on the binary the update replaced,
	// and the retained one is still the one before that. It costs one binary's
	// worth of disk for good. The rollback copy is a hardlink on Unix and costs
	// nothing extra, so the peak while an update is in flight is the running
	// binary, the download in progress and the retained one, plus a full copy of
	// the binary for the detached helper on Windows.
	//
	// Never execute the retained file in place. On Windows a running .exe cannot
	// be replaced, so the next commit could not retain over it.
	//
	// Turning it off later does not remove a binary already retained, and
	// [Updater.PreviousBinary] still reports it.
	RetainPrevious bool

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
	// PartialRetention bounds how long an interrupted download is kept for a
	// retry to resume. A partial nothing has written to for longer is deleted,
	// by the next download or by [Updater.Repair] on a start with no update in
	// flight. Default 24 hours.
	//
	// Only a [ResumableSource] ever leaves a partial behind.
	PartialRetention time.Duration
}

// Defaults, applied by [New] to any zero-valued field.
const (
	defaultDrainTimeout     = 2 * time.Minute
	defaultSelftestTimeout  = 30 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
	defaultDownloadTimeout  = 15 * time.Minute
	defaultCrashTolerance   = 2
	defaultStaleThreshold   = 1 * time.Hour
	defaultPartialRetention = 24 * time.Hour
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
	if c.PartialRetention == 0 {
		c.PartialRetention = defaultPartialRetention
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

	// The binary kept by Config.RetainPrevious, and the record describing it.
	sufPrevious       string // .<ns>-previous
	sufPreviousRecord string // .<ns>-previous.json

	// A partial download is <binary><prePartial><key><extPartial>, where key
	// is derived from the request (partialKey). Only a ResumableSource leaves one.
	prePartial string // .<ns>-download-
	extPartial string // .partial

	// Patterns for os.CreateTemp, all created in the binary's own directory so
	// the rename that follows is same-filesystem and therefore atomic.
	tmpState     string // .<ns>-state-*
	tmpDownload  string // .<ns>-update-*
	tmpPermCheck string // .<ns>-permcheck-*
	tmpRecord    string // .<ns>-record-*
	tmpPrevious  string // .<ns>-previous-*

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

		sufPrevious:       "." + ns + "-previous",
		sufPreviousRecord: "." + ns + "-previous.json",

		prePartial: "." + ns + "-download-",
		extPartial: ".partial",

		tmpState:     "." + ns + "-state-*",
		tmpDownload:  "." + ns + "-update-*",
		tmpPermCheck: "." + ns + "-permcheck-*",
		tmpRecord:    "." + ns + "-record-*",
		tmpPrevious:  "." + ns + "-previous-*",

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
