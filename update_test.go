package denju

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource hands back canned bytes, or an error.
type fakeSource struct {
	payload []byte
	err     error
	calls   int
}

func (f *fakeSource) Fetch(_ context.Context, _ Request, w io.Writer) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	_, err := w.Write(f.payload)
	return err
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// updateRig is a fully seamed Updater on a throwaway binary: a real file
// standing in for the installed program, a canned download, and a faked image
// swap. It exists so Update can be driven end to end - most of what matters
// here is the ORDER the stages run in, which no direct call to an internal
// helper can observe.
type updateRig struct {
	u       *Updater
	bin     string
	dir     string
	payload []byte
	sha     string
	src     *fakeSource
	log     *captureLog

	// A Drain callback that overruns DrainTimeout is abandoned but keeps
	// running, so it records steps concurrently with the swap and the handoff
	// on the main goroutine. That is the library behaving as designed - see
	// Updater.drain - which makes the recorder itself the thing that has to be
	// safe.
	mu    sync.Mutex
	steps []string
	exits []int
}

func (r *updateRig) step(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, s)
}

func (r *updateRig) exit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exits = append(r.exits, code)
}

func (r *updateRig) stepsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.steps)
}

func (r *updateRig) exitsSnapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.exits)
}

func newUpdateRig(t *testing.T, apply ...func(*Config)) *updateRig {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "current")

	payload := []byte("this is the new program binary")
	log := &captureLog{}
	rig := &updateRig{
		bin: bin, dir: dir,
		payload: payload, sha: sha256Hex(payload),
		src: &fakeSource{payload: payload},
		log: log,
	}

	prevExec := execSelf
	execSelf = func(string, []string, []string) error {
		rig.step("exec")
		return nil
	}
	t.Cleanup(func() { execSelf = prevExec })

	prevExit := osExit
	osExit = func(code int) { rig.exit(code) }
	t.Cleanup(func() { osExit = prevExit })

	prevIdentity := serviceIdentity
	serviceIdentity = func() (string, error) { return "", nil }
	t.Cleanup(func() { serviceIdentity = prevIdentity })

	cfg := Config{
		Namespace:  "app",
		Version:    "1.4.0",
		BinaryPath: bin,
		RecordPath: filepath.Join(t.TempDir(), "last-update.json"),
		Log:        log.Logger(),
	}
	for _, fn := range apply {
		fn(&cfg)
	}
	u, err := New(cfg)
	noErr(t, err, "New")

	u.runSelftest = func(string, []string, string) error {
		rig.step("selftest")
		return nil
	}

	rig.u = u
	return rig
}

// req builds the request this rig's source satisfies.
func (r *updateRig) req() Request {
	return Request{
		ID:            "cmd-1",
		TargetVersion: "1.5.0",
		SHA256:        r.sha,
		TargetOS:      runtime.GOOS,
		TargetArch:    runtime.GOARCH,
	}
}

func (r *updateRig) binaryContent(t *testing.T) string {
	t.Helper()
	return readFileString(t, r.bin)
}

// extraFiles counts everything in the install directory except the binary. An
// aborted update has to be indistinguishable from no update, and a leftover
// journal or staged download is how that guarantee gets quietly broken.
func (r *updateRig) extraFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(r.dir)
	noErr(t, err, "read the install directory")
	n := 0
	for _, e := range entries {
		if filepath.Join(r.dir, e.Name()) != r.bin {
			n++
			t.Logf("leftover: %s", e.Name())
		}
	}
	return n
}

func TestUpdate_IncompleteRequestIsRefused(t *testing.T) {
	r := newUpdateRig(t)

	got := r.u.Update(context.Background(), Request{ID: "cmd-1"}, r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "incomplete", "error")
	eq(t, r.src.calls, 0, "nothing may be downloaded for an unusable request")
}

// Installing a binary for the wrong platform is not recoverable in place, so the
// check happens before anything is fetched.
func TestUpdate_PlatformMismatchIsRefused(t *testing.T) {
	r := newUpdateRig(t)
	req := r.req()
	req.TargetOS = "plan9"

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "plan9", "error names the requested platform")
	eq(t, r.src.calls, 0, "nothing may be downloaded for the wrong platform")
	eq(t, r.binaryContent(t), "current", "the binary is untouched")
}

func TestUpdate_ArchMismatchIsRefused(t *testing.T) {
	r := newUpdateRig(t)
	req := r.req()
	req.TargetArch = "mips"

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusFailed, "status")
	eq(t, r.src.calls, 0, "nothing may be downloaded for the wrong architecture")
}

// An empty TargetOS/TargetArch skips the guard, for callers whose control plane
// does not carry the information.
func TestUpdate_UnsetPlatformSkipsTheGuard(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)
	req := r.req()
	req.TargetOS, req.TargetArch = "", ""

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusSucceeded, "status")
}

func TestUpdate_SameVersionIsRefused(t *testing.T) {
	r := newUpdateRig(t)
	req := r.req()
	req.TargetVersion = "1.4.0" // the running version

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "already running", "error")
	eq(t, r.src.calls, 0, "nothing may be downloaded to install what is already running")
}

func TestUpdate_DownloadErrorLeavesProgramUntouched(t *testing.T) {
	r := newUpdateRig(t)
	r.src.err = errors.New("the connection dropped")

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "the connection dropped", "error")
	eq(t, r.binaryContent(t), "current", "the binary is untouched")
	eq(t, r.extraFiles(t), 0, "a failed download leaves nothing behind")
}

// The digest is the only integrity check denju makes, so it has to be the one
// thing that cannot be skipped.
func TestUpdate_ChecksumMismatchIsRefused(t *testing.T) {
	r := newUpdateRig(t)
	req := r.req()
	req.SHA256 = sha256Hex([]byte("something else entirely"))

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "checksum", "error")
	eq(t, r.binaryContent(t), "current", "the binary is untouched")
	eq(t, r.extraFiles(t), 0, "a corrupt download leaves nothing behind")
}

// A digest is a digest whichever case it arrives in.
func TestUpdate_ChecksumIsCaseInsensitive(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)
	req := r.req()
	req.SHA256 = strings.ToUpper(req.SHA256)

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusSucceeded, "status")
}

// The selftest is the last free check: the running program is still intact, so
// a binary that cannot start is simply deleted.
func TestUpdate_SelftestFailureLeavesProgramUntouched(t *testing.T) {
	r := newUpdateRig(t)
	r.u.runSelftest = func(string, []string, string) error {
		return errors.New("update selftest failed: config is invalid")
	}

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "config is invalid", "error")
	eq(t, r.binaryContent(t), "current", "the binary is untouched")
	eq(t, r.extraFiles(t), 0, "a rejected binary leaves nothing behind")
}

// The Unix happy path: swap, journal at attesting, exec in place.
func TestUpdate_HappyPathSwapsAndExecs(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusSucceeded, "status")
	eq(t, r.binaryContent(t), string(r.payload), "the new binary is installed")
	eq(t, readFileString(t, r.u.paths.RollbackBinary), "current", "the rollback copy holds the old binary")

	j, err := readState(r.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseAttesting, "phase")
	eq(t, j.CommandID, "cmd-1", "command id")
	eq(t, j.TargetVersion, "1.5.0", "target version")
	eq(t, j.OldVersion, "1.4.0", "old version")
	eq(t, j.NewSHA256, r.sha, "new digest")
	isTrue(t, j.OldSHA256 != "", "the old digest is recorded for identity checks")
}

// If the exec fails the program is still the old image, so the swap is undone
// with a single atomic rename and the restored binary is exec'd instead.
func TestUpdate_ExecFailureRollsBackAndExecsTheOldBinary(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)

	var execs int
	prev := execSelf
	execSelf = func(string, []string, []string) error {
		execs++
		if execs == 1 {
			return errors.New("exec format error")
		}
		return nil
	}
	t.Cleanup(func() { execSelf = prev })

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusRolledBack, "status")
	hasSubstr(t, got.Error, "exec failed", "error")
	eq(t, execs, 2, "the restored old binary must be exec'd too")
	eq(t, r.binaryContent(t), "current", "the old binary is back in place")

	j, err := readState(r.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseRolledBack, "phase")
}

// BeforeHandoff is the caller's last word before the process ceases to be
// itself. It must run after the swap - so the successor is already on disk - and
// before the exec.
func TestUpdate_BeforeHandoffRunsAfterTheSwapAndBeforeTheExec(t *testing.T) {
	withGOOS(t, "linux")
	var contentAtHandoff string
	r := newUpdateRig(t, func(c *Config) {
		c.BeforeHandoff = func() {}
	})
	r.u.cfg.BeforeHandoff = func() {
		r.step("handoff")
		contentAtHandoff = r.binaryContent(t)
	}

	eq(t, r.u.Update(context.Background(), r.req(), r.src).Status, StatusSucceeded, "status")
	steps := r.stepsSnapshot()
	eq(t, len(steps), 3, "steps")
	eq(t, steps[0], "selftest", "steps[0]")
	eq(t, steps[1], "handoff", "steps[1]")
	eq(t, steps[2], "exec", "steps[2]")
	eq(t, contentAtHandoff, string(r.payload), "the new binary must already be installed")
}

// A panic in BeforeHandoff must not strand a swapped binary with nothing running
// it: the point of no return is already behind us.
func TestUpdate_BeforeHandoffPanicDoesNotStopTheHandover(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)
	r.u.cfg.BeforeHandoff = func() { panic("callback is broken") }

	eq(t, r.u.Update(context.Background(), r.req(), r.src).Status, StatusSucceeded, "status")
	eq(t, r.binaryContent(t), string(r.payload), "the new binary is installed")
	isTrue(t, r.log.contains("BeforeHandoff panicked"), "the panic must be reported")
}

// An update nobody asked for by id has nothing to report against, so no record
// is written - but it still happens.
func TestUpdate_NoRequestIDRecordsNothing(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)
	req := r.req()
	req.ID = ""

	eq(t, r.u.Update(context.Background(), req, r.src).Status, StatusSucceeded, "status")

	rec, err := r.u.records.load()
	noErr(t, err, "load the record")
	if rec != nil {
		t.Fatalf("expected no record, got %+v", rec)
	}
}

// ---------------------------------------------------------------- drain

// An error from the drain callback aborts the update outright. The program said
// it could not quiesce, so replacing its binary underneath it is the one thing
// not to do - and because nothing has been touched yet, aborting is free.
func TestUpdate_DrainErrorAbortsTheUpdate(t *testing.T) {
	withGOOS(t, "linux")
	var r *updateRig
	r = newUpdateRig(t, func(c *Config) {
		c.Drain = func(context.Context) error {
			r.step("drain")
			return errors.New("a job refused to stop")
		}
	})

	got := r.u.Update(context.Background(), r.req(), r.src)

	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "prepare-for-update failed", "error")
	hasSubstr(t, got.Error, "a job refused to stop", "error")

	steps := r.stepsSnapshot()
	eq(t, len(steps), 2, "steps")
	eq(t, steps[0], "selftest", "steps[0]")
	eq(t, steps[1], "drain", "steps[1]")
	eq(t, r.binaryContent(t), "current", "the binary must be untouched")
	eq(t, r.extraFiles(t), 0, "an aborted update has to be indistinguishable from no update")
}

// A drain that overruns does NOT abort. This is the surprising half of the
// contract and the one most likely to be "tidied up" into an abort by a future
// reader: a program wedged on stuck work must still be able to receive the fix
// for whatever wedged it. Abandoned work is usually recoverable; a blocked
// update needs a human on the host.
func TestUpdate_DrainTimeoutProceedsAnyway(t *testing.T) {
	withGOOS(t, "linux")
	drainReturned := make(chan struct{})

	var r *updateRig
	r = newUpdateRig(t, func(c *Config) {
		c.DrainTimeout = 50 * time.Millisecond
		c.Drain = func(ctx context.Context) error {
			r.step("drain")
			<-ctx.Done() // never finishes on its own
			close(drainReturned)
			return ctx.Err()
		}
	})

	got := r.u.Update(context.Background(), r.req(), r.src)

	eq(t, got.Status, StatusSucceeded, "status")
	steps := r.stepsSnapshot()
	eq(t, len(steps), 3, "an overrunning drain must be abandoned, not allowed to block the swap")
	eq(t, steps[2], "exec", "steps[2]")
	eq(t, r.binaryContent(t), string(r.payload), "the new binary must have been swapped in")

	// The callback's own error, arriving after the deadline, must be ignored
	// rather than racing the swap into a rollback.
	select {
	case <-drainReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain callback was never released by its context")
	}
}

// Drain runs after the selftest and before the swap. The ordering is the point:
// everything that can fail cheaply happens while the program is still fully
// serving, and it only stops serving once the update is certain to go ahead.
// Asserted from inside the callback, where the on-disk state at that exact
// moment is observable.
func TestUpdate_DrainRunsAfterSelftestAndBeforeTheSwap(t *testing.T) {
	withGOOS(t, "linux")
	var atDrain string
	var rollbackExisted bool

	var r *updateRig
	r = newUpdateRig(t, func(c *Config) {
		c.Drain = func(context.Context) error {
			r.step("drain")
			atDrain = r.binaryContent(t)
			rollbackExisted = exists(r.u.paths.RollbackBinary)
			return nil
		}
	})

	eq(t, r.u.Update(context.Background(), r.req(), r.src).Status, StatusSucceeded, "status")

	steps := r.stepsSnapshot()
	eq(t, len(steps), 3, "steps")
	eq(t, steps[0], "selftest", "steps[0]")
	eq(t, steps[1], "drain", "steps[1]")
	eq(t, steps[2], "exec", "steps[2]")
	eq(t, atDrain, "current", "the binary must still be the old one while the program is draining")
	isFalse(t, rollbackExisted, "nothing may be linked aside before the drain has finished")
	eq(t, r.binaryContent(t), string(r.payload), "the new binary is installed afterwards")
}

// A drain that finishes normally is not given a cancelled context - a callback
// that treats ctx.Err() as "abandon ship" would otherwise cancel work it was
// meant to finish gracefully.
func TestUpdate_DrainContextIsLiveWhileItRuns(t *testing.T) {
	withGOOS(t, "linux")
	var errAtEntry error
	var deadlineSet bool

	r := newUpdateRig(t, func(c *Config) {
		c.Drain = func(ctx context.Context) error {
			errAtEntry = ctx.Err()
			_, deadlineSet = ctx.Deadline()
			return nil
		}
	})

	eq(t, r.u.Update(context.Background(), r.req(), r.src).Status, StatusSucceeded, "status")
	noErr(t, errAtEntry, "the drain context must be live when the callback starts")
	isTrue(t, deadlineSet, "the drain must be bounded by DrainTimeout")
}

// ---------------------------------------------------------------- cooldown

// The cooldown must not be bypassable by simply asking again. A refusal is
// recorded so it can be reported, and recording it must leave the anchor where
// the completed update put it.
func TestUpdate_CooldownRefusalIsNotBypassableByRetrying(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t, func(c *Config) { c.Cooldown = testCooldown })
	noErr(t, r.u.records.record(&Outcome{
		At: time.Now().Add(-time.Minute), ID: "cmd-0",
		FromVersion: "1.3.0", ToVersion: "1.4.0", Status: StatusSucceeded,
	}), "seed a just-completed update")

	first := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, first.Status, StatusRefusedCooldown, "first attempt")

	second := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, second.Status, StatusRefusedCooldown, "retrying must not get through")

	eq(t, r.src.calls, 0, "a refused update must not download anything")
	eq(t, r.binaryContent(t), "current", "the binary is untouched")
}

func TestUpdate_ExpiredCooldownNoLongerRefuses(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t, func(c *Config) { c.Cooldown = testCooldown })
	noErr(t, r.u.records.record(&Outcome{
		At: time.Now().Add(-testCooldown - time.Minute), ID: "cmd-0",
		FromVersion: "1.3.0", ToVersion: "1.4.0", Status: StatusSucceeded,
	}), "seed an old update")

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusSucceeded, "an expired cooldown holds nothing back")
}

// The default is no cooldown at all, so a program that never asked for one
// behaves exactly as it did before.
func TestUpdate_CooldownDisabledByDefault(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t)
	noErr(t, r.u.records.record(&Outcome{
		At: time.Now(), ID: "cmd-0", Status: StatusSucceeded,
	}), "seed a just-completed update")

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusSucceeded, "no cooldown configured, no refusal")
}

// ---------------------------------------------------------------- windows

// handshakingSpawn stands in for the detached helper: it records the spawn and
// writes the handshake PID the original process waits for.
func handshakingSpawn(t *testing.T, u *Updater, seams *helperSeams, handshake bool) {
	t.Helper()
	prev := spawnDetached
	spawnDetached = func(path string, args []string, cwd string, env []string) (int, error) {
		seams.spawns = append(seams.spawns, spawnCall{path: path, args: args, cwd: cwd, env: env})
		if seams.spawnErr != nil {
			return 0, seams.spawnErr
		}
		if handshake {
			j, err := readState(u.paths.State)
			if err == nil {
				j.HelperPID = 7777
				_ = writeState(u.n, u.paths.State, j)
			}
		}
		return 7777, nil
	}
	t.Cleanup(func() { spawnDetached = prev })
}

// The Windows path stages the update, hands off to the helper, and exits zero so
// the helper's wait can complete.
func TestUpdate_WindowsHandoffExitsZero(t *testing.T) {
	withGOOS(t, "windows")
	r := newUpdateRig(t)
	seams := &helperSeams{}
	handshakingSpawn(t, r.u, seams, true)

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusSucceeded, "status")
	exits := r.exitsSnapshot()
	eq(t, len(exits), 1, "the process must exit for the helper to take over")
	eq(t, exits[0], 0, "exit code")

	eq(t, len(seams.spawns), 1, "the helper must be spawned")
	eq(t, seams.spawns[0].path, r.u.paths.HelperCopy, "the helper is a copy of the binary")

	j, err := readState(r.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseStaged, "the helper does the swap, so the journal is left staged")
	isTrue(t, exists(r.u.paths.HelperCopy), "the helper copy must be on disk")
	eq(t, r.binaryContent(t), "current", "the original process must not swap anything itself")
}

// A helper that never proves it started means the handoff has no successor, so
// the update is abandoned while the program is still serving.
func TestUpdate_WindowsHandshakeTimeoutAborts(t *testing.T) {
	withGOOS(t, "windows")
	r := newUpdateRig(t, func(c *Config) { c.HandshakeTimeout = 20 * time.Millisecond })
	seams := &helperSeams{}
	handshakingSpawn(t, r.u, seams, false) // never writes the handshake

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "did not start", "error")
	eq(t, len(r.exitsSnapshot()), 0, "the program must keep running")
	eq(t, r.binaryContent(t), "current", "the binary is untouched")
	eq(t, r.extraFiles(t), 0, "an aborted handoff cleans up after itself")
}

// A service-hosted program whose service name cannot be resolved has no way
// back, so it must abort before anything is disturbed rather than after its
// binary is gone.
func TestUpdate_WindowsServiceIdentityErrorAborts(t *testing.T) {
	withGOOS(t, "windows")
	r := newUpdateRig(t)
	prev := serviceIdentity
	serviceIdentity = func() (string, error) { return "", errors.New("no matching service for pid") }
	t.Cleanup(func() { serviceIdentity = prev })

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "could not determine how to restart", "error")
	eq(t, r.src.calls, 0, "nothing may be downloaded when there is no way to restart")
	eq(t, r.extraFiles(t), 0, "nothing is left behind")
}

// A drain error on Windows arrives AFTER the helper has been spawned, so the
// abort has to kill the helper and reclaim everything.
func TestUpdate_WindowsDrainErrorAbortsAndCleansUp(t *testing.T) {
	withGOOS(t, "windows")
	var r *updateRig
	r = newUpdateRig(t, func(c *Config) {
		c.Drain = func(context.Context) error { return errors.New("could not quiesce") }
	})
	seams := &helperSeams{}
	handshakingSpawn(t, r.u, seams, true)

	got := r.u.Update(context.Background(), r.req(), r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "could not quiesce", "error")
	eq(t, len(r.exitsSnapshot()), 0, "the program must keep running")
	eq(t, r.extraFiles(t), 0, "the journal, helper copy and staged download are all reclaimed")
}

// The caller's ServiceName wins over discovery, for a program that already knows
// how it is hosted.
func TestUpdate_ConfiguredServiceNameIsUsed(t *testing.T) {
	withGOOS(t, "windows")
	r := newUpdateRig(t, func(c *Config) { c.ServiceName = "my-service" })
	prev := serviceIdentity
	serviceIdentity = func() (string, error) { return "", errors.New("discovery must not be consulted") }
	t.Cleanup(func() { serviceIdentity = prev })
	seams := &helperSeams{}
	handshakingSpawn(t, r.u, seams, true)

	eq(t, r.u.Update(context.Background(), r.req(), r.src).Status, StatusSucceeded, "status")

	j, err := readState(r.u.paths.State)
	noErr(t, err, "readState")
	eq(t, j.ServiceName, "my-service", "the configured service name is journaled")
}
