package denju

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Updater performs self-updates for one program.
//
// It is safe to build early - [New] touches no files - and it must be built
// before anything else in main, because the process may turn out to be an
// update helper or a selftest child rather than a normal start. See
// [Updater.RunProcessRole].
type Updater struct {
	cfg        Config
	n          names
	binaryPath string
	paths      paths
	records    *store

	// Seams. Fields rather than package vars because they close over the
	// configuration, and tests need them per-Updater.
	runSelftest func(stagedPath string, args []string, cwd string) error

	// mu guards attestCancel, which is the deadline armed by Attest. Commit and
	// Rollback cancel it, and they can arrive from any goroutine.
	mu           sync.Mutex
	attestCancel func()
}

// New validates cfg and prepares an Updater. It performs no I/O beyond resolving
// the running executable's path.
func New(cfg Config) (*Updater, error) {
	if err := validateNamespace(cfg.Namespace); err != nil {
		return nil, err
	}
	if cfg.Version == "" {
		// Startup repair distinguishes the new image from the old one by
		// comparing this against the journal's target version. Without it every
		// start after a swap looks like the old image, so a successful update
		// would be reported as an interrupted one and undone.
		return nil, errors.New("denju: Config.Version is required")
	}
	cfg = cfg.withDefaults()

	binaryPath := cfg.BinaryPath
	if binaryPath == "" {
		resolved, err := ResolveBinary()
		if err != nil {
			return nil, fmt.Errorf("denju: locate the running binary: %w", err)
		}
		binaryPath = resolved
	}

	n := newNames(cfg.Namespace)
	p := n.pathsFor(binaryPath)
	if cfg.RecordPath != "" {
		p.Record = cfg.RecordPath
	}

	u := &Updater{
		cfg:        cfg,
		n:          n,
		binaryPath: binaryPath,
		paths:      p,
		records: &store{
			path:       p.Record,
			tmpPattern: n.tmpRecord,
			cooldown:   cfg.Cooldown,
		},
	}
	u.runSelftest = func(stagedPath string, args []string, cwd string) error {
		return runSelftestChild(u.n, u.cfg.SelftestTimeout, stagedPath, args, cwd)
	}
	return u, nil
}

// ResolveBinary returns the absolute path of the running executable with
// symlinks resolved, so an update targets the real file rather than a link to
// it.
func ResolveBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// BinaryPath returns the executable denju will replace.
func (u *Updater) BinaryPath() string { return u.binaryPath }

// RunProcessRole reports whether this process was started to play a part in an
// update rather than to run the program, and if so plays it.
//
// It MUST be the first thing main does, before flags are parsed, before
// configuration is read, before anything is locked or bound. Two kinds of
// process reach it: the detached helper that performs a Windows swap, and the
// selftest child that proves a freshly downloaded binary can start. Neither may
// do any of the program's normal work.
//
// When it returns true the process has finished its role and must exit with the
// returned code. denju does not exit on its own:
//
//	if code, isRole := u.RunProcessRole(); isRole {
//		os.Exit(code)
//	}
func (u *Updater) RunProcessRole() (exitCode int, isRole bool) {
	switch {
	case u.isHelper():
		return u.runHelper(), true
	case u.isSelftest():
		return u.runSelftestRole(), true
	default:
		return 0, false
	}
}

// runSelftestRole executes the caller's selftest in a freshly downloaded binary
// and reports the verdict as an exit code.
//
// The error goes to stderr because that is what the parent process captures: the
// tail of this child's stderr becomes the human-readable reason the update was
// refused, and it is usually the only explanation anyone will see.
func (u *Updater) runSelftestRole() int {
	if u.cfg.Selftest == nil {
		// Reaching this point is already meaningful - the binary started and
		// executed the program's own entry point without crashing.
		return 0
	}
	if err := u.cfg.Selftest(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

// PendingOutcome returns an update outcome that has not yet been reported, or
// nil when there is none.
//
// Call it once the program can talk to whatever it reports to - typically right
// after connecting. The outcome usually describes an update performed by a
// PREVIOUS process, which had no way to report it because it ceased to exist.
// Call [Updater.MarkReported] once delivery is confirmed.
func (u *Updater) PendingOutcome() (*Outcome, error) { return u.records.pending() }

// MarkReported records that the pending outcome reached its destination, so it
// is not reported again. The record itself stays on disk: the cooldown still
// needs its timestamp.
func (u *Updater) MarkReported() error { return u.records.markReported() }

// InCooldown reports whether an update completed recently enough that another
// should be refused, and how long remains. It is always false when
// [Config.Cooldown] is zero.
//
// [Updater.Update] checks this itself; the method is exported so a caller can
// answer for itself without starting an update.
func (u *Updater) InCooldown(now time.Time) (bool, time.Duration, error) {
	return u.records.inCooldown(now)
}

// CleanupReported removes the update journal once its outcome has been
// delivered. The outcome record survives - it is what the cooldown reads.
//
// Call it after [Updater.MarkReported].
func (u *Updater) CleanupReported() {
	if !exists(u.paths.State) {
		return
	}
	j, err := readState(u.paths.State)
	if err != nil || (j.Phase != phaseCommitted && j.Phase != phaseRolledBack) {
		return
	}
	u.removeLeftovers()
}

// resolveServiceName reports the Windows service to restart through, preferring
// the caller's answer. An empty result means "not a service, restart directly",
// which is always the case on Unix.
func (u *Updater) resolveServiceName() (string, error) {
	if u.cfg.ServiceName != "" {
		return u.cfg.ServiceName, nil
	}
	return serviceIdentity()
}

// removeLeftovers best-effort deletes every update artifact beside the binary
// except the binary itself. The outcome record is not one of them: it outlives
// the update.
func (u *Updater) removeLeftovers() {
	_ = os.Remove(u.paths.State)
	_ = os.Remove(u.paths.Restart)
	_ = os.Remove(u.paths.RollbackBinary)
	_ = os.Remove(u.paths.HelperCopy)
	_ = os.Remove(u.paths.Discard)
	_ = os.Remove(u.paths.HelperLog)
}

// selfIsNewVersion reports whether the running process is the update's new
// image. Version strings decide it when they differ; when an update moved
// between two builds carrying the SAME version string they cannot, so it
// tiebreaks on the running binary's SHA-256.
func (u *Updater) selfIsNewVersion(j *state) bool {
	if j.TargetVersion != j.OldVersion {
		return u.cfg.Version == j.TargetVersion
	}
	sum, err := sha256File(u.binaryPath)
	if err != nil {
		return false
	}
	return sum == j.NewSHA256
}
