package denju

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// These tests drive the whole flow against REAL compiled binaries. Everything is
// genuine except the image swap itself, which cannot be performed by a process
// that has to survive to make assertions: the staged binary is really built,
// really executed as a selftest child, really hardlinked aside and really
// renamed into place, and the successor's repair and attestation really read
// what the predecessor wrote.
//
// The selftest child is the part worth having real. It is a contract between
// denju and a program's main function - "you will be started with this variable
// set, and you must exit zero" - and a mock on either side of it proves nothing.

// program is the source of a throwaway binary. It answers the selftest with
// selftestExit and otherwise prints its version.
const programSource = `package main

import (
	"fmt"
	"os"
)

func main() {
	if os.Getenv("APP_SELFTEST") == "1" {
		%s
		os.Exit(%d)
	}
	fmt.Println("%s")
}
`

// buildProgram compiles a single-file program and returns its path.
func buildProgram(t *testing.T, name, version string, selftestExit int, selftestStderr string) string {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is not on PATH; skipping the end-to-end build")
	}

	src := t.TempDir()
	stderrStmt := ""
	if selftestStderr != "" {
		stderrStmt = `fmt.Fprintln(os.Stderr, "` + selftestStderr + `")`
	}
	noErr(t, os.WriteFile(filepath.Join(src, "main.go"),
		[]byte(fmt.Sprintf(programSource, stderrStmt, selftestExit, version)), 0o600), "write main.go")
	noErr(t, os.WriteFile(filepath.Join(src, "go.mod"),
		[]byte("module e2eprog\n\ngo 1.24.0\n"), 0o600), "write go.mod")

	out := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = src
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, combined)
	}
	return out
}

// fileSource serves a binary from the local filesystem.
type fileSource struct{ path string }

func (f fileSource) Fetch(_ context.Context, _ Request, w io.Writer) error {
	src, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	_, err = io.Copy(w, src)
	return err
}

// e2eEnv is an installed program, a staged replacement for it, and the shared
// record directory both generations read.
type e2eEnv struct {
	installed string
	recordDir string
}

func newE2E(t *testing.T, v1 string) *e2eEnv {
	t.Helper()
	withGOOS(t, "linux") // exercise the swap-and-exec path on any host

	installDir := t.TempDir()
	// The installed name has to be executable on the host, extension and all -
	// these tests really run the binary before and after the swap.
	name := "prog"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	installed := filepath.Join(installDir, name)
	data, err := os.ReadFile(v1)
	noErr(t, err, "read the built binary")
	noErr(t, os.WriteFile(installed, data, 0o755), "install v1")

	return &e2eEnv{installed: installed, recordDir: t.TempDir()}
}

// updaterFor builds the Updater a given generation of the program would build
// for itself.
func (e *e2eEnv) updaterFor(t *testing.T, version string) *Updater {
	t.Helper()
	return e.updaterWith(t, version, func(*Config) {})
}

// updaterWith is updaterFor with whatever configuration a scenario adds.
func (e *e2eEnv) updaterWith(t *testing.T, version string, apply func(*Config)) *Updater {
	t.Helper()
	cfg := Config{
		Namespace:  "app",
		Version:    version,
		BinaryPath: e.installed,
		RecordPath: filepath.Join(e.recordDir, "last-update.json"),
		Cooldown:   testCooldown,
	}
	apply(&cfg)
	u, err := New(cfg)
	noErr(t, err, "New")
	return u
}

// sdkLike is the configuration the Sentinel SDK will run with.
func sdkLike(c *Config) {
	c.RetainPrevious = true
	c.CooldownScope = CooldownRolledBackVersion
}

// TestEndToEnd_UpdateAttestAndCommit walks the whole life of a successful
// update: v1 downloads v2, really runs it as a selftest child, swaps it in, and
// then v2 - a second Updater standing in for the successor process - repairs,
// attests and commits.
func TestEndToEnd_UpdateAttestAndCommit(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	v2 := buildProgram(t, "v2", "2.0.0", 0, "")
	e := newE2E(t, v1)

	v2bytes, err := os.ReadFile(v2)
	noErr(t, err, "read v2")

	execed := withStubbedExec(t)

	// --- generation one performs the update -----------------------------
	old := e.updaterFor(t, "1.0.0")
	got := old.Update(context.Background(), Request{
		ID:            "cmd-e2e",
		TargetVersion: "2.0.0",
		SHA256:        sha256Hex(v2bytes),
		TargetOS:      runtime.GOOS,
		TargetArch:    runtime.GOARCH,
	}, fileSource{path: v2})

	eq(t, got.Status, StatusSucceeded, "the update must succeed")
	isTrue(t, *execed, "the new image must be exec'd")

	installedNow, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(installedNow), sha256Hex(v2bytes), "v2 is installed")

	// The new binary must actually run, which is the whole point.
	out, err := exec.Command(e.installed).CombinedOutput()
	noErr(t, err, "run the newly installed binary")
	hasSubstr(t, string(out), "2.0.0", "the installed binary reports the new version")

	// --- generation two resolves and commits ----------------------------
	next := e.updaterFor(t, "2.0.0")
	noErr(t, next.Repair(), "Repair in the successor")

	j, err := readState(next.paths.State)
	noErr(t, err, "readState")
	eq(t, j.CrashCount, 1, "the successor's first start is counted")
	eq(t, j.Phase, phaseAttesting, "still undecided until the program says otherwise")

	noErr(t, next.Commit(), "Commit")

	j, err = readState(next.paths.State)
	noErr(t, err, "readState")
	eq(t, j.Phase, phaseCommitted, "phase")
	isFalse(t, exists(next.paths.RollbackBinary), "the rollback copy is released")

	pending, err := next.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if pending == nil {
		t.Fatal("the successor must have an outcome to report")
	}
	eq(t, pending.Status, StatusSucceeded, "reported status")
	eq(t, pending.ID, "cmd-e2e", "reported id")
	eq(t, pending.FromVersion, "1.0.0", "reported from version")
	eq(t, pending.ToVersion, "2.0.0", "reported to version")

	noErr(t, next.MarkReported(), "MarkReported")
	next.CleanupReported()
	isFalse(t, exists(next.paths.State), "the journal is cleaned up once reported")
}

// TestEndToEnd_RollbackRestoresTheOldBinary is the same walk, diverted: the
// successor decides the new version is not healthy, and the previous binary has
// to come back byte for byte.
func TestEndToEnd_RollbackRestoresTheOldBinary(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	v2 := buildProgram(t, "v2", "2.0.0", 0, "")
	e := newE2E(t, v1)

	v1bytes, err := os.ReadFile(v1)
	noErr(t, err, "read v1")
	v2bytes, err := os.ReadFile(v2)
	noErr(t, err, "read v2")

	withStubbedExec(t)

	old := e.updaterFor(t, "1.0.0")
	eq(t, old.Update(context.Background(), Request{
		ID: "cmd-e2e", TargetVersion: "2.0.0", SHA256: sha256Hex(v2bytes),
	}, fileSource{path: v2}).Status, StatusSucceeded, "the update must succeed")

	next := e.updaterFor(t, "2.0.0")
	noErr(t, next.Repair(), "Repair")
	noErr(t, next.Rollback("the health probe never passed"), "Rollback")

	restored, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(restored), sha256Hex(v1bytes), "v1 is restored byte for byte")

	out, err := exec.Command(e.installed).CombinedOutput()
	noErr(t, err, "run the restored binary")
	hasSubstr(t, string(out), "1.0.0", "the restored binary reports the old version")

	pending, err := next.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if pending == nil {
		t.Fatal("a rollback must be reportable")
	}
	eq(t, pending.Status, StatusRolledBack, "reported status")
	hasSubstr(t, pending.Error, "health probe", "reported cause")
}

// TestEndToEnd_SelftestRejectsAnUnstartableBinary is the check that earns its
// keep: a binary that cannot start is caught while the running program is still
// completely intact, at no cost.
func TestEndToEnd_SelftestRejectsAnUnstartableBinary(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	bad := buildProgram(t, "bad", "2.0.0", 1, "cannot read my configuration")
	e := newE2E(t, v1)

	v1bytes, err := os.ReadFile(v1)
	noErr(t, err, "read v1")
	badBytes, err := os.ReadFile(bad)
	noErr(t, err, "read the bad build")

	withStubbedExec(t)

	old := e.updaterFor(t, "1.0.0")
	got := old.Update(context.Background(), Request{
		ID: "cmd-e2e", TargetVersion: "2.0.0", SHA256: sha256Hex(badBytes),
	}, fileSource{path: bad})

	eq(t, got.Status, StatusFailed, "an unstartable binary must be refused")
	hasSubstr(t, got.Error, "cannot read my configuration",
		"the child's own explanation must reach the operator")

	installedNow, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(installedNow), sha256Hex(v1bytes), "the running program is untouched")

	entries, err := os.ReadDir(filepath.Dir(e.installed))
	noErr(t, err, "read the install directory")
	eq(t, len(entries), 1, "a refused update leaves nothing behind")
}

// TestEndToEnd_CrashLoopRollsItselfBack proves the last line of defence: a new
// version that starts, passes its selftest, and then dies before it can attest
// is undone without anyone intervening.
func TestEndToEnd_CrashLoopRollsItselfBack(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	v2 := buildProgram(t, "v2", "2.0.0", 0, "")
	e := newE2E(t, v1)

	v1bytes, err := os.ReadFile(v1)
	noErr(t, err, "read v1")
	v2bytes, err := os.ReadFile(v2)
	noErr(t, err, "read v2")

	withStubbedExec(t)

	old := e.updaterFor(t, "1.0.0")
	eq(t, old.Update(context.Background(), Request{
		ID: "cmd-e2e", TargetVersion: "2.0.0", SHA256: sha256Hex(v2bytes),
	}, fileSource{path: v2}).Status, StatusSucceeded, "the update must succeed")

	// Each Repair stands for one start of the new version that never got as far
	// as attesting. The tolerance is two; the third start gives up.
	for i := 1; i <= defaultCrashTolerance; i++ {
		next := e.updaterFor(t, "2.0.0")
		noErr(t, next.Repair(), "Repair")
		j, err := readState(next.paths.State)
		noErr(t, err, "readState")
		eq(t, j.CrashCount, i, "crash count")
	}

	final := e.updaterFor(t, "2.0.0")
	noErr(t, final.Repair(), "the crash-looping start")

	restored, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(restored), sha256Hex(v1bytes), "the old version is back")

	pending, err := final.PendingOutcome()
	noErr(t, err, "PendingOutcome")
	if pending == nil {
		t.Fatal("a crash-loop rollback with nothing to report is an invisible outage")
	}
	eq(t, pending.Status, StatusRolledBack, "reported status")
	hasSubstr(t, pending.Error, "crashed on startup", "reported cause")
}

// TestEndToEnd_RetainAndRollBackLocally walks the operator rollback piece C
// exists for: 1.0.0 updates to 2.0.0 and commits, keeping 1.0.0; 2.0.0 is then
// told to move back to 1.0.0 and installs it from the retained file, reading no
// byte from any network; 1.0.0 commits and now keeps 2.0.0.
func TestEndToEnd_RetainAndRollBackLocally(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	v2 := buildProgram(t, "v2", "2.0.0", 0, "")
	e := newE2E(t, v1)
	v1bytes, err := os.ReadFile(v1)
	noErr(t, err, "read v1")
	v2bytes, err := os.ReadFile(v2)
	noErr(t, err, "read v2")
	withStubbedExec(t)

	// --- 1.0.0 updates to 2.0.0 over an ordinary source --------------------
	gen1 := e.updaterWith(t, "1.0.0", sdkLike)
	eq(t, gen1.Update(context.Background(), Request{
		ID: "cmd-up", TargetVersion: "2.0.0", SHA256: sha256Hex(v2bytes),
	}, fileSource{path: v2}).Status, StatusSucceeded, "the update to 2.0.0")

	// --- 2.0.0 commits, reports and cleans up ------------------------------
	gen2 := e.updaterWith(t, "2.0.0", sdkLike)
	noErr(t, gen2.Repair(), "Repair")
	noErr(t, gen2.Commit(), "Commit")
	noErr(t, gen2.MarkReported(), "MarkReported")
	gen2.CleanupReported()
	isFalse(t, exists(gen2.paths.State), "the journal is cleaned up")
	isFalse(t, exists(gen2.paths.RollbackBinary), "no rollback copy lingers")

	prevPath, prevSum, prevVersion, ok := gen2.PreviousBinary()
	isTrue(t, ok, "1.0.0 is retained")
	eq(t, prevVersion, "1.0.0", "retained version")
	eq(t, prevSum, sha256Hex(v1bytes), "retained digest")

	// --- the move back to 1.0.0 is served from the retained file -----------
	got := gen2.Update(context.Background(), Request{
		ID: "cmd-back", TargetVersion: prevVersion, SHA256: prevSum,
	}, FileSource(prevPath))
	eq(t, got.Status, StatusSucceeded, "the local rollback")

	installed, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(installed), sha256Hex(v1bytes), "1.0.0 is installed byte for byte")
	out, err := exec.Command(e.installed).CombinedOutput()
	noErr(t, err, "run the reinstalled binary")
	hasSubstr(t, string(out), "1.0.0", "it reports 1.0.0")

	// --- 1.0.0 commits; what it replaced is now the retained one -----------
	gen3 := e.updaterWith(t, "1.0.0", sdkLike)
	noErr(t, gen3.Repair(), "Repair")
	noErr(t, gen3.Commit(), "Commit")
	noErr(t, gen3.MarkReported(), "MarkReported")
	gen3.CleanupReported()

	path, sum, version, ok := gen3.PreviousBinary()
	isTrue(t, ok, "2.0.0 is retained")
	eq(t, version, "2.0.0", "retained version")
	eq(t, sum, sha256Hex(v2bytes), "retained digest")
	retained, err := os.ReadFile(path)
	noErr(t, err, "read the retained binary")
	eq(t, sha256Hex(retained), sha256Hex(v2bytes), "the retained file is what its record says")
}

// TestEndToEnd_RolledBackVersionIsHeldAndAFixIsAdmitted: a version rolled back
// by the program itself is not re-applied, and the corrective version lands at
// once.
func TestEndToEnd_RolledBackVersionIsHeldAndAFixIsAdmitted(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	v2 := buildProgram(t, "v2", "2.0.0", 0, "")
	v3 := buildProgram(t, "v3", "2.0.1", 0, "")
	e := newE2E(t, v1)
	v2bytes, err := os.ReadFile(v2)
	noErr(t, err, "read v2")
	v3bytes, err := os.ReadFile(v3)
	noErr(t, err, "read v3")
	withStubbedExec(t)

	gen1 := e.updaterWith(t, "1.0.0", sdkLike)
	eq(t, gen1.Update(context.Background(), Request{
		ID: "cmd-bad", TargetVersion: "2.0.0", SHA256: sha256Hex(v2bytes),
	}, fileSource{path: v2}).Status, StatusSucceeded, "the update to 2.0.0")

	bad := e.updaterWith(t, "2.0.0", sdkLike)
	noErr(t, bad.Repair(), "Repair")
	noErr(t, bad.Rollback("the soak failed"), "Rollback")

	back := e.updaterWith(t, "1.0.0", sdkLike)
	noErr(t, back.Repair(), "Repair")
	noErr(t, back.MarkReported(), "MarkReported")
	back.CleanupReported()

	again := back.Update(context.Background(), Request{
		ID: "cmd-again", TargetVersion: "2.0.0", SHA256: sha256Hex(v2bytes),
	}, fileSource{path: v2})
	eq(t, again.Status, StatusRefusedCooldown, "the rolled-back version is held")
	isTrue(t, again.ProgramIntact, "and nothing was disturbed")

	fixed := back.Update(context.Background(), Request{
		ID: "cmd-fix", TargetVersion: "2.0.1", SHA256: sha256Hex(v3bytes),
	}, fileSource{path: v3})
	eq(t, fixed.Status, StatusSucceeded, "the fix is admitted at once")
	installed, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(installed), sha256Hex(v3bytes), "2.0.1 is installed")
}

// breakingSource serves a real binary from req.Offset and breaks the stream
// once, part way through - the shape of a customer link dropping mid-transfer.
type breakingSource struct {
	path    string
	breakAt int64
	broke   bool
	offsets []int64
}

func (s *breakingSource) ResumesFromOffset() {}

func (s *breakingSource) Fetch(ctx context.Context, req Request, w io.Writer) error {
	s.offsets = append(s.offsets, req.Offset)
	if s.broke {
		return FileSource(s.path).Fetch(ctx, req, w)
	}
	s.broke = true
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.CopyN(w, f, s.breakAt); err != nil {
		return err
	}
	return errors.New("connection reset by peer")
}

// TestEndToEnd_InterruptedDownloadResumes: the first attempt loses the link half
// way, keeps what arrived and leaves the program untouched; a later process
// retrying the same request asks only for the rest, and the resumed file really
// runs as a selftest child and really installs.
func TestEndToEnd_InterruptedDownloadResumes(t *testing.T) {
	v1 := buildProgram(t, "v1", "1.0.0", 0, "")
	v2 := buildProgram(t, "v2", "2.0.0", 0, "")
	e := newE2E(t, v1)
	v1bytes, err := os.ReadFile(v1)
	noErr(t, err, "read v1")
	v2bytes, err := os.ReadFile(v2)
	noErr(t, err, "read v2")
	withStubbedExec(t)

	half := int64(len(v2bytes) / 2)
	src := &breakingSource{path: v2, breakAt: half}
	req := Request{
		ID: "cmd-resume", TargetVersion: "2.0.0", SHA256: sha256Hex(v2bytes),
		TargetOS: runtime.GOOS, TargetArch: runtime.GOARCH,
	}

	// The SDK configuration: a failed attempt must not hold the retry, which
	// the default any-update cooldown would.
	first := e.updaterWith(t, "1.0.0", sdkLike).Update(context.Background(), req, src)
	eq(t, first.Status, StatusFailed, "the interrupted attempt fails")
	isTrue(t, first.ProgramIntact, "and leaves the program intact")
	isTrue(t, errors.Is(first.Cause, ErrDownloadFailed), "as a download failure")

	installed, err := os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(installed), sha256Hex(v1bytes), "1.0.0 is untouched")

	retry := e.updaterWith(t, "1.0.0", sdkLike)
	partial := retry.partialPath(req)
	info, err := os.Stat(partial)
	noErr(t, err, "the partial survives into a later process")
	eq(t, info.Size(), half, "it holds exactly what arrived")

	second := retry.Update(context.Background(), req, src)
	eq(t, second.Status, StatusSucceeded, "the retry resumes and installs")
	if !slices.Equal(src.offsets, []int64{0, half}) {
		t.Errorf("offsets asked for = %v, want [0 %d]", src.offsets, half)
	}
	isFalse(t, exists(partial), "the completed partial became the installed binary")

	installed, err = os.ReadFile(e.installed)
	noErr(t, err, "read the installed binary")
	eq(t, sha256Hex(installed), sha256Hex(v2bytes), "2.0.0 is installed byte for byte")
	out, err := exec.Command(e.installed).CombinedOutput()
	noErr(t, err, "run the resumed binary")
	hasSubstr(t, string(out), "2.0.0", "it reports 2.0.0")

	gen2 := e.updaterWith(t, "2.0.0", sdkLike)
	noErr(t, gen2.Repair(), "Repair")
	noErr(t, gen2.Commit(), "Commit")
}
