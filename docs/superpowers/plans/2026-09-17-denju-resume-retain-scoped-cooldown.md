# denju v0.4.0 — resumable downloads, retained previous binary, scoped cooldown — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Implementers NEVER stage, commit, stash, checkout or restore.** Every task ends with "Stop: controller reviews; do not stage or commit". Because nothing is committed between tasks, `git checkout -- <file>`, `git restore` and `git stash` would destroy earlier tasks' work: **revert every mutation by editing the code back by hand**, then re-run the test to confirm green.

**Goal:** Ship denju v0.4.0, an additive minor that lets the Sentinel SDK resume an interrupted update download from an offset, keep the replaced binary after a commit and reinstall it with zero network bytes, and hold back only the version it just rolled back.

**Architecture:** Five additive features in the one core package, each off unless the caller opts in: (1) a typed `Result.Cause` so a caller can tell failure kinds apart without parsing text; (2) a `ResumableSource` marker interface plus `Request.Offset`, driving a deterministic, request-keyed partial file that survives a failed `Fetch`; (3) `FileSource`, a resumable local-file source; (4) `Config.RetainPrevious`, which turns the rollback copy into `<binary>.<ns>-previous` plus a JSON record on commit, reported by `Updater.PreviousBinary`; (5) `Config.CooldownScope`, whose new `CooldownRolledBackVersion` value holds only the most recently rolled-back version, carried forward in the outcome record. `httpsource` is untouched.

**Tech Stack:** Go (module floor 1.24, `go 1.24.0` in go.mod), stdlib only (`golang.org/x/sys` remains the single dependency, Windows only), `testing` with the in-repo helpers in `helpers_test.go`.

**Spec:**
- Piece C — `/home/qnu/Desktop/Workspace/QNu/sentinel-drm/sentinel-drm-go-sdk/docs/superpowers/specs/2026-09-11-client-rollback-design.md` §2, §5, §8, §10 (§10 wins: retain ONE previous binary; soak numbers are SDK-side).
- Piece D — `/home/qnu/Desktop/Workspace/QNu/sentinel-drm/sentinel-drm-heartbeat-service/docs/superpowers/specs/2026-09-11-unified-delivery-design.md` §3 (resume lives in denju; deterministic partial named from id + target version + sha; keep on transfer error; delete on checksum mismatch, on success, and past a retention window; denju stats the partial, replays SHA-256, passes the offset in `Request`), §9 item 1 (`offset` on `denju.Request`), §9 item 8c (`offset == total_size` → header only; `offset > total_size` → `OUT_OF_RANGE`, client restarts from 0).
- SDK v1 — `/home/qnu/Desktop/Workspace/QNu/sentinel-drm/sentinel-drm-go-sdk/docs/superpowers/specs/2026-09-11-sdk-v1-messenger-design.md` §7 (the sole exit on `ProgramIntact == false`), §8 (download off the session loop, `FetchArtifact` with offset resume).
- denju's own invariants — `/home/qnu/Desktop/Workspace/QNu/denju/CLAUDE.md` (read it before Task 1; invariants 1-17 and "The naming contract" bind every task).

## Global Constraints

- Repository: `/home/qnu/Desktop/Workspace/QNu/denju`, base `main` = `v0.3.0` = `e541312`. Line references below are as of `e541312`. The controller creates the working branch (suggested `feat/resume-retain-scoped-cooldown`) before Task 1; implementers do not.
- **Additive only.** No exported identifier is removed, renamed or changes signature. With every new option unset, behaviour is byte-for-byte v0.3.0: same files on disk, same `Result.Error` text, same record JSON, same cooldown semantics.
- **No new module dependencies.** Stdlib only. Never run `go get` or `go mod tidy` (they are the user's). The read-only check `go mod tidy -diff` is allowed.
- Go floor 1.24: `omitzero` struct tags are allowed; nothing newer than 1.24 may be used.
- **No fallback logic.** Every failure is loud: an error returned, or an ERROR/WARN log line where the path deliberately does not fail. No silent retry, no silent restart of a download within one `Update`. Where denju deletes something (a stale or superseded partial, an undescribed retained binary) it logs it.
- **Naming contract:** new derived names are ADDED to `TestNamingContract` / `TestNamingContractPaths`; no existing row or shape changes. JSON tags of `state`, `Outcome` and the new `previousRecord` are on-disk contracts.
- Coding preferences from denju `CLAUDE.md`: assign every ignored error to `_`; comment the *why*; no test dependencies (`TestNoTestDependencies`); `gofmt -s`.
- **TDD and mutation:** each task writes failing tests first, and every new test is mutation-verified with the mutation named in the task: apply it, run the named test, see red, revert by hand, see green. If a named mutation does not go red, STOP and report it — do not weaken the test or pick a different mutation silently. Equivalent mutants are named as such in the plan.
- **Verification reality:** locally only Linux runs. `go test -race` runs locally (gcc is present). Native Windows behaviour — real `os.Rename` over a non-running `.exe` hardlink, `CreateProcess` of a `.partial`-suffixed staged file in the e2e selftest, sharing-violation semantics — is proved only by the `windows-latest` CI job. `GOOS=windows go vet ./...` locally proves the Windows build of tests compiles, nothing more.
- Tagging `v0.4.0` is the user's decision. The last task stops before any tag.

## Decisions this plan makes (controller: review before dispatching Task 1)

1. **`Request.Offset` plus a `ResumableSource` marker interface.** D §9 item 1 fixed the offset on `Request`. The marker (`ResumesFromOffset()`, never called) is what keeps existing sources safe: denju only resumes, and only keeps a partial, for a source that declares it honours the offset. `httpsource` and every existing `Source` see `Offset == 0` and the v0.3.0 random-temp, delete-on-failure path. A `Config` flag instead would let a program enable resume for a source that ignores the offset and silently corrupt-then-discard every retry.
2. **denju does not restart a download inside one `Update`.** D says a rejected offset "restarts from 0"; here that means the partial is deleted and the attempt fails with `ErrResumeRejected`, so the caller's next `Update` starts at byte 0. One retry policy, in the caller; no hidden second transfer.
3. **Typed failures via `Result.Cause error`** with three sentinels: `ErrDownloadFailed`, `ErrChecksumMismatch`, `ErrResumeRejected`. The kind is attached without changing the message text, so `Result.Error` stays identical for existing consumers. *(Amended by the review-fix pass: a fourth sentinel, `ErrResumedChecksumMismatch`; see "Review-fix amendments" below.)*
4. **One partial at a time, keyed by a 16-hex-char SHA-256 of `ID \x00 TargetVersion \x00 lower(SHA256)`**, named `<binary>.<ns>-download-<key>.partial`. A digest, not the raw fields, because `Request.ID` is opaque. Every download deletes partials for other requests first (bounded disk: an artifact can be ~300 MB). `Config.PartialRetention` (default 24 h) discards a partial nobody has written to for longer, at the next download and at `Repair` when no update is in flight.
5. **The retained binary's sha and version live in a sidecar record** `<binary>.<ns>-previous.json`, written only after the binary is in place and deleted before it is replaced, so a record never describes a different file. The journal's `OldVersion`/`OldSHA256` are the source. Retention is idempotent and is retried by `Repair` (committed journal) and `CleanupReported` (which keeps the journal if retention still fails). *(Amended by the review-fix pass: `Repair` removes the committed journal once retention succeeds; see below.)*
6. **Scoped cooldown is a new `Config.CooldownScope`, default unchanged.** Piece C §5 asked to change `Cooldown` semantics; the brief requires existing behaviour unchanged for other consumers, and the old any-update hold also stops upgrade/downgrade flip-flop loops that the scoped one does not. The SDK opts in with `CooldownRolledBackVersion`. The hold is carried forward in two new `Outcome` fields so later attempts cannot erase it. Match is on version string only (C §5); Sentinel release versions are unique.
7. **`PreviousBinary()` keeps C's exact signature** `(path, sha256, version string, ok bool)`. It has no error return, so a corrupt record, or a record with no binary, returns `ok == false` **and logs ERROR**. It does not hash the file on each call; the digest check in `Update` is what guarantees a `FileSource` install is the right bytes.

## Review-fix amendments (formal review, 2026-09-17)

The formal review of the finished branch (`review-2026-09-17-denju-v0.4.0.md` in the Sentinel DRM hub) led to these changes. Where a task body below disagrees, this section and the code win; task bodies are the record of how the branch was first built.

- **M2 - a local write failure deletes the partial.** A failure to write what arrived (a full disk, a file-size limit) is told apart from a broken transfer by recording the first error from the staging writer. It deletes the partial, as v0.3.0 deleted its temp file, and carries no failure kind, for a plain `Source` as well (whose text stays v0.3.0's). Only a broken transfer keeps the partial.
- **M3 - `ErrResumedChecksumMismatch`.** A digest mismatch on a download that resumed from a non-zero offset gets this new sentinel; `errors.Is(err, ErrChecksumMismatch)` is false for it. A mismatch from byte zero stays `ErrChecksumMismatch`. The caller's rule: retry once from zero; a second mismatch is permanent.
- **M8 - a failed partial delete is not hidden.** It is logged at ERROR and the error says the partial could not be deleted instead of claiming it was discarded. No retry.
- **M4 - `Repair` removes the committed journal once retention succeeds** (under `RetainPrevious`, with the outcome on record), with the leftovers beside it (Windows helper copy, helper log), so the stale-partial sweep runs again. Invariant 20 no longer assumes a second `CleanupReported` call.
- **M5 / L8 - `Repair` does not re-record a decided outcome over a later record.** A record written after the decided journal's modification time is left alone. This also keeps a dead Windows handoff (journaled `rolledback`, recorded `failed`) from being re-recorded as `rolled_back`.
- **M1 - the upgrade caveat.** The scoped hold is written by the image rolled back *from* and enforced by the image rolled back *to*; both need v0.4.0 with the scope. A v0.4.0 program moved back to a denju v0.3.0 build that then fails is not held. The README and CHANGELOG say so.
- **Smaller:** `FileSource` refuses non-regular files and words a copy failure as "copy", not "read"; partial listing requires a 16-hex key, so a nested namespace's partials are not deleted; the `CooldownRolledBackVersion` doc states the full corrupt-record consequence; new tests pin the release-after-record ordering and the `RetainPrevious` opt-in at `Repair` and `CleanupReported`.

---

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `errors.go` (new) | Failure-kind sentinels and `kindError` | 1, 2 |
| `request.go` | `Request.Offset`, `Result.Cause`, `ResumableSource` | 1, 2 |
| `download.go` | fresh and resumable staging, digest check, failure wording | 1, 2 |
| `partial.go` (new) | partial key, path, listing, discarding | 2 |
| `filesource.go` (new) | `FileSource`, `ctxReader` | 3 |
| `previous.go` (new) | `previousRecord`, `PreviousBinary`, `retainPrevious`, `releaseRollbackCopy` | 4 |
| `config.go` | `PartialRetention`, `RetainPrevious`, `CooldownScope` + names | 2, 4, 5 |
| `updater.go` | `New` wiring/validation, `CleanupReported`, `InCooldownFor` | 4, 5 |
| `update.go` | `Offset` refusal, `failErr`, `Cause` plumbing, scoped refusal message | 1, 2, 5 |
| `state.go` | `paths.Previous`, `paths.PreviousRecord` | 4 |
| `attest.go` | commit releases via `releaseRollbackCopy` | 4 |
| `repair.go` | stale-partial sweep; retention on committed/reconciled journals | 2, 4 |
| `record.go` | `Outcome` rollback anchor, `store.scope`, carry-forward, `inCooldownFor` | 5 |
| tests | `resume_test.go`, `filesource_test.go`, `previous_test.go` (new); `update_test.go`, `download_test.go`, `names_test.go`, `repair_test.go`, `attest_test.go`, `record_test.go`, `updater_test.go`, `e2e_test.go` (extended) | 1-6 |
| docs | `README.md`, `doc.go`, `CLAUDE.md`, `CHANGELOG.md` | 7 |

Task order: 1 → 2 → 3 → 4 → 5 → 6 → 7. Tasks 4 and 5 do not depend on 2/3 but run sequentially anyway (shared files).

---

### Task 1: Typed failure causes on `Result`

**Files:**
- Create: `errors.go`
- Modify: `request.go:48-81` (`Result`), `download.go:25-67`, `update.go:82-85` (download call), `update.go:331-393` (`fail`, `record`, `abandon`, `recordWith`)
- Test: `update_test.go`, `download_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `var ErrDownloadFailed error`, `var ErrChecksumMismatch error` (package `denju`)
  - `type kindError struct{ kind, err error }` with `Error() string` (returns `err.Error()`) and `Unwrap() []error`
  - `func withKind(kind, err error) error`
  - `Result.Cause error`
  - `func (u *Updater) failErr(req Request, err error) Result`
  - `func (u *Updater) recordWith(req Request, status Status, cause error, intact bool) Result` (signature change of an unexported func: `msg string` → `cause error`)

- [ ] **Step 1: Write the failing tests**

Append to `update_test.go`:

```go
// ---------------------------------------------------------------- failure kinds

// A caller has to be able to tell a transfer worth retrying from a download that
// is simply wrong, without parsing a message meant for a human. The message
// itself must not change: existing consumers log and report it verbatim.
func TestUpdate_DownloadFailuresCarryTheirKind(t *testing.T) {
	t.Run("a source error", func(t *testing.T) {
		r := newUpdateRig(t)
		r.src.err = errors.New("the connection dropped")

		got := r.u.Update(context.Background(), r.req(), r.src)
		isTrue(t, errors.Is(got.Cause, ErrDownloadFailed), "Cause is a download failure")
		isFalse(t, errors.Is(got.Cause, ErrChecksumMismatch), "Cause is not a checksum mismatch")
		isTrue(t, errors.Is(got.Cause, r.src.err), "the source's own error stays reachable")
		eq(t, got.Error, "download the update: the connection dropped", "the message is unchanged")
	})

	t.Run("a checksum mismatch", func(t *testing.T) {
		r := newUpdateRig(t)
		req := r.req()
		req.SHA256 = sha256Hex([]byte("something else entirely"))

		got := r.u.Update(context.Background(), req, r.src)
		isTrue(t, errors.Is(got.Cause, ErrChecksumMismatch), "Cause is a checksum mismatch")
		isFalse(t, errors.Is(got.Cause, ErrDownloadFailed), "Cause is not a download failure")
		isTrue(t, strings.HasPrefix(got.Error, "the downloaded update does not match its checksum: expected "),
			"the message is unchanged")
	})
}

// Cause is never left nil beside a non-empty Error, and a failure that is not a
// download failure carries no download kind.
func TestUpdate_EveryFailureCarriesACause(t *testing.T) {
	r := newUpdateRig(t)

	got := r.u.Update(context.Background(), Request{ID: "cmd-1"}, r.src)
	if got.Cause == nil {
		t.Fatal("a refused update must carry a Cause")
	}
	eq(t, got.Cause.Error(), got.Error, "Cause and Error say the same thing")
	isFalse(t, errors.Is(got.Cause, ErrDownloadFailed), "an incomplete request is not a download failure")
	isFalse(t, errors.Is(got.Cause, ErrChecksumMismatch), "an incomplete request is not a checksum mismatch")
}
```

Append to `download_test.go`:

```go
// A source that overruns DownloadTimeout failed to download, whatever its own
// error says.
func TestDownload_TimeoutIsADownloadFailure(t *testing.T) {
	u := downloadTestUpdater(t)
	u.cfg.DownloadTimeout = 30 * time.Millisecond

	_, err := u.download(context.Background(), Request{SHA256: "irrelevant"}, slowSource{})
	isTrue(t, errors.Is(err, ErrDownloadFailed), "a timed-out download is a download failure")
	isTrue(t, errors.Is(err, context.DeadlineExceeded), "the deadline stays reachable")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -count=1 -run 'TestUpdate_DownloadFailuresCarryTheirKind|TestUpdate_EveryFailureCarriesACause|TestDownload_TimeoutIsADownloadFailure'`
Expected: FAIL to compile — `undefined: ErrDownloadFailed`, `got.Cause undefined`.

- [ ] **Step 3: Implement**

Create `errors.go`:

```go
package denju

import "errors"

// Failure kinds a caller may need to tell apart. Match them with errors.Is
// against [Result.Cause]. The human-readable text is unaffected and is still in
// [Result.Error].
var (
	// ErrDownloadFailed means [Source.Fetch] returned an error, or overran
	// [Config.DownloadTimeout]. With a [ResumableSource] the bytes already
	// received are kept, and the next Update for the same request resumes from
	// them; with any other Source nothing is kept.
	ErrDownloadFailed = errors.New("denju: the download failed")

	// ErrChecksumMismatch means the complete download did not hash to
	// [Request.SHA256]. Everything written for it has been deleted, so the next
	// attempt starts from byte zero. There is no partial-trust path.
	ErrChecksumMismatch = errors.New("denju: the download does not match its checksum")
)

// kindError attaches a failure kind to an error WITHOUT changing its text, so
// Result.Error reads exactly as it did before kinds existed - consumers log and
// report that text, and some match on it.
type kindError struct {
	kind error
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }

// Unwrap exposes both the kind and the underlying error, so errors.Is matches
// either.
func (e *kindError) Unwrap() []error { return []error{e.kind, e.err} }

// withKind tags err with kind.
func withKind(kind, err error) error { return &kindError{kind: kind, err: err} }
```

In `request.go`, add to `Result` directly after `Error string`:

```go
	// Cause is the error behind Error, for errors.Is. It is nil exactly when
	// Error is empty. Download failures carry a kind - [ErrDownloadFailed] or
	// [ErrChecksumMismatch] - so a caller can tell a transfer worth retrying
	// from one that is not; every other failure carries its message and no kind.
	Cause error
```

(Task 2 adds `ErrResumeRejected` to this list.)

In `download.go`, wrap the two failure returns (message text unchanged):

```go
	if err := src.Fetch(ctx, req, io.MultiWriter(tmp, h)); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return "", withKind(ErrDownloadFailed, fmt.Errorf("download the update: %w (after %s)", err, u.cfg.DownloadTimeout))
		}
		return "", withKind(ErrDownloadFailed, fmt.Errorf("download the update: %w", err))
	}
```

```go
	if !strings.EqualFold(got, req.SHA256) {
		_ = os.Remove(tmpPath)
		return "", withKind(ErrChecksumMismatch, fmt.Errorf("the downloaded update does not match its checksum: expected %s, got %s", req.SHA256, got))
	}
```

In `update.go`, change the download call site:

```go
	stagedPath, err := u.download(ctx, req, src)
	if err != nil {
		return u.failErr(req, err)
	}
```

and replace `fail`, `record`, `abandon`, `recordWith` (keep their existing doc comments; add `failErr`'s):

```go
// fail logs and records a failure that left the program untouched.
func (u *Updater) fail(req Request, msg string) Result {
	return u.failErr(req, errors.New(msg))
}

// failErr is fail for an error whose kind a caller may need: the error itself
// becomes Result.Cause, so errors.Is still sees the kind.
func (u *Updater) failErr(req Request, err error) Result {
	u.log(LevelError, "refused",
		"target_version", req.TargetVersion,
		"current_version", u.cfg.Version,
		"reason", err.Error())
	return u.recordWith(req, StatusFailed, err, true)
}
```

```go
func (u *Updater) record(req Request, status Status, msg string) Result {
	return u.recordWith(req, status, errors.New(msg), true)
}
```

```go
func (u *Updater) abandon(j *state, status Status, msg string) Result {
	return u.recordWith(Request{ID: j.CommandID, TargetVersion: j.TargetVersion}, status, errors.New(msg), false)
}

func (u *Updater) recordWith(req Request, status Status, cause error, intact bool) Result {
	msg := cause.Error()
	res := Result{Status: status, Error: msg, Cause: cause, ProgramIntact: intact}
	if req.ID == "" {
		return res
	}
	o := &Outcome{
		At:          time.Now(),
		ID:          req.ID,
		FromVersion: u.cfg.Version,
		ToVersion:   req.TargetVersion,
		Status:      status,
		Error:       msg,
	}
	if err := u.records.record(o); err != nil {
		u.log(LevelError, "could not write the update record", "err", err)
	}
	return res
}
```

Add `"errors"` to `update.go`'s imports.

- [ ] **Step 4: Run the tests to verify they pass, then the whole suite**

Run: `go test ./... -count=1 -run 'TestUpdate_DownloadFailuresCarryTheirKind|TestUpdate_EveryFailureCarriesACause|TestDownload_TimeoutIsADownloadFailure' -v`
Expected: PASS.
Run: `go test ./... -count=1`
Expected: PASS (no existing test changes behaviour).

- [ ] **Step 5: Mutation-verify (revert each by hand)**

| Test | Mutation | Expected |
|---|---|---|
| `TestUpdate_DownloadFailuresCarryTheirKind/a_source_error` | In `download.go` drop `withKind(ErrDownloadFailed, …)` from the non-timeout Fetch branch (return the bare `fmt.Errorf`) | red: "Cause is a download failure" |
| `TestUpdate_DownloadFailuresCarryTheirKind/a_source_error` | Change `kindError.Error()` to `return e.kind.Error() + ": " + e.err.Error()` | red: "the message is unchanged" |
| `TestUpdate_DownloadFailuresCarryTheirKind/a_checksum_mismatch` | Swap the kind on the checksum branch to `ErrDownloadFailed` | red |
| `TestUpdate_EveryFailureCarriesACause` | In `recordWith` build `Result{…}` without `Cause: cause` | red: Fatal "must carry a Cause" |
| `TestDownload_TimeoutIsADownloadFailure` | Drop `withKind` from the `ctx.Err() != nil` branch only | red |

- [ ] **Step 6: Stop: controller reviews; do not stage or commit**

---

### Task 2: Resumable downloads (`Request.Offset`, `ResumableSource`, request-keyed partial)

**Files:**
- Create: `partial.go`, `resume_test.go`
- Modify: `errors.go` (add `ErrResumeRejected`), `request.go` (`Request.Offset`, `ResumableSource`), `config.go` (`PartialRetention`, default, `names.prePartial`/`extPartial`), `download.go` (full rewrite below), `update.go:31-41` (refuse a caller-set Offset), `repair.go:30-39` (stale sweep), `names_test.go`, `updater_test.go:28-37`, `update_test.go`, `repair_test.go`

**Interfaces:**
- Consumes (Task 1): `withKind`, `ErrDownloadFailed`, `ErrChecksumMismatch`, `failErr`.
- Produces:
  - `var ErrResumeRejected error`
  - `Request.Offset int64`
  - `type ResumableSource interface { Source; ResumesFromOffset() }`
  - `Config.PartialRetention time.Duration`, `const defaultPartialRetention = 24 * time.Hour`
  - `names.prePartial string` (`.<ns>-download-`), `names.extPartial string` (`.partial`)
  - `func partialKey(req Request) string`
  - `func (u *Updater) partialPath(req Request) string`
  - `func (u *Updater) partialFiles() ([]string, error)`
  - `func (u *Updater) discardOtherPartials(keep string)`
  - `func (u *Updater) discardStalePartials(now time.Time)`
  - `func (u *Updater) downloadFresh(ctx context.Context, req Request, src Source) (string, error)`
  - `func (u *Updater) downloadResumable(ctx context.Context, req Request, src ResumableSource) (string, error)`

- [ ] **Step 1: Write the failing tests**

Create `resume_test.go`:

```go
package denju

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// resumingSource serves payload from req.Offset, as a server honouring a ranged
// read does. failAfter > 0 breaks the stream after that many bytes of the NEXT
// call only; reject makes it refuse the offset outright. Every offset it is
// asked for is kept, because the offset is the whole point.
type resumingSource struct {
	payload   []byte
	failAfter int
	reject    bool
	offsets   []int64
}

func (s *resumingSource) ResumesFromOffset() {}

func (s *resumingSource) Fetch(_ context.Context, req Request, w io.Writer) error {
	s.offsets = append(s.offsets, req.Offset)
	if s.reject || req.Offset > int64(len(s.payload)) {
		return fmt.Errorf("%w: the artifact changed", ErrResumeRejected)
	}
	rest := s.payload[req.Offset:]
	if s.failAfter > 0 && s.failAfter < len(rest) {
		n := s.failAfter
		s.failAfter = 0
		if _, err := w.Write(rest[:n]); err != nil {
			return err
		}
		return errors.New("the stream broke")
	}
	_, err := w.Write(rest)
	return err
}

// plainOffsetSource is an ordinary Source that records the offset it sees.
type plainOffsetSource struct {
	payload []byte
	offsets *[]int64
}

func (s plainOffsetSource) Fetch(_ context.Context, req Request, w io.Writer) error {
	*s.offsets = append(*s.offsets, req.Offset)
	_, err := w.Write(s.payload)
	return err
}

var resumePayload = []byte("0123456789abcdefghijklmnopqrstuvwxyz")

func resumeRequest() Request {
	return Request{ID: "cmd-7", TargetVersion: "1.5.0", SHA256: sha256Hex(resumePayload)}
}

func resumeTestUpdater(t *testing.T) (*Updater, *captureLog) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "current")
	log := &captureLog{}
	u, err := New(Config{Namespace: "app", Version: "1.4.0", BinaryPath: bin, Log: log.Logger()})
	noErr(t, err, "New")
	return u, log
}

func eqOffsets(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("offsets asked for = %v, want %v", got, want)
	}
}

// A broken stream keeps what arrived. Deleting it - v0.3.0's behaviour - is what
// made every retry start again from zero over a link that had just failed.
func TestDownloadResumable_FailedFetchKeepsThePartial(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	src := &resumingSource{payload: resumePayload, failAfter: 10}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrDownloadFailed), "a broken stream is a download failure")
	isFalse(t, errors.Is(err, ErrResumeRejected), "and not a rejected resume")
	hasSubstr(t, err.Error(), "kept for the next attempt", "the error says the bytes were kept")
	eq(t, readFileString(t, u.partialPath(req)), "0123456789", "the bytes received are kept")
}

// The next attempt at the SAME request asks for the bytes it lacks, and the
// prefix plus the remainder is the artifact.
func TestDownloadResumable_ResumesFromTheBytesOnDisk(t *testing.T) {
	u, log := resumeTestUpdater(t)
	req := resumeRequest()
	src := &resumingSource{payload: resumePayload, failAfter: 10}

	_, err := u.download(context.Background(), req, src)
	wantErr(t, err, "the first attempt breaks")

	staged, err := u.download(context.Background(), req, src)
	noErr(t, err, "the second attempt resumes")
	eq(t, staged, u.partialPath(req), "the completed partial is what gets staged")
	eq(t, readFileString(t, staged), string(resumePayload), "prefix and remainder form the artifact")
	eqOffsets(t, src.offsets, 0, 10)
	isTrue(t, log.contains("resuming a partial download"), "the resume is logged")
}

// Nothing already on disk is trusted on its own: the digest covers the prefix,
// and a partial that fails it is deleted so the next attempt starts clean.
func TestDownloadResumable_ACorruptPrefixFailsTheDigestAndIsDeleted(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, []byte("X123456789"), 0o600), "seed a corrupt prefix")
	src := &resumingSource{payload: resumePayload}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrChecksumMismatch), "a corrupt prefix is a checksum mismatch")
	eqOffsets(t, src.offsets, 10)
	isFalse(t, exists(path), "a partial that fails the digest is deleted")
}

// A source that cannot continue from the offset is a distinct, visible outcome:
// the partial goes, the attempt fails, and the NEXT attempt starts from zero.
// denju does not quietly restart inside the same attempt.
func TestDownloadResumable_RejectedOffsetDiscardsThePartial(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
	src := &resumingSource{payload: resumePayload, reject: true}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrResumeRejected), "a refused offset is a rejected resume")
	isFalse(t, errors.Is(err, ErrDownloadFailed), "and not an ordinary transfer failure")
	hasSubstr(t, err.Error(), "byte 10", "the error names the offset that was refused")
	isFalse(t, exists(path), "the partial is discarded")
	eqOffsets(t, src.offsets, 10)

	src.reject = false
	staged, err := u.download(context.Background(), req, src)
	noErr(t, err, "the next attempt")
	eq(t, readFileString(t, staged), string(resumePayload), "the next attempt fetched everything")
	eqOffsets(t, src.offsets, 10, 0)
}

func TestDownloadResumable_PartialRetention(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		want     []int64
		wantWarn bool
	}{
		{name: "a fresh partial is resumed", age: 30 * time.Minute, want: []int64{10}},
		{name: "a stale partial is discarded", age: 2 * time.Hour, want: []int64{0}, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, log := resumeTestUpdater(t)
			u.cfg.PartialRetention = time.Hour
			req := resumeRequest()
			path := u.partialPath(req)
			noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
			backdate(t, path, tc.age)
			src := &resumingSource{payload: resumePayload}

			staged, err := u.download(context.Background(), req, src)
			noErr(t, err, "download")
			eq(t, readFileString(t, staged), string(resumePayload), "the artifact")
			eqOffsets(t, src.offsets, tc.want...)
			eq(t, log.contains("discarded a stale partial download"), tc.wantWarn, "stale discard logged")
		})
	}
}

// One download is in flight at a time, so a partial for any other request is
// superseded and only holds disk.
func TestDownloadResumable_DiscardsPartialsOfOtherRequests(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	other := u.partialPath(Request{ID: "cmd-6", TargetVersion: req.TargetVersion, SHA256: req.SHA256})
	noErr(t, os.WriteFile(other, resumePayload[:10], 0o600), "seed another request's partial")
	src := &resumingSource{payload: resumePayload}

	_, err := u.download(context.Background(), req, src)
	noErr(t, err, "download")
	isFalse(t, exists(other), "the superseded partial is discarded")
	eqOffsets(t, src.offsets, 0)
}

// An ordinary Source behaves exactly as in v0.3.0: offset zero, a random temp
// file, and no partial left behind - including one some earlier resumable
// attempt at the same request left.
func TestDownload_PlainSourceIgnoresPartials(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
	var offsets []int64

	staged, err := u.download(context.Background(), req, plainOffsetSource{payload: resumePayload, offsets: &offsets})
	noErr(t, err, "download")
	isTrue(t, strings.HasPrefix(filepath.Base(staged), ".app-update-"), "a plain source stages exactly as before")
	isFalse(t, exists(path), "a plain source leaves no partial behind")
	eqOffsets(t, offsets, 0)
}
```

Append to `names_test.go`:

```go
// TestPartialKey pins how a partial download is named after its request. It is
// an on-disk contract like the rest of this file: a program that changes it
// orphans every partial on disk across the changeover.
func TestPartialKey(t *testing.T) {
	const sha = "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e"
	base := Request{ID: "cmd-42", TargetVersion: "1.4.0", SHA256: sha}

	eq(t, partialKey(base), "1d0e5a656aa7af44", "the key for a fixed request")

	upper := base
	upper.SHA256 = strings.ToUpper(sha)
	eq(t, partialKey(upper), partialKey(base), "a digest is the same digest in either case")

	withOffset := base
	withOffset.Offset = 99
	eq(t, partialKey(withOffset), partialKey(base), "the offset is not part of the request's identity")

	for name, r := range map[string]Request{
		"id":             {ID: "cmd-43", TargetVersion: base.TargetVersion, SHA256: sha},
		"target version": {ID: base.ID, TargetVersion: "1.4.1", SHA256: sha},
		"digest":         {ID: base.ID, TargetVersion: base.TargetVersion, SHA256: strings.Repeat("0", 64)},
	} {
		isTrue(t, partialKey(r) != partialKey(base), "a different "+name+" is a different partial")
	}

	u, err := New(Config{Namespace: "acme", Version: "1.0.0", BinaryPath: "/opt/app/myprog"})
	noErr(t, err, "New")
	eq(t, u.partialPath(base), "/opt/app/myprog.acme-download-1d0e5a656aa7af44.partial", "the partial's path")
}
```

Also in `names_test.go` `TestNamingContract`: add fields `prePartial string` and `extPartial string` to the case struct; set `prePartial: ".acme-download-", extPartial: ".partial"` for `acme` and `prePartial: ".widget-co-download-", extPartial: ".partial"` for `widget-co`; add `{"prePartial", n.prePartial, tc.prePartial}` and `{"extPartial", n.extPartial, tc.extPartial}` to the compared list. Do not touch any existing row.

In `updater_test.go` `TestNew_AppliesDefaults`, add:

```go
	eq(t, u.cfg.PartialRetention, defaultPartialRetention, "PartialRetention")
```

Append to `update_test.go`:

```go
// The offset describes bytes denju itself holds on disk. A caller that sets it
// is asking denju to trust bytes it never wrote.
func TestUpdate_CallerSetOffsetIsRefused(t *testing.T) {
	r := newUpdateRig(t)
	req := r.req()
	req.Offset = 5

	got := r.u.Update(context.Background(), req, r.src)
	eq(t, got.Status, StatusFailed, "status")
	hasSubstr(t, got.Error, "Offset", "error")
	isTrue(t, got.ProgramIntact, "refused before anything happened")
	eq(t, r.src.calls, 0, "nothing is downloaded")
}
```

Append to `repair_test.go`:

```go
// A partial for an update that never came back is reclaimed once it is past
// retention, so a host that stops updating does not hold hundreds of megabytes
// forever. A fresh one may still be resumed and is kept.
func TestRepair_NoJournalDiscardsStalePartials(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	f.u.cfg.PartialRetention = time.Hour
	stale := f.u.partialPath(Request{ID: "cmd-old", TargetVersion: "0.9.0", SHA256: "aa"})
	fresh := f.u.partialPath(Request{ID: "cmd-new", TargetVersion: "1.1.0", SHA256: "bb"})
	writeBinary(t, stale, "half an old download")
	backdate(t, stale, 2*time.Hour)
	writeBinary(t, fresh, "half a current download")

	noErr(t, f.u.Repair(), "Repair")

	isFalse(t, exists(stale), "a stale partial is reclaimed")
	isTrue(t, exists(fresh), "a fresh partial is kept for its retry")
}

// With an update in flight the journal may point at a completed partial as the
// staged binary, so repair leaves partials alone.
func TestRepair_PartialsAreLeftAloneWhileAnUpdateIsInFlight(t *testing.T) {
	f := newRepairFixture(t, "program", "1.0.0")
	f.u.cfg.PartialRetention = time.Hour
	staged := f.u.partialPath(Request{ID: "cmd-1", TargetVersion: "1.1.0", SHA256: "aa"})
	writeBinary(t, staged, "a verified download")
	backdate(t, staged, 2*time.Hour)
	stageJournal(t, f.u, &state{Phase: phaseStaged, HelperPID: 999, NewBinaryPath: staged})
	withPIDAlive(t, func(int) bool { return true })

	noErr(t, f.u.Repair(), "Repair")

	isTrue(t, exists(staged), "the journal's staged binary is not a leftover")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -count=1`
Expected: FAIL to compile — `undefined: ErrResumeRejected`, `u.partialPath undefined`, `req.Offset undefined`, `partialKey undefined`, `n.prePartial undefined`, `defaultPartialRetention undefined`.

- [ ] **Step 3: Implement**

Append to `errors.go`'s `var (…)` block:

```go
	// ErrResumeRejected is returned BY a [ResumableSource], wrapped, when it
	// cannot continue from [Request.Offset]: the artifact behind the request is
	// shorter than the offset, or is no longer the artifact the earlier bytes
	// came from. denju then deletes the partial and fails the attempt with this
	// kind; the next attempt starts from byte zero. denju never restarts a
	// transfer on its own inside one attempt.
	ErrResumeRejected = errors.New("denju: the source cannot resume from this offset")
```

In `request.go`, change the `Result.Cause` doc's kind list to "[ErrDownloadFailed], [ErrChecksumMismatch] or [ErrResumeRejected]", and add to `Request` after `TargetArch string`:

```go

	// Offset is how many bytes of the artifact denju already holds on disk
	// from an earlier attempt at this same request. denju sets it; a caller
	// must leave it zero, and [Updater.Update] refuses a request that arrives
	// with it set.
	//
	// Only a [ResumableSource] ever sees a non-zero Offset. Any other Source
	// always sees zero.
	Offset int64
```

and after the `Source` interface:

```go
// ResumableSource is a [Source] that honours [Request.Offset]: its Fetch writes
// the artifact starting at that byte, not at the beginning, or returns an error
// wrapping [ErrResumeRejected] when it cannot.
//
// Implementing it changes what denju does with a failed download. For a plain
// Source a failed Fetch deletes everything written. For a ResumableSource the
// bytes are kept in a partial named after the request - its ID, TargetVersion
// and SHA256 - and the next Update with the same three resumes from them, after
// denju re-hashes what is already on disk. The digest check at the end still
// covers every byte, old and new.
//
// A partial is deleted when the source rejects its offset, when the complete
// file fails the digest, when a download for a different request starts, and
// when nothing has written to it for [Config.PartialRetention].
type ResumableSource interface {
	Source
	// ResumesFromOffset declares that Fetch honours Request.Offset. denju never
	// calls it; implementing it is the declaration.
	ResumesFromOffset()
}
```

In `config.go`: add to `Config` after `StaleThreshold`:

```go
	// PartialRetention bounds how long an interrupted download is kept for a
	// retry to resume. A partial nothing has written to for longer is deleted,
	// by the next download or by [Updater.Repair] on a start with no update in
	// flight. Default 24 hours.
	//
	// Only a [ResumableSource] ever leaves a partial behind.
	PartialRetention time.Duration
```

add `defaultPartialRetention = 24 * time.Hour` to the defaults block, and in `withDefaults`:

```go
	if c.PartialRetention == 0 {
		c.PartialRetention = defaultPartialRetention
	}
```

add to `names` after `sufRecord`:

```go

	// A partial download is <binary><prePartial><key><extPartial>, where key
	// is derived from the request (partialKey). Only a ResumableSource leaves one.
	prePartial string // .<ns>-download-
	extPartial string // .partial
```

and to `newNames` after `sufRecord`:

```go

		prePartial: "." + ns + "-download-",
		extPartial: ".partial",
```

Create `partial.go`:

```go
package denju

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// partialKey names a partial download after the request it belongs to, so the
// next attempt at the SAME request finds it and an attempt at any other request
// does not. It is a digest rather than the fields themselves because Request.ID
// is opaque and may hold characters no filename can.
//
// The digest is lowercased first because denju compares digests
// case-insensitively; Offset is not part of the identity.
func partialKey(req Request) string {
	h := sha256.New()
	_, _ = io.WriteString(h, req.ID)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, req.TargetVersion)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, strings.ToLower(req.SHA256))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// partialPath is where a ResumableSource's download for req is staged. It sits
// beside the binary for the same reason every staged download does: the swap
// that follows is a rename, atomic only within one filesystem.
func (u *Updater) partialPath(req Request) string {
	return u.binaryPath + u.n.prePartial + partialKey(req) + u.n.extPartial
}

// partialFiles lists every partial download beside the binary.
//
// It matches names by prefix and suffix rather than with filepath.Glob, because
// the binary's own name may contain glob metacharacters.
func (u *Updater) partialFiles() ([]string, error) {
	dir := filepath.Dir(u.binaryPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(u.binaryPath) + u.n.prePartial
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, u.n.extPartial) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// discardOtherPartials deletes every partial download except keep ("" keeps
// none). One download is in flight at a time, so any other partial belongs to a
// request that has been superseded - another command, target or digest - and
// keeping it would only hold disk: an artifact can be hundreds of megabytes.
//
// Best-effort, like every other cleanup here, but never silent.
func (u *Updater) discardOtherPartials(keep string) {
	files, err := u.partialFiles()
	if err != nil {
		u.log(LevelWarn, "could not list partial downloads", "err", err)
		return
	}
	for _, f := range files {
		if f == keep {
			continue
		}
		if err := os.Remove(f); err != nil {
			u.log(LevelWarn, "could not discard a partial download for another request", "path", f, "err", err)
			continue
		}
		u.log(LevelInfo, "discarded a partial download for another request", "path", f)
	}
}

// discardStalePartials deletes every partial nothing has written to for longer
// than PartialRetention. Repair calls it only when no update is in flight: with
// a journal present, a completed partial may be the journal's staged binary.
func (u *Updater) discardStalePartials(now time.Time) {
	files, err := u.partialFiles()
	if err != nil {
		u.log(LevelWarn, "could not list partial downloads", "err", err)
		return
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || now.Sub(info.ModTime()) <= u.cfg.PartialRetention {
			continue
		}
		if err := os.Remove(f); err != nil {
			u.log(LevelWarn, "could not discard a stale partial download", "path", f, "err", err)
			continue
		}
		u.log(LevelInfo, "discarded a stale partial download", "path", f, "bytes", info.Size())
	}
}
```

Replace `download.go` entirely with:

```go
package denju

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// keptSuffix is appended to a failed resumable download's error, so the log
// line says the bytes survived.
const keptSuffix = "; the partial download is kept for the next attempt"

// download stages the new binary next to the one it will replace and verifies
// its digest, returning the staged path.
//
// The staged file is created in the TARGET BINARY'S directory, not in the
// system temp directory, for two reasons: the swap that follows is an
// os.Rename, which is only atomic within one filesystem, and staging elsewhere
// would silently turn the swap into a copy across a mount boundary. It also
// means a download too large for the destination fails here, while the running
// program is still untouched, rather than halfway through the swap.
//
// A ResumableSource stages into a partial named after the request, kept across
// a failed Fetch (downloadResumable). Any other Source stages into a fresh temp
// file deleted on any failure, exactly as before resuming existed
// (downloadFresh). Either way every partial left by a DIFFERENT request is
// deleted first. On success the caller owns the staged file.
func (u *Updater) download(ctx context.Context, req Request, src Source) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.DownloadTimeout)
	defer cancel()

	if rs, ok := src.(ResumableSource); ok {
		u.discardOtherPartials(u.partialPath(req))
		return u.downloadResumable(ctx, req, rs)
	}
	u.discardOtherPartials("")
	return u.downloadFresh(ctx, req, src)
}

// downloadFresh is the v0.3.0 path: a random temp file, deleted on any failure.
func (u *Updater) downloadFresh(ctx context.Context, req Request, src Source) (string, error) {
	dir := filepath.Dir(u.binaryPath)
	tmp, err := os.CreateTemp(dir, u.n.tmpDownload)
	if err != nil {
		return "", fmt.Errorf("create a staging file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	h := sha256.New()
	if err := src.Fetch(ctx, req, io.MultiWriter(tmp, h)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", u.fetchFailure(ctx, err, "")
	}
	// fsync before the digest is trusted: the bytes have to be on the device,
	// not merely in the page cache, because the process that verified them is
	// about to be replaced by one that only sees what survived.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("flush the downloaded update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("finalize the downloaded update: %w", err)
	}
	if err := checkDigest(h, req.SHA256); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// downloadResumable stages the artifact in a partial named after the request,
// resuming from whatever an earlier attempt at the same request left behind.
//
// The prefix already on disk is re-hashed before a single new byte is
// written, so the digest check at the end covers every byte of the file, old
// and new alike. Nothing written earlier is trusted on its own.
//
// What happens to the partial is the contract a caller retries against:
//   - Fetch fails: it is flushed and KEPT, and the next attempt resumes.
//   - The source rejects the offset (ErrResumeRejected): it is deleted and the
//     attempt fails; the next attempt starts from byte zero.
//   - The complete file fails the digest: it is deleted and the attempt fails;
//     the next attempt starts from byte zero.
//   - Nothing has written to it for PartialRetention: it is deleted before
//     this attempt, which starts from byte zero.
func (u *Updater) downloadResumable(ctx context.Context, req Request, src ResumableSource) (string, error) {
	path := u.partialPath(req)

	if info, err := os.Stat(path); err == nil {
		if age := time.Since(info.ModTime()); age > u.cfg.PartialRetention {
			if err := os.Remove(path); err != nil {
				return "", fmt.Errorf("discard a stale partial download: %w", err)
			}
			u.log(LevelWarn, "discarded a stale partial download; starting from the beginning",
				"bytes", info.Size(), "age", age.Round(time.Second), "id", req.ID)
		}
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("open a partial download in %s: %w", filepath.Dir(path), err)
	}
	h := sha256.New()
	// Reading to the end both replays the hash and leaves the file position at
	// the end, so the bytes Fetch writes next are appended.
	offset, err := io.Copy(h, f)
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("read back the partial download: %w", err)
	}
	if offset > 0 {
		u.log(LevelInfo, "resuming a partial download", "offset", offset, "id", req.ID)
	}

	req.Offset = offset
	if err := src.Fetch(ctx, req, io.MultiWriter(f, h)); err != nil {
		if errors.Is(err, ErrResumeRejected) {
			_ = f.Close()
			_ = os.Remove(path)
			return "", withKind(ErrResumeRejected, fmt.Errorf(
				"download the update: the source refused to resume from byte %d, so the partial download was discarded: %w", offset, err))
		}
		// Flushed so what is kept is really on the device. If a power cut loses
		// part of it anyway, the digest check at the end of a later attempt still
		// refuses the result - the error is deliberately not escalated here.
		_ = f.Sync()
		_ = f.Close()
		return "", u.fetchFailure(ctx, err, keptSuffix)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("flush the downloaded update: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("finalize the downloaded update: %w", err)
	}
	if err := checkDigest(h, req.SHA256); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// fetchFailure words a failed Fetch, tagged ErrDownloadFailed. suffix is
// appended when bytes survive for the next attempt; for a plain Source it is
// empty and the text is exactly v0.3.0's.
func (u *Updater) fetchFailure(ctx context.Context, err error, suffix string) error {
	if ctx.Err() != nil {
		return withKind(ErrDownloadFailed, fmt.Errorf("download the update: %w (after %s)%s", err, u.cfg.DownloadTimeout, suffix))
	}
	return withKind(ErrDownloadFailed, fmt.Errorf("download the update: %w%s", err, suffix))
}

// checkDigest compares the running hash with the promised digest, tagged
// ErrChecksumMismatch when they differ.
func checkDigest(h hash.Hash, want string) error {
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return withKind(ErrChecksumMismatch, fmt.Errorf("the downloaded update does not match its checksum: expected %s, got %s", want, got))
	}
	return nil
}
```

Note: `fmt.Errorf("…%w (after %s)%s", …)` — keep `%w` exactly once per call.

In `update.go` `Update`, immediately after the `TargetVersion == "" || SHA256 == ""` check:

```go
	if req.Offset != 0 {
		return u.fail(req, "the update request sets Offset, which denju owns: leave it zero")
	}
```

In `repair.go` `Repair`, inside `if !exists(p.State) {`, after `_ = os.Remove(p.Restart)`:

```go
		// Partials only when nothing is in flight: with a journal present a
		// completed partial may be the journal's staged binary.
		u.discardStalePartials(time.Now())
```

(`repair.go` already imports `time`.) Update the comment above the block to mention stale partial downloads.

- [ ] **Step 4: Run the tests to verify they pass, then the whole suite**

Run: `go test ./... -count=1 -run 'TestDownloadResumable|TestDownload_PlainSourceIgnoresPartials|TestPartialKey|TestNamingContract|TestNew_AppliesDefaults|TestUpdate_CallerSetOffsetIsRefused|TestRepair_NoJournalDiscardsStalePartials|TestRepair_PartialsAreLeftAloneWhileAnUpdateIsInFlight' -v`
Expected: PASS.
Run: `go test ./... -count=1` then `GOOS=windows go vet ./...`
Expected: PASS; vet clean.

- [ ] **Step 5: Mutation-verify (revert each by hand)**

| Test | Mutation | Expected |
|---|---|---|
| `TestDownloadResumable_FailedFetchKeepsThePartial` | In the non-rejected Fetch-error branch add `_ = os.Remove(path)` after `f.Close()` | red |
| `TestDownloadResumable_ResumesFromTheBytesOnDisk` | Replace `req.Offset = offset` with `req.Offset = 0` | red (offsets `[0 0]`, content wrong) |
| `TestDownloadResumable_ResumesFromTheBytesOnDisk` | Open with `os.O_RDWR\|os.O_CREATE\|os.O_TRUNC` | red (offsets `[0 0]`) |
| `TestDownloadResumable_ResumesFromTheBytesOnDisk` | Don't replay the prefix: replace `io.Copy(h, f)` with `f.Seek(0, io.SeekEnd)` (hash starts empty) | red (digest mismatch on a correct resume) |
| `TestDownloadResumable_ACorruptPrefixFailsTheDigestAndIsDeleted` | Skip `checkDigest` when `offset > 0` | red |
| `TestDownloadResumable_ACorruptPrefixFailsTheDigestAndIsDeleted` | Drop `_ = os.Remove(path)` on digest failure | red: "is deleted" |
| `TestDownloadResumable_RejectedOffsetDiscardsThePartial` | Remove the `errors.Is(err, ErrResumeRejected)` branch (treat as ordinary failure) | red |
| `TestDownloadResumable_PartialRetention` | Delete the stale-check block | red (stale subtest offsets `[10]`) |
| `TestDownloadResumable_PartialRetention` | Change `age > u.cfg.PartialRetention` to `age > 0` | red (fresh subtest offsets `[0]`) |
| `TestDownloadResumable_DiscardsPartialsOfOtherRequests` | Remove the `discardOtherPartials(...)` call in the resumable branch of `download` | red |
| `TestDownload_PlainSourceIgnoresPartials` | In `download`, route every source through `downloadResumable` via a wrapper type that adds `ResumesFromOffset` | red: stage-name assertion |
| `TestDownload_PlainSourceIgnoresPartials` | Change the plain branch to `u.discardOtherPartials(u.partialPath(req))` | red: partial still exists |
| `TestPartialKey` | Drop the `ID` write from `partialKey` | red (pinned value and "different id") |
| `TestPartialKey` | Drop `strings.ToLower` | red: case assertion |
| `TestNamingContract` | Change `prePartial` to `"." + ns + "-partial-"` | red |
| `TestUpdate_CallerSetOffsetIsRefused` | Delete the `req.Offset != 0` check | red |
| `TestRepair_NoJournalDiscardsStalePartials` | Remove the `discardStalePartials` call from `Repair` | red |
| `TestRepair_NoJournalDiscardsStalePartials` | Change `<= u.cfg.PartialRetention` to `< 0` in `discardStalePartials` | red: fresh partial removed |
| `TestRepair_PartialsAreLeftAloneWhileAnUpdateIsInFlight` | Move the `discardStalePartials` call to the top of `Repair`, before `if !exists(p.State)` | red |
| `TestNew_AppliesDefaults` | Delete the `PartialRetention` default in `withDefaults` | red |

- [ ] **Step 6: Stop: controller reviews; do not stage or commit**

---

### Task 3: `FileSource` — a resumable local-file source

**Files:**
- Create: `filesource.go`, `filesource_test.go`

**Interfaces:**
- Consumes (Task 2): `ResumableSource`, `Request.Offset`, `ErrResumeRejected`.
- Produces:
  - `func FileSource(path string) ResumableSource`
  - unexported `type localFile struct{ path string }`, `type ctxReader struct{ ctx context.Context; r io.Reader }`

  (Not named `fileSource`: `e2e_test.go` already declares a test type `fileSource` in package `denju`.)

- [ ] **Step 1: Write the failing tests**

Create `filesource_test.go`:

```go
package denju

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A compile-time proof: the retained-binary source must resume like any other.
var _ ResumableSource = FileSource("")

func writeSourceFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact")
	noErr(t, os.WriteFile(path, []byte(content), 0o600), "write the source file")
	return path
}

func TestFileSource_WritesTheWholeFileFromZero(t *testing.T) {
	path := writeSourceFile(t, "0123456789")
	var buf bytes.Buffer

	noErr(t, FileSource(path).Fetch(context.Background(), Request{}, &buf), "Fetch")
	eq(t, buf.String(), "0123456789", "the whole file")
}

func TestFileSource_WritesFromTheOffset(t *testing.T) {
	path := writeSourceFile(t, "0123456789")

	for _, tc := range []struct {
		offset int64
		want   string
	}{
		{offset: 4, want: "456789"},
		{offset: 10, want: ""}, // exactly complete: nothing left to send, and not an error
	} {
		var buf bytes.Buffer
		noErr(t, FileSource(path).Fetch(context.Background(), Request{Offset: tc.offset}, &buf), "Fetch")
		eq(t, buf.String(), tc.want, "bytes from the offset")
	}
}

func TestFileSource_OffsetPastTheEndIsRejected(t *testing.T) {
	path := writeSourceFile(t, "0123456789")
	var buf bytes.Buffer

	err := FileSource(path).Fetch(context.Background(), Request{Offset: 11}, &buf)
	isTrue(t, errors.Is(err, ErrResumeRejected), "an offset past the end cannot be resumed")
	eq(t, buf.Len(), 0, "nothing is written")
}

// A missing file is an ordinary failure, not a rejected resume: it says nothing
// about whether the bytes already downloaded are good.
func TestFileSource_MissingFileIsNotARejectedResume(t *testing.T) {
	var buf bytes.Buffer
	err := FileSource(filepath.Join(t.TempDir(), "gone")).Fetch(context.Background(), Request{}, &buf)
	isTrue(t, errors.Is(err, fs.ErrNotExist), "the cause is reachable")
	isFalse(t, errors.Is(err, ErrResumeRejected), "and it is not a rejected resume")
}

func TestFileSource_HonoursCancellation(t *testing.T) {
	path := writeSourceFile(t, "0123456789")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer

	err := FileSource(path).Fetch(ctx, Request{}, &buf)
	isTrue(t, errors.Is(err, context.Canceled), "a cancelled copy stops")
}

// Through the Updater, a FileSource is staged and verified like any source: a
// file that is not the promised artifact fails the digest.
func TestFileSource_IsVerifiedLikeAnyOtherSource(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	path := writeSourceFile(t, "not the artifact")

	_, err := u.download(context.Background(), resumeRequest(), FileSource(path))
	isTrue(t, errors.Is(err, ErrChecksumMismatch), "the wrong file is refused by the digest")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -count=1 -run TestFileSource`
Expected: FAIL to compile — `undefined: FileSource`.

- [ ] **Step 3: Implement**

Create `filesource.go`:

```go
package denju

import (
	"context"
	"fmt"
	"io"
	"os"
)

// FileSource returns a [ResumableSource] that reads the artifact from a file on
// the local filesystem.
//
// It exists for installing the binary kept by [Config.RetainPrevious]: pass it
// the path [Updater.PreviousBinary] reports, and moving back to that version
// reads no byte from any network. denju verifies what it reads against
// [Request.SHA256] exactly as for any other source, so pointing it at the wrong
// file fails the digest check rather than installing anything.
//
// An offset past the end of the file is rejected with [ErrResumeRejected]; an
// offset exactly at the end writes nothing and succeeds.
func FileSource(path string) ResumableSource { return localFile{path: path} }

type localFile struct{ path string }

func (localFile) ResumesFromOffset() {}

func (s localFile) Fetch(ctx context.Context, req Request, w io.Writer) error {
	f, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", s.path, err)
	}
	if req.Offset > info.Size() {
		return fmt.Errorf("%w: %s is %d bytes, fewer than the %d already downloaded",
			ErrResumeRejected, s.path, info.Size(), req.Offset)
	}
	if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s to byte %d: %w", s.path, req.Offset, err)
	}
	if _, err := io.Copy(w, ctxReader{ctx: ctx, r: f}); err != nil {
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	return nil
}

// ctxReader stops a local copy when its context ends. A file read does not
// block the way a network read does, so checking between reads is enough to
// honour DownloadTimeout and a caller shutting down.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -count=1 -run TestFileSource -v` then `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Mutation-verify (revert each by hand)**

| Test | Mutation | Expected |
|---|---|---|
| `TestFileSource_WritesTheWholeFileFromZero` | `f.Seek(req.Offset+1, io.SeekStart)` | red |
| `TestFileSource_WritesFromTheOffset` | Delete the `Seek` call | red (offset 4 returns whole file) |
| `TestFileSource_WritesFromTheOffset` | Change `req.Offset > info.Size()` to `>=` | red (offset 10 rejected) |
| `TestFileSource_OffsetPastTheEndIsRejected` | Delete the size check | red (no `ErrResumeRejected`) |
| `TestFileSource_MissingFileIsNotARejectedResume` | Wrap the open error with `ErrResumeRejected` (`fmt.Errorf("%w: open %s: %w", ErrResumeRejected, s.path, err)`) | red |
| `TestFileSource_HonoursCancellation` | Replace `ctxReader{ctx: ctx, r: f}` with `f` | red |
| `TestFileSource_IsVerifiedLikeAnyOtherSource` | In `downloadResumable`, skip `checkDigest` | red |

- [ ] **Step 6: Stop: controller reviews; do not stage or commit**

---

### Task 4: `Config.RetainPrevious` and `Updater.PreviousBinary`

**Files:**
- Create: `previous.go`, `previous_test.go`
- Modify: `config.go` (`RetainPrevious`, names `sufPrevious`, `sufPreviousRecord`, `tmpPrevious`), `state.go:98-143` (`paths.Previous`, `paths.PreviousRecord`, `pathsFor`), `attest.go:205-208` and imports, `repair.go:56-57` (committed case) and `repair.go:148-159` (`reconcileCommitted`), `updater.go:173-182` (`CleanupReported`) and `updater.go:193-204` (`removeLeftovers` comment), `attest_test.go:18-55` (`newAttestFixture`), `names_test.go`

**Interfaces:**
- Consumes (Task 2/3): nothing functional; `FileSource` is used only in Task 6.
- Produces:
  - `Config.RetainPrevious bool`
  - `names.sufPrevious` (`.<ns>-previous`), `names.sufPreviousRecord` (`.<ns>-previous.json`), `names.tmpPrevious` (`.<ns>-previous-*`)
  - `paths.Previous string`, `paths.PreviousRecord string`
  - `type previousRecord struct { Version string \`json:"version"\`; SHA256 string \`json:"sha256"\`; RetainedAt time.Time \`json:"retained_at"\` }`
  - `func (u *Updater) PreviousBinary() (path, sha256, version string, ok bool)`
  - `func readPreviousRecord(path string) (*previousRecord, error)`
  - `func (u *Updater) writePreviousRecord(version, sum string) error`
  - `func (u *Updater) retainPrevious(j *state) error`
  - `func (u *Updater) releaseRollbackCopy(j *state)`
  - `newAttestFixture(t *testing.T, phase string, apply ...func(*Config)) *attestFixture` (test helper, variadic added)

- [ ] **Step 1: Write the failing tests**

In `attest_test.go`, change `newAttestFixture` to accept `apply ...func(*Config)`: build the `Config` literal into a variable `cfg`, run `for _, fn := range apply { fn(&cfg) }`, then `New(cfg)`. In its `stageJournal` call add `OldSHA256: sha256Hex([]byte("old-binary")),`. Existing callers are unchanged.

In `names_test.go`: add `sufPrevious`, `sufPreviousRecord`, `tmpPrevious` to the case struct and compared list, with `acme` values `".acme-previous"`, `".acme-previous.json"`, `".acme-previous-*"` and `widget-co` values `".widget-co-previous"`, `".widget-co-previous.json"`, `".widget-co-previous-*"`. In `TestNamingContractPaths` add to both the unix and windows expectations `Previous: "/opt/app/myprog.acme-previous"` and `PreviousRecord: "/opt/app/myprog.acme-previous.json"` (no `.exe` on Windows: the file is never executed in place), and add both fields to `expectPaths`' compared list.

Create `previous_test.go`:

```go
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
// journal until retention succeeds.
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

	noErr(t, os.RemoveAll(f.u.paths.Previous), "unblock")
	f.u.CleanupReported()
	isFalse(t, exists(f.u.paths.State), "the journal goes once retention succeeds")
	eq(t, readFileString(t, f.u.paths.Previous), "old-binary", "retained at last")
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -count=1`
Expected: FAIL to compile — `unknown field RetainPrevious`, `u.paths.Previous undefined`, `u.PreviousBinary undefined`, `n.sufPrevious undefined`.

- [ ] **Step 3: Implement**

`config.go` — add to `Config` after `Cooldown`:

```go
	// RetainPrevious keeps the binary an update replaced once that update is
	// committed, as <binary>.<namespace>-previous, instead of deleting it.
	// [Updater.PreviousBinary] reports it and [FileSource] installs it, so moving
	// back to the version the program just left reads nothing from any network.
	//
	// One generation is kept: the next committed update replaces it. A rollback
	// does not touch it - the program is back on the binary the update replaced,
	// and the retained one is still the one before that. It costs one binary's
	// worth of disk for good, and while an update is in flight the running
	// binary, the rollback copy and the retained one sit side by side, on top of
	// the download.
	//
	// Never execute the retained file in place. On Windows a running .exe cannot
	// be replaced, so the next commit could not retain over it.
	RetainPrevious bool
```

`names` — after `sufRecord`: `sufPrevious string // .<ns>-previous` and `sufPreviousRecord string // .<ns>-previous.json`; after `tmpRecord`: `tmpPrevious string // .<ns>-previous-*`. `newNames` — `sufPrevious: "." + ns + "-previous"`, `sufPreviousRecord: "." + ns + "-previous.json"`, `tmpPrevious: "." + ns + "-previous-*"`.

`state.go` — add to `paths` after `Record`:

```go
	// Previous is the binary kept by Config.RetainPrevious after a commit, and
	// PreviousRecord describes it (version and SHA-256). Neither is a leftover:
	// no cleanup path removes them.
	Previous       string
	PreviousRecord string
```

and to `pathsFor`'s literal: `Previous: binaryPath + n.sufPrevious,` and `PreviousRecord: binaryPath + n.sufPreviousRecord,`. Fix the phaseCommitted comment (`state.go:29-31`) to "the rollback copy has been discarded, or kept as the retained previous binary".

Create `previous.go`:

```go
package denju

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// previousRecord describes the binary kept by Config.RetainPrevious. It lives
// beside that binary, is deleted before the binary is replaced, and is written
// only once its binary is in place - so a record on disk always describes the
// file next to it.
//
// The JSON field names are an on-disk contract between consecutive versions of
// a program, like the journal's.
type previousRecord struct {
	Version    string    `json:"version"`
	SHA256     string    `json:"sha256"`
	RetainedAt time.Time `json:"retained_at"`
}

// PreviousBinary reports the binary kept by [Config.RetainPrevious]: its path,
// its hex SHA-256 and the version it was. ok is false when nothing is retained.
//
// Compare sha256 with an update's digest; when they match, install it with
// [FileSource] and no byte crosses a network. The digest is re-checked during
// the install like any other download, so a retained file that has changed on
// disk is refused rather than installed.
//
// It has no error to return, so a record that cannot be read, or a record whose
// binary is gone, is logged at ERROR and reported as nothing retained - never
// as something retained.
func (u *Updater) PreviousBinary() (path, sha256, version string, ok bool) {
	rec, err := readPreviousRecord(u.paths.PreviousRecord)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			u.log(LevelError, "could not read the record of the retained previous binary", "err", err)
		}
		return "", "", "", false
	}
	if !exists(u.paths.Previous) {
		u.log(LevelError, "the retained previous binary is missing although its record is present",
			"path", u.paths.Previous)
		return "", "", "", false
	}
	return u.paths.Previous, rec.SHA256, rec.Version, true
}

func readPreviousRecord(path string) (*previousRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the record of the retained previous binary: %w", err)
	}
	var rec previousRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("the record of the retained previous binary at %s is corrupt: %w", path, err)
	}
	return &rec, nil
}

// writePreviousRecord atomically writes the record: temp file, fsync, rename,
// best-effort directory sync - the journal's discipline.
func (u *Updater) writePreviousRecord(version, sum string) error {
	data, err := json.MarshalIndent(previousRecord{Version: version, SHA256: sum, RetainedAt: time.Now()}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of the retained previous binary: %w", err)
	}
	dir := filepath.Dir(u.paths.PreviousRecord)
	tmp, err := os.CreateTemp(dir, u.n.tmpPrevious)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write the record of the retained previous binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush the record of the retained previous binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize the record of the retained previous binary: %w", err)
	}
	if err := os.Rename(tmpPath, u.paths.PreviousRecord); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write the record of the retained previous binary: %w", err)
	}
	syncDir(dir)
	return nil
}

// retainPrevious keeps a committed update's rollback copy as the retained
// previous binary, and records what it is, from the journal's OldVersion and
// OldSHA256.
//
// It is idempotent and resumable, because the commit is not the only place it
// runs: a process can stop anywhere inside it, and Repair and CleanupReported
// run it again for as long as the committed journal exists. The order is what
// makes that safe. The old record is deleted BEFORE the binary it describes is
// replaced, and the new record written only AFTER its binary is in place, so no
// record ever describes a file other than the one beside it. The worst
// interrupted state is a retained binary with no record, which PreviousBinary
// reports as nothing and the next call finishes - after checking by content that
// it really is the journal's old binary.
func (u *Updater) retainPrevious(j *state) error {
	p := u.paths
	if exists(p.RollbackBinary) {
		if err := os.Remove(p.PreviousRecord); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the record of the binary being replaced: %w", err)
		}
		// An atomic rename-over: the older generation is replaced in one step.
		if err := os.Rename(p.RollbackBinary, p.Previous); err != nil {
			return fmt.Errorf("retain the previous binary: %w", err)
		}
		return u.writePreviousRecord(j.OldVersion, j.OldSHA256)
	}
	if _, err := readPreviousRecord(p.PreviousRecord); err == nil {
		return nil // retained and recorded already
	}
	if !exists(p.Previous) {
		return nil // nothing was kept for this update
	}
	sum, err := sha256File(p.Previous)
	if err != nil {
		return fmt.Errorf("hash the retained previous binary: %w", err)
	}
	if !strings.EqualFold(sum, j.OldSHA256) {
		// No record can describe it, so nothing can ever install it: it is only
		// disk. Deleted, and said so.
		if err := os.Remove(p.Previous); err != nil {
			return fmt.Errorf("remove a retained binary no record describes: %w", err)
		}
		u.log(LevelWarn, "deleted a retained binary that no record describes", "path", p.Previous)
		return nil
	}
	return u.writePreviousRecord(j.OldVersion, j.OldSHA256)
}

// releaseRollbackCopy disposes of the rollback copy once an update is proven.
// Without RetainPrevious it is deleted, as it always was. With it, it becomes
// the retained previous binary - and a failure is logged, not returned: the
// update IS committed, and the journal stays until CleanupReported, which tries
// again.
func (u *Updater) releaseRollbackCopy(j *state) {
	if !u.cfg.RetainPrevious {
		_ = os.Remove(u.paths.RollbackBinary)
		return
	}
	if err := u.retainPrevious(j); err != nil {
		u.log(LevelError, "could not retain the previous binary; it stays as the rollback copy and is retried",
			"err", err)
	}
}
```

`attest.go` `decide` — replace `attest.go:205-208`:

```go
		// The rollback copy is released only here - the one point at which the
		// new version has actually been proven. Released means deleted, or kept
		// as the retained previous binary under Config.RetainPrevious.
		u.releaseRollbackCopy(j)
```

Remove the now-unused `"os"` import from `attest.go`.

`repair.go` — the committed case:

```go
	case phaseCommitted:
		if u.cfg.RetainPrevious {
			// A retention interrupted by a crash is finished here; the journal
			// is still on disk until CleanupReported, so this runs until it lands.
			if err := u.retainPrevious(j); err != nil {
				u.log(LevelError, "could not retain the previous binary; it stays as the rollback copy and is retried",
					"err", err)
			}
		}
		return u.repairDecided(j, StatusSucceeded, "")
```

and in `reconcileCommitted` replace `_ = os.Remove(u.paths.RollbackBinary)` with `u.releaseRollbackCopy(j)`.

`updater.go` `CleanupReported`:

```go
func (u *Updater) CleanupReported() {
	if !exists(u.paths.State) {
		return
	}
	j, err := readState(u.paths.State)
	if err != nil || (j.Phase != phaseCommitted && j.Phase != phaseRolledBack) {
		return
	}
	if j.Phase == phaseCommitted && u.cfg.RetainPrevious {
		if err := u.retainPrevious(j); err != nil {
			// The journal is the only thing that says the rollback copy belongs
			// to a committed update. Deleting it now would lose the binary.
			u.log(LevelError, "could not retain the previous binary; keeping the journal so the next start tries again",
				"err", err)
			return
		}
	}
	u.removeLeftovers()
}
```

and extend `removeLeftovers`' comment: "It never removes the retained previous binary or its record (Config.RetainPrevious): they are not leftovers of this update, they are the product of the last one."

- [ ] **Step 4: Run the tests to verify they pass, then the whole suite**

Run: `go test ./... -count=1 -run 'TestCommit_|TestRollback_|TestCleanupReported|TestRepair_SparesTheRetainedPrevious|TestRetainPrevious_|TestPreviousBinary_|TestPreviousRecordWireCompatibility|TestNamingContract' -v`
Expected: PASS.
Run: `go test ./... -count=1` then `GOOS=windows go vet ./...`
Expected: PASS; vet clean.

- [ ] **Step 5: Mutation-verify (revert each by hand)**

| Test | Mutation | Expected |
|---|---|---|
| `TestCommit_RetainPreviousKeepsTheReplacedBinary` | In `releaseRollbackCopy`, ignore the flag (always `os.Remove`) | red |
| `TestCommit_RetainPreviousKeepsTheReplacedBinary` | Write the record with `j.TargetVersion` instead of `j.OldVersion` | red: version |
| `TestCommit_WithoutRetainPreviousNothingIsKept` | In `releaseRollbackCopy`, ignore the flag (always retain) | red |
| `TestCommit_RetainPreviousKeepsOneGeneration` | Rename to `p.Previous + "." + time.Now().Format("20060102150405")` instead of `p.Previous` | red |
| `TestRollback_LeavesTheRetainedPreviousAlone` | Add `_ = os.Remove(u.paths.Previous)` at the top of `rollbackAndRestart` | red |
| `TestCleanupReported_SparesTheRetainedPrevious` | Add `_ = os.Remove(u.paths.Previous)` and `_ = os.Remove(u.paths.PreviousRecord)` to `removeLeftovers` | red |
| `TestRepair_SparesTheRetainedPrevious/no_journal` | Add `_ = os.Remove(p.Previous)` in `Repair`'s no-journal block | red |
| `TestRepair_SparesTheRetainedPrevious/stale_journal` and `…/stale_corrupt_journal` | Same `removeLeftovers` mutation as above | red |
| `TestRetainPrevious_FinishesAnInterruptedRetention/stopped_before_the_rename` | Delete the `retainPrevious` call from `Repair`'s committed case | red |
| `TestRetainPrevious_FinishesAnInterruptedRetention/an_undescribed_binary…` | Skip the digest comparison (write the record unconditionally) | red |
| `TestRetainPrevious_FinishesAnInterruptedRetention/a_recorded_older_generation…` | Delete the `readPreviousRecord(...) == nil → return nil` early exit | red (gen-0 hashed, mismatches, deleted) |
| (ordering) | Swap the order: rename first, then remove the old record | ~~Equivalent mutant~~ **Not equivalent (task review, 2026-09-17):** a failure of the record removal can be injected (a directory at the record path), and the swapped order then replaces the older generation while its record survives. Killed by the injected-failure commit test added in fix round 1. |
| `TestCommit_RetentionFailureDoesNotReverseTheCommit` | `releaseRollbackCopy` returns the error and `decide` returns it | red: "the commit itself stands" |
| `TestCommit_RetentionFailureDoesNotReverseTheCommit` | In `CleanupReported`, log but fall through to `removeLeftovers` | red: journal gone / rollback copy lost |
| `TestPreviousBinary_ReportsOnlyWhatIsReallyThere/record_without_its_binary` | Delete the `exists(u.paths.Previous)` check | red |
| `TestPreviousBinary_ReportsOnlyWhatIsReallyThere/corrupt_record` | Delete the ERROR log for a non-`ErrNotExist` error | red |
| `TestPreviousBinary_ReportsOnlyWhatIsReallyThere/nothing_retained` | Log ERROR for `ErrNotExist` too | red |
| `TestPreviousRecordWireCompatibility` | Rename the tag `retained_at` → `retainedAt` | red |
| `TestNamingContract` / `TestNamingContractPaths` | Change `sufPrevious` to `".previous"` | red |

- [ ] **Step 6: Stop: controller reviews; do not stage or commit**

---

### Task 5: `Config.CooldownScope` — hold only the rolled-back version

**Files:**
- Modify: `config.go` (`CooldownScope` type, constants, `Config.CooldownScope`, `Config.Cooldown` doc), `updater.go` (`New` validation + `store.scope`, `InCooldown` doc, `InCooldownFor`), `record.go` (`Outcome` fields, `store.scope`, `record`, `inCooldown`, `inCooldownFor`, `check`), `update.go:43-57`, `attest.go:139-141` (Rollback doc mentions Cooldown), `record_test.go`, `update_test.go`, `updater_test.go`

**Interfaces:**
- Consumes: nothing from Tasks 1-4 beyond the existing store.
- Produces:
  - `type CooldownScope int`; `const CooldownAnyUpdate CooldownScope = 0`, `CooldownRolledBackVersion CooldownScope = 1`
  - `Config.CooldownScope CooldownScope`
  - `Outcome.RolledBackVersion string \`json:"rolled_back_version,omitempty"\``, `Outcome.RolledBackAt time.Time \`json:"rolled_back_at,omitzero"\``
  - `store.scope CooldownScope`
  - `func (s *store) inCooldownFor(target string, now time.Time) (bool, time.Duration, error)`
  - `func (u *Updater) InCooldownFor(targetVersion string, now time.Time) (bool, time.Duration, error)`

- [ ] **Step 1: Write the failing tests**

Append to `record_test.go`:

```go
func newScopedStore(t *testing.T) *store {
	t.Helper()
	s := newTestStore(t)
	s.scope = CooldownRolledBackVersion
	return s
}

func rolledBack(at time.Time, id, to string) *Outcome {
	return &Outcome{At: at, ID: id, FromVersion: "1.4.0", ToVersion: to, Status: StatusRolledBack, Error: "soak failed"}
}

func TestScopedCooldown_HoldsOnlyTheRolledBackVersion(t *testing.T) {
	s := newScopedStore(t)
	now := time.Now()
	noErr(t, s.record(rolledBack(now.Add(-time.Minute), "cmd-1", "1.5.0")), "record the rollback")

	held, left, err := s.inCooldownFor("1.5.0", now)
	noErr(t, err, "inCooldownFor")
	isTrue(t, held, "the rolled-back version is held")
	isTrue(t, left > testCooldown-2*time.Minute && left <= testCooldown-time.Minute, "remaining is measured from the rollback")

	held, _, err = s.inCooldownFor("1.6.0", now)
	noErr(t, err, "inCooldownFor")
	isFalse(t, held, "any other target is admitted at once")

	held, _, err = s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isTrue(t, held, "inCooldown reports that some version is held")
}

// The record is the LAST attempt, so every later write must carry the hold
// forward - or a failed download of something else would erase it, and a
// refusal would restart it.
func TestScopedCooldown_LaterAttemptsCarryTheHoldForward(t *testing.T) {
	s := newScopedStore(t)
	now := time.Now()
	rolledAt := now.Add(-5 * time.Minute)
	noErr(t, s.record(rolledBack(rolledAt, "cmd-1", "1.5.0")), "rollback")
	noErr(t, s.record(&Outcome{At: now.Add(-4 * time.Minute), ID: "cmd-2", ToVersion: "1.6.0", Status: StatusFailed}), "a failed attempt")
	noErr(t, s.record(&Outcome{At: now.Add(-3 * time.Minute), ID: "cmd-3", ToVersion: "1.5.0", Status: StatusRefusedCooldown}), "a refusal")
	noErr(t, s.record(&Outcome{At: now.Add(-2 * time.Minute), ID: "cmd-4", ToVersion: "1.6.0", Status: StatusSucceeded}), "a success")

	o, err := s.load()
	noErr(t, err, "load")
	eq(t, o.RolledBackVersion, "1.5.0", "the held version survives")
	isTrue(t, o.RolledBackAt.Equal(rolledAt), "the hold's anchor does not move")

	held, left, err := s.inCooldownFor("1.5.0", now)
	noErr(t, err, "inCooldownFor")
	isTrue(t, held, "still held")
	isTrue(t, left <= testCooldown-5*time.Minute, "the window runs from the rollback, not from the latest attempt")
}

func TestScopedCooldown_ANewRollbackReplacesTheHold(t *testing.T) {
	s := newScopedStore(t)
	now := time.Now()
	noErr(t, s.record(rolledBack(now.Add(-5*time.Minute), "cmd-1", "1.5.0")), "first rollback")
	noErr(t, s.record(rolledBack(now.Add(-time.Minute), "cmd-2", "1.6.0")), "second rollback")

	held, _, err := s.inCooldownFor("1.5.0", now)
	noErr(t, err, "inCooldownFor")
	isFalse(t, held, "the older rolled-back version is released")
	held, _, err = s.inCooldownFor("1.6.0", now)
	noErr(t, err, "inCooldownFor")
	isTrue(t, held, "the newest rolled-back version is held")
}

func TestScopedCooldown_Expires(t *testing.T) {
	s := newScopedStore(t)
	now := time.Now()
	noErr(t, s.record(rolledBack(now.Add(-testCooldown-time.Minute), "cmd-1", "1.5.0")), "an old rollback")

	held, _, err := s.inCooldownFor("1.5.0", now)
	noErr(t, err, "inCooldownFor")
	isFalse(t, held, "an expired hold admits the version again")
}

// Removing the cooldown <= 0 guard is an equivalent mutant here: the window
// arithmetic already admits everything at zero. It is kept as a behaviour pin.
func TestScopedCooldown_ZeroCooldownHoldsNothing(t *testing.T) {
	s := newScopedStore(t)
	s.cooldown = 0
	now := time.Now()
	noErr(t, s.record(rolledBack(now, "cmd-1", "1.5.0")), "rollback")

	held, _, err := s.inCooldownFor("1.5.0", now)
	noErr(t, err, "inCooldownFor")
	isFalse(t, held, "no cooldown, no hold")
}

// Under the scoped cooldown a record that cannot be parsed would silently drop
// the hold if it were overwritten, so every write fails loudly instead. The
// default scope overwrites exactly as v0.3.0 did.
func TestScopedCooldown_CorruptRecordFailsEveryWrite(t *testing.T) {
	scoped := newScopedStore(t)
	noErr(t, os.WriteFile(scoped.path, []byte("{"), 0o600), "corrupt")
	wantErrContaining(t, scoped.record(&Outcome{At: time.Now(), ID: "cmd-2", ToVersion: "1.6.0", Status: StatusSucceeded}),
		"corrupt", "a write over a corrupt record under the scoped cooldown")

	legacy := newTestStore(t)
	noErr(t, os.WriteFile(legacy.path, []byte("{"), 0o600), "corrupt")
	noErr(t, legacy.record(&Outcome{At: time.Now(), ID: "cmd-2", ToVersion: "1.6.0", Status: StatusSucceeded}),
		"the default scope overwrites exactly as before")
}

// The default scope is v0.3.0: every update is held, and the record carries no
// new fields.
func TestDefaultCooldownScope_IsUnchanged(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	noErr(t, s.record(rolledBack(now.Add(-time.Minute), "cmd-1", "1.5.0")), "rollback")

	held, _, err := s.inCooldownFor("1.6.0", now)
	noErr(t, err, "inCooldownFor")
	isTrue(t, held, "the default scope still holds every update")

	data, err := os.ReadFile(s.path)
	noErr(t, err, "read the record")
	isFalse(t, strings.Contains(string(data), "rolled_back_version"), "no rolled_back_version key")
	isFalse(t, strings.Contains(string(data), "rolled_back_at"), "no rolled_back_at key")
}

func TestOutcomeWireCompatibility_RollbackAnchor(t *testing.T) {
	const fixture = `{
  "at": "2026-09-17T08:09:44Z",
  "command_id": "cmd-42",
  "from_version": "1.3.2",
  "to_version": "1.4.0",
  "status": "rolled_back",
  "reported": false,
  "cooldown_at": "2026-09-17T08:09:44Z",
  "rolled_back_version": "1.4.0",
  "rolled_back_at": "2026-09-17T08:09:44Z"
}`
	s := newScopedStore(t)
	noErr(t, os.WriteFile(s.path, []byte(fixture), 0o600), "write the fixture")

	o, err := s.load()
	noErr(t, err, "parse")
	eq(t, o.RolledBackVersion, "1.4.0", "rolled_back_version")
	eq(t, o.RolledBackAt.UTC().Format(time.RFC3339), "2026-09-17T08:09:44Z", "rolled_back_at")
}
```

(Add `"strings"` to `record_test.go` imports if absent.)

Append to `update_test.go`:

```go
func scopedCooldown(c *Config) {
	c.Cooldown = testCooldown
	c.CooldownScope = CooldownRolledBackVersion
}

// The version that was just rolled back is not re-applied, and nothing is
// downloaded to find that out.
func TestUpdate_ScopedCooldownRefusesTheRolledBackVersion(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t, scopedCooldown)
	noErr(t, r.u.records.record(&Outcome{
		At: time.Now().Add(-time.Minute), ID: "cmd-0",
		FromVersion: "1.4.0", ToVersion: "1.5.0", Status: StatusRolledBack,
	}), "seed a rollback of 1.5.0")

	got := r.u.Update(context.Background(), r.req(), r.src) // target 1.5.0
	eq(t, got.Status, StatusRefusedCooldown, "status")
	hasSubstr(t, got.Error, "1.5.0 was rolled back", "error")
	isTrue(t, got.ProgramIntact, "a refusal leaves the program intact")
	eq(t, r.src.calls, 0, "nothing is downloaded")
}

// A corrective update lands at once, which is the reason the scope exists.
func TestUpdate_ScopedCooldownAdmitsACorrectiveUpdate(t *testing.T) {
	withGOOS(t, "linux")
	r := newUpdateRig(t, scopedCooldown)
	noErr(t, r.u.records.record(&Outcome{
		At: time.Now().Add(-time.Minute), ID: "cmd-0",
		FromVersion: "1.4.0", ToVersion: "1.4.9", Status: StatusRolledBack,
	}), "seed a rollback of 1.4.9")

	got := r.u.Update(context.Background(), r.req(), r.src) // target 1.5.0
	eq(t, got.Status, StatusSucceeded, "a different target is admitted")
}
```

Append to `updater_test.go`:

```go
func TestNew_RejectsAnUnknownCooldownScope(t *testing.T) {
	_, err := New(Config{Namespace: "app", Version: "1.0.0", BinaryPath: "/opt/app/prog", CooldownScope: CooldownScope(7)})
	wantErrContaining(t, err, "CooldownScope", "an unknown scope")
}

func TestInCooldownFor(t *testing.T) {
	u := testUpdater(t, scopedCooldown)
	now := time.Now()
	noErr(t, u.records.record(&Outcome{At: now.Add(-time.Minute), ID: "cmd-0", ToVersion: "1.5.0", Status: StatusRolledBack}), "seed")

	held, _, err := u.InCooldownFor("1.5.0", now)
	noErr(t, err, "InCooldownFor")
	isTrue(t, held, "the rolled-back version")
	held, _, err = u.InCooldownFor("1.6.0", now)
	noErr(t, err, "InCooldownFor")
	isFalse(t, held, "another version")
}
```

(`testUpdater` sets no `RecordPath`; the record lands beside the throwaway binary, which is fine.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -count=1`
Expected: FAIL to compile — `undefined: CooldownRolledBackVersion`, `s.scope undefined`, `o.RolledBackVersion undefined`, `s.inCooldownFor undefined`, `u.InCooldownFor undefined`.

- [ ] **Step 3: Implement**

`config.go` — add before `Config`:

```go
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
	// Versions are matched by string. Under this scope every write to the
	// outcome record reads the previous record first, so a record that cannot
	// be parsed fails every write - including a commit's - rather than silently
	// dropping the hold.
	CooldownRolledBackVersion
)
```

Extend the `Cooldown` doc with one paragraph: "[Config.CooldownScope] decides what is held: every update (the default) or only the version most recently rolled back." Add after `Cooldown`:

```go
	// CooldownScope is what Cooldown holds back. The zero value,
	// [CooldownAnyUpdate], is the behaviour Cooldown has always had.
	CooldownScope CooldownScope
```

`updater.go` `New` — after the `cfg.Version == ""` check:

```go
	switch cfg.CooldownScope {
	case CooldownAnyUpdate, CooldownRolledBackVersion:
	default:
		return nil, fmt.Errorf("denju: Config.CooldownScope %d is not a known scope", cfg.CooldownScope)
	}
```

and add `scope: cfg.CooldownScope,` to the `store` literal. Replace `InCooldown`'s doc and add `InCooldownFor`:

```go
// InCooldown reports whether an update would be refused right now, and how long
// remains. Under [CooldownAnyUpdate] that is any update; under
// [CooldownRolledBackVersion] it is whether some version is currently held - use
// [Updater.InCooldownFor] to ask about a particular one. It is always false when
// [Config.Cooldown] is zero.
//
// [Updater.Update] checks this itself; the method is exported so a caller can
// answer for itself without starting an update.
func (u *Updater) InCooldown(now time.Time) (bool, time.Duration, error) {
	return u.records.inCooldown(now)
}

// InCooldownFor reports whether an update to targetVersion would be refused
// right now, and how long remains. Under [CooldownAnyUpdate] the target makes no
// difference.
func (u *Updater) InCooldownFor(targetVersion string, now time.Time) (bool, time.Duration, error) {
	return u.records.inCooldownFor(targetVersion, now)
}
```

`record.go` — add to `Outcome` after `CooldownAt`:

```go
	// RolledBackVersion and RolledBackAt name the version most recently rolled
	// back and when. They are the anchor [CooldownRolledBackVersion] measures
	// from, and every later record carries them forward until another rollback
	// replaces them. Empty under any other scope.
	//
	// Maintained by the store; do not set them by hand.
	RolledBackVersion string    `json:"rolled_back_version,omitempty"`
	RolledBackAt      time.Time `json:"rolled_back_at,omitzero"`
```

`store` gains `scope CooldownScope`. Replace `record`:

```go
func (s *store) record(o *Outcome) error {
	// The previous record is needed for a refusal (to inherit its anchor), and
	// under the scoped cooldown for EVERY write (to carry the hold forward).
	// Loading it only then keeps the default scope byte-for-byte as it was: a
	// corrupt record is simply overwritten by a completed attempt.
	var prev *Outcome
	if o.Status == StatusRefusedCooldown || s.scope == CooldownRolledBackVersion {
		var err error
		if prev, err = s.load(); err != nil {
			return err
		}
	}
	if o.Status == StatusRefusedCooldown {
		if prev != nil {
			o.CooldownAt = prev.CooldownAt
		}
	} else {
		o.CooldownAt = o.At
	}
	if s.scope == CooldownRolledBackVersion {
		switch {
		case o.Status == StatusRolledBack:
			o.RolledBackVersion, o.RolledBackAt = o.ToVersion, o.At
		case prev != nil:
			o.RolledBackVersion, o.RolledBackAt = prev.RolledBackVersion, prev.RolledBackAt
		}
	}
	return s.save(o)
}
```

(Extend its doc comment with one paragraph on the rollback anchor rule: only a rollback moves it; everything else, refusals included, inherits it.)

Replace `inCooldown`:

```go
// inCooldown reports whether ANY update would be refused now: every update
// under CooldownAnyUpdate, the held version under CooldownRolledBackVersion.
func (s *store) inCooldown(now time.Time) (bool, time.Duration, error) {
	return s.check(now, func(*Outcome) bool { return true })
}

// inCooldownFor reports whether an update to target would be refused now.
func (s *store) inCooldownFor(target string, now time.Time) (bool, time.Duration, error) {
	return s.check(now, func(o *Outcome) bool { return o.RolledBackVersion == target })
}

// check measures the window from the anchor the scope uses. Refusals in between
// are invisible here - record keeps them from moving either anchor.
func (s *store) check(now time.Time, held func(*Outcome) bool) (bool, time.Duration, error) {
	if s.cooldown <= 0 {
		return false, 0, nil
	}
	o, err := s.load()
	if err != nil {
		return false, 0, err
	}
	if o == nil {
		return false, 0, nil
	}
	anchor := o.CooldownAt
	if s.scope == CooldownRolledBackVersion {
		if o.RolledBackVersion == "" || !held(o) {
			return false, 0, nil
		}
		anchor = o.RolledBackAt
	}
	if anchor.IsZero() {
		return false, 0, nil
	}
	elapsed := now.Sub(anchor)
	// A negative elapsed means the record is dated in the future - a clock that
	// jumped backwards, or a record copied from another host. Holding an update
	// back until wall-clock time catches up could mean holding it for years, so
	// an unusable anchor is treated as no anchor.
	if elapsed < 0 || elapsed >= s.cooldown {
		return false, 0, nil
	}
	return true, s.cooldown - elapsed, nil
}
```

`update.go` — the cooldown block:

```go
	inCooldown, left, err := u.records.inCooldownFor(req.TargetVersion, time.Now())
	if err != nil {
		return u.fail(req, "could not read the update record: "+err.Error())
	}
	if inCooldown {
		msg := fmt.Sprintf("another update completed less than %s ago; refusing for another %s",
			u.cfg.Cooldown.Round(time.Minute), left.Round(time.Second))
		if u.cfg.CooldownScope == CooldownRolledBackVersion {
			msg = fmt.Sprintf("version %s was rolled back less than %s ago; refusing it for another %s",
				req.TargetVersion, u.cfg.Cooldown.Round(time.Minute), left.Round(time.Second))
		}
		u.log(LevelWarn, "refused by cooldown",
			"target_version", req.TargetVersion,
			"current_version", u.cfg.Version,
			"remaining", left)
		return u.record(req, StatusRefusedCooldown, msg)
	}
```

`attest.go` `Rollback` doc: change "Enable [Config.Cooldown] so a rejected version is not immediately reapplied." to "Enable [Config.Cooldown] - with [CooldownRolledBackVersion] to hold only this version - so a rejected version is not immediately reapplied."

- [ ] **Step 4: Run the tests to verify they pass, then the whole suite**

Run: `go test ./... -count=1 -run 'TestScopedCooldown|TestDefaultCooldownScope|TestOutcomeWireCompatibility|TestUpdate_ScopedCooldown|TestNew_RejectsAnUnknownCooldownScope|TestInCooldownFor|TestInCooldown|TestUpdate_Cooldown|TestUpdate_ExpiredCooldown' -v`
Expected: PASS.
Run: `go test ./... -count=1`
Expected: PASS (every existing cooldown test is default scope and unchanged).

- [ ] **Step 5: Mutation-verify (revert each by hand)**

| Test | Mutation | Expected |
|---|---|---|
| `TestScopedCooldown_HoldsOnlyTheRolledBackVersion` | `inCooldownFor`'s predicate returns `true` for every outcome | red: "any other target is admitted" |
| `TestScopedCooldown_LaterAttemptsCarryTheHoldForward` | Delete the `case prev != nil:` carry-forward | red |
| `TestScopedCooldown_LaterAttemptsCarryTheHoldForward` | Under scope, make a refusal set `o.RolledBackAt = o.At` | red: "anchor does not move" |
| `TestScopedCooldown_LaterAttemptsCarryTheHoldForward` | In `check`, delete `anchor = o.RolledBackAt` (the scoped branch measures from `CooldownAt`) | red: "the window runs from the rollback" |
| `TestScopedCooldown_ANewRollbackReplacesTheHold` | Swap the switch order so `prev != nil` wins over `StatusRolledBack` | red |
| `TestScopedCooldown_Expires` | Change `elapsed >= s.cooldown` to `elapsed >= 2*s.cooldown` | red |
| `TestScopedCooldown_ZeroCooldownHoldsNothing` | Remove the `cooldown <= 0` guard | **Equivalent mutant** (stated in the test). Record it as equivalent; the test is a behaviour pin |
| `TestScopedCooldown_CorruptRecordFailsEveryWrite` | Under scope, ignore the load error (`prev = nil`) | red: scoped half |
| `TestScopedCooldown_CorruptRecordFailsEveryWrite` | Load `prev` unconditionally for every scope | red: legacy half |
| `TestDefaultCooldownScope_IsUnchanged` | Maintain the rollback anchor for every scope (drop the `s.scope ==` condition on the anchor block) | red: key present |
| `TestDefaultCooldownScope_IsUnchanged` | Swap the constant order so `CooldownRolledBackVersion` is `iota` 0 | red |
| `TestDefaultCooldownScope_IsUnchanged` | Change `omitzero` to `omitempty` on `RolledBackAt` | red: `rolled_back_at` key present (zero time is not "empty") |
| `TestOutcomeWireCompatibility_RollbackAnchor` | Rename tag `rolled_back_version` → `rolled_back` | red |
| `TestUpdate_ScopedCooldownAdmitsACorrectiveUpdate` | In `Update`, call `inCooldown(time.Now())` instead of `inCooldownFor` | red (the same mutation leaves `…RefusesTheRolledBackVersion` green: expected, since a version is held there) |
| `TestUpdate_ScopedCooldownRefusesTheRolledBackVersion` | Delete the scoped `msg` override | red: message |
| `TestNew_RejectsAnUnknownCooldownScope` | Delete the scope validation | red |
| `TestInCooldownFor` | `InCooldownFor` calls `u.records.inCooldown(now)` | red |

- [ ] **Step 6: Stop: controller reviews; do not stage or commit**

---

### Task 6: End-to-end proof with real built programs

**Files:**
- Modify: `e2e_test.go`

**Interfaces:**
- Consumes: `FileSource`, `ResumableSource`, `ErrDownloadFailed`, `Result.Cause`, `Updater.partialPath`, `Config.RetainPrevious`, `Config.CooldownScope`, `CooldownRolledBackVersion`, `Updater.PreviousBinary` (Tasks 1-5); existing `buildProgram`, `newE2E`, `fileSource`, `withStubbedExec`, `sha256Hex`.
- Produces: `func (e *e2eEnv) updaterWith(t *testing.T, version string, apply func(*Config)) *Updater`; test type `breakingSource`.

- [ ] **Step 1: Write the tests**

In `e2e_test.go`, replace `updaterFor` with:

```go
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
```

Add `"errors"` and `"slices"` to its imports, then append:

```go
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
```

- [ ] **Step 2: Run the tests**

Run: `go test ./... -count=1 -run TestEndToEnd -v`
Expected: PASS (Tasks 1-5 are in place, so these pass immediately; the mutations below are what prove they bite).

- [ ] **Step 3: Mutation-verify (revert each by hand)**

| Test | Mutation | Expected |
|---|---|---|
| `TestEndToEnd_RetainAndRollBackLocally` | In `releaseRollbackCopy`, always `os.Remove` | red: "1.0.0 is retained" |
| `TestEndToEnd_RetainAndRollBackLocally` | In `CleanupReported`, add `_ = os.Remove(u.paths.Previous)` before `removeLeftovers` | red |
| `TestEndToEnd_RetainAndRollBackLocally` | In `retainPrevious`, record `j.NewSHA256` instead of `j.OldSHA256` | red: digest |
| `TestEndToEnd_RolledBackVersionIsHeldAndAFixIsAdmitted` | In `Update`, call `inCooldown` instead of `inCooldownFor` | red: the fix is refused |
| `TestEndToEnd_RolledBackVersionIsHeldAndAFixIsAdmitted` | Delete the `StatusRolledBack` case in `store.record`'s anchor switch | red: 2.0.0 admitted |
| `TestEndToEnd_InterruptedDownloadResumes` | In `downloadResumable`, delete the partial on a non-rejected Fetch error | red: stat fails |
| `TestEndToEnd_InterruptedDownloadResumes` | `req.Offset = 0` in `downloadResumable` | red: offsets / digest |
| `TestEndToEnd_InterruptedDownloadResumes` | Remove `CooldownScope` from `sdkLike` | red: retry refused by the any-update cooldown (proves the SDK must opt in) |

- [ ] **Step 4: Stop: controller reviews; do not stage or commit**

---

### Task 7: Documentation, changelog, and release readiness (stops before tagging)

**Files:**
- Modify: `README.md`, `doc.go`, `CLAUDE.md` (denju's), `CHANGELOG.md`

**Interfaces:**
- Consumes: the whole v0.4.0 surface from Tasks 1-5.
- Produces: no code.

- [ ] **Step 1: README.md**

1. In "Supplying the bytes", after the `Source` block, add:

````markdown
### Resuming an interrupted download

A `Source` that also implements `denju.ResumableSource` declares that it honours
`Request.Offset`:

```go
type ResumableSource interface {
    Source
    ResumesFromOffset() // a declaration; denju never calls it
}
```

For such a source a failed `Fetch` keeps the bytes that arrived, in
`<binary>.<ns>-download-<key>.partial`, where the key is derived from the request's
`ID`, `TargetVersion` and `SHA256`. Calling `Update` again with the same three resumes:
denju re-hashes what is on disk, sets `Request.Offset`, and your `Fetch` writes only the
rest. The digest check still covers every byte.

The partial is deleted when your source returns an error wrapping
`denju.ErrResumeRejected` (it cannot continue from that offset), when the complete file
fails the digest, when a download for a different request starts, and when nothing has
written to it for `Config.PartialRetention` (default 24 hours). denju never restarts a
transfer by itself inside one `Update`; the next call starts from zero.

Leave `Request.Offset` zero - it is denju's. A plain `Source`, `httpsource` included, is
unaffected: it always sees zero and never leaves a partial behind.

Tell failures apart with `errors.Is` on `Result.Cause`: `ErrDownloadFailed` (worth
retrying), `ErrResumeRejected` (retry starts from zero), `ErrChecksumMismatch` (the
artifact is wrong). `Result.Error` carries the same text as before.
````

2. After "How it behaves", add:

````markdown
## Keeping the previous binary

With `Config.RetainPrevious`, a commit keeps the binary the update replaced as
`<binary>.<ns>-previous`, described by `<binary>.<ns>-previous.json`, instead of deleting
it. One generation is kept; the next commit replaces it, and a rollback leaves it alone.

```go
if path, sum, _, ok := u.PreviousBinary(); ok && strings.EqualFold(sum, req.SHA256) {
    res := u.Update(ctx, req, denju.FileSource(path)) // no network at all
}
```

`FileSource` is verified against `Request.SHA256` like any source. Budget the disk: one
extra binary for good, and while an update is in flight the running binary, the rollback
copy and the retained binary side by side, plus the download. Never run the retained
file in place.
````

3. Replace the `**Cooldown**` paragraph with:

```markdown
**`Cooldown`** (off by default) refuses updates for a while. `Config.CooldownScope`
decides which: `CooldownAnyUpdate` (the default, and the only behaviour before v0.4.0)
refuses every update after one completes; `CooldownRolledBackVersion` refuses only the
version most recently rolled back and admits every other target at once, so a corrective
update or a move back to the retained binary is never held. Turn it on if your control
plane can re-issue an update to a program that just rolled it back - without it that is an
unbounded loop that re-downloads the binary every cycle. `InCooldownFor` answers for a
particular target.
```

4. Add rows to the naming table after "outcome record":

```markdown
| partial download | `<binary>.<ns>-download-<key>.partial` (a `ResumableSource` only) |
| retained previous binary | `<binary>.<ns>-previous` (`RetainPrevious` only) |
| its record | `<binary>.<ns>-previous.json` |
```

- [ ] **Step 2: doc.go**

After the `# Integrity` section add:

```go
// # Resuming, retaining and holding back
//
// A [ResumableSource] keeps an interrupted download for the next [Updater.Update]
// of the same request, which resumes from [Request.Offset]. [Config.RetainPrevious]
// keeps the binary a committed update replaced; [Updater.PreviousBinary] reports it
// and [FileSource] reinstalls it without a network. [Config.CooldownScope] set to
// [CooldownRolledBackVersion] holds back only the version most recently rolled back.
// [Result.Cause] carries a failure kind for errors.Is.
//
```

- [ ] **Step 3: CLAUDE.md (denju)**

1. Layout block: add lines `errors.go     failure kinds for Result.Cause, kindError`, `partial.go    partial download naming, listing and discarding`, `filesource.go FileSource, the local resumable source`, `previous.go   RetainPrevious: retention, its record, PreviousBinary`; amend `download.go` to `fresh and resumable staging, hashing, digest check`.
2. Append invariants:

```markdown
**18. A partial is never trusted on its own.** The bytes already on disk are re-hashed
before anything is appended, and the digest check covers the whole file. A partial that
fails it is deleted. Skipping the replay "because the bytes were verified as they
arrived" is wrong: they were never verified, only hashed into a digest that no longer
exists.

**19. denju does not restart a download inside one attempt.** A source that rejects the
offset (`ErrResumeRejected`) deletes the partial and fails the attempt. A silent second
transfer would hide a server that cannot serve ranges, and the retry policy belongs to
the caller.

**20. The retained binary's record never describes another file.** Retention deletes the
old record before replacing the binary and writes the new one after. It is resumable
from any point, retried by `Repair` and `CleanupReported` while the committed journal
exists, and `CleanupReported` keeps the journal until it succeeds - the journal is the
only thing that says the rollback copy belongs to a committed update.

**21. Under `CooldownRolledBackVersion` the hold is carried forward by every write.** The
record is the last attempt, so a later attempt that did not carry the hold would erase it.
That makes every write read the previous record first, so a corrupt record fails every
write under that scope. The default scope does not read first and is unchanged.
```

3. "The naming contract" section: add a sentence that the partial download name (including `partialKey`), the retained binary and its record, and the JSON tags of `previousRecord` and the new `Outcome` fields are part of the contract and pinned by `TestNamingContract`, `TestPartialKey`, `TestPreviousRecordWireCompatibility` and `TestOutcomeWireCompatibility_RollbackAnchor`.

- [ ] **Step 4: CHANGELOG.md**

`CHANGELOG.md` has no `[0.3.0]` entry although `v0.3.0` is tagged. Under `## [Unreleased]` write the v0.4.0 notes, and insert a `[0.3.0]` section above `[0.2.0]` from the tag message:

```markdown
## [Unreleased]

### Added

- `ResumableSource` and `Request.Offset` - a source that declares it honours the offset
  keeps an interrupted download in a partial named after the request, and the next
  `Update` of the same request resumes from it after re-hashing what is on disk.
  `Config.PartialRetention` (default 24 hours) bounds how long a partial is kept.
- `ErrDownloadFailed`, `ErrChecksumMismatch`, `ErrResumeRejected` and `Result.Cause` -
  failure kinds for `errors.Is`. `Result.Error` text is unchanged.
- `FileSource` - a resumable source over a local file.
- `Config.RetainPrevious` and `Updater.PreviousBinary` - a commit keeps the replaced
  binary as `<binary>.<ns>-previous` with a record of its version and SHA-256, so moving
  back to it needs no download. One generation is kept.
- `Config.CooldownScope`, `CooldownRolledBackVersion` and `Updater.InCooldownFor` - a
  cooldown that holds only the version most recently rolled back.
- `Outcome.RolledBackVersion` and `Outcome.RolledBackAt`, maintained under that scope.

### Changed

- Nothing for a program that sets none of the above: the default cooldown scope, the
  staging of a plain `Source`, and the files on disk are exactly as in 0.3.0.

## [0.3.0]

### Added

- `Result.ProgramIntact` - distinguishes an update refused with the program untouched from
  one that got past the point of no return and could not be undone. Callers should
  terminate when it is false.

### Fixed

- A commit that cannot be journalled is no longer reversed, and a rollback that fails to
  restore is no longer reported as a rollback.
```

Update the link references at the bottom: `[Unreleased]: https://github.com/qunulabs/denju/compare/v0.3.0...HEAD` and add `[0.3.0]: https://github.com/qunulabs/denju/compare/v0.2.0...v0.3.0`. Do not rename `[Unreleased]` to `[0.4.0]`; that happens with the tag, which is the user's decision.

- [ ] **Step 5: Full verification (all must pass; report output summaries)**

Run, from `/home/qnu/Desktop/Workspace/QNu/denju`:

```bash
gofmt -s -l .                       # expect no output
go vet ./...
GOOS=windows go vet ./...
go test ./... -count=1
go test ./... -count=1 -race
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  echo "  $target"; GOOS=${target%/*} GOARCH=${target#*/} go build ./... || exit 1
done
go mod tidy -diff                   # read-only; expect exit 0 and no diff. If it reports a diff, STOP and report it - do not run go mod tidy.
git diff --stat -- go.mod go.sum    # expect empty: no dependency change
```

- [ ] **Step 6: Public API is additive only**

```bash
rm -rf /tmp/denju-base && mkdir -p /tmp/denju-base
git -C /home/qnu/Desktop/Workspace/QNu/denju archive e541312 | tar -x -C /tmp/denju-base
(cd /tmp/denju-base && go doc -all .) > /tmp/denju-api-before.txt
(cd /home/qnu/Desktop/Workspace/QNu/denju && go doc -all .) > /tmp/denju-api-after.txt
diff /tmp/denju-api-before.txt /tmp/denju-api-after.txt
(cd /tmp/denju-base && go doc -all ./httpsource) > /tmp/denju-httpsource-before.txt
(cd /home/qnu/Desktop/Workspace/QNu/denju && go doc -all ./httpsource) > /tmp/denju-httpsource-after.txt
diff /tmp/denju-httpsource-before.txt /tmp/denju-httpsource-after.txt   # expect identical
```

Expected: every `<` line in the core diff is a doc-comment line that was reworded (`Config.Cooldown`, `InCooldown`, `Rollback`, `CleanupReported`, `Result`), never a declaration. Any removed or changed signature is a defect: STOP and report.

- [ ] **Step 7: State what was not verified locally**

Report to the controller, verbatim in substance: native Windows behaviour — renaming the rollback copy over the retained binary, the Windows helper swap followed by a retaining commit, deleting and reopening partials, and running a `.partial`-named staged file as the selftest child in `TestEndToEnd_InterruptedDownloadResumes` — is proved only by the `windows-latest` CI jobs (Go 1.24 and stable), which run on the pull request.

- [ ] **Step 8: Stop: controller reviews; do not stage, commit or tag.** Tagging `v0.4.0` (and renaming `[Unreleased]`) is the user's decision after the pull request is merged and CI is green on both platforms.

---

## Self-review against the specs

| Requirement | Task |
|---|---|
| D §3: deterministic partial from id + target version + sha | 2 (`partialKey`, `TestPartialKey`) |
| D §3: keep on transfer error | 2, 6 |
| D §3: delete on checksum mismatch / success / retention | 2 (mismatch, retention), 2 (success: the partial becomes the staged binary, consumed by the swap; e2e asserts it is gone) |
| D §3: denju stats, replays SHA-256, passes offset in `Request` | 2 |
| D §3: no partial-trust path | 2 (`…ACorruptPrefixFailsTheDigestAndIsDeleted`) |
| D §9 item 8c: offset > total → client restarts from 0 | 2 (`ErrResumeRejected` discards; next attempt at 0), 3 (`FileSource`) |
| C §2: `RetainPrevious`, `<binary>.<ns>-previous`, cleanup spares it | 4 |
| C §2: `PreviousBinary() (path, sha256, version string, ok bool)` | 4 |
| C §2: local-file `Source`, zero bytes over the network | 3, 6 |
| C §5: cooldown refuses only the rolled-back `ToVersion`, admits others | 5, 6 |
| C §8 denju tests | 4, 5, 6 |
| C §10.2: one generation | 4 (`…KeepsOneGeneration`) |
| Existing behaviour unchanged with options unset | 1 (message text), 2 (`TestDownload_PlainSourceIgnoresPartials`), 4 (`…WithoutRetainPreviousNothingIsKept`), 5 (`TestDefaultCooldownScope_IsUnchanged`), 7 (API diff) |
| Windows considered | 4 (no `.exe` on retained name; never executed in place; rename-over semantics), 2/6 (close before remove; `.partial` staged file executed natively in CI), 7 Step 7 |

---

## API the SDK plan will consume

All in `github.com/qunulabs/denju` v0.4.0; nothing in `httpsource` changes. The user runs `go get github.com/qunulabs/denju@v0.4.0` in the SDK once tagged (hub §7).

```go
// Configuration (internal/update/adapter.go New)
Config.RetainPrevious   bool          // SDK sets true
Config.Cooldown         time.Duration // SDK enables (value is the SDK plan's decision)
Config.CooldownScope    CooldownScope // SDK sets CooldownRolledBackVersion - REQUIRED for resume retries:
                                      // under the default scope a failed attempt holds the next one
Config.PartialRetention time.Duration // default 24h; SDK may leave unset

// Sources
type ResumableSource interface { Source; ResumesFromOffset() }
func FileSource(path string) ResumableSource
Request.Offset int64 // set by denju; the SDK must leave it zero

// The SDK's FetchArtifact source must:
//   - implement ResumesFromOffset();
//   - send FetchArtifactRequest{update_command_id, offset: req.Offset};
//   - return an error wrapping denju.ErrResumeRejected when the header's
//     checksum_sha256 differs from req.SHA256, or the server answers OUT_OF_RANGE
//     (offset > total_size); offset == total_size is a header with no data and is success;
//   - write data frames to w and return nil at end of stream.
// To resume, call Update again with the IDENTICAL ID, TargetVersion and SHA256.

// Results
Result.Cause error
var ErrDownloadFailed          error // transfer failed; partial kept (resumable source) - retry resumes
var ErrResumeRejected          error // partial discarded - retry starts at 0
var ErrResumedChecksumMismatch error // resumed download failed the digest; partial discarded - retry ONCE from 0
var ErrChecksumMismatch        error // download from byte 0 failed the digest: artifact wrong; permanent
// A local write failure (full disk) deletes the partial and carries NO kind.
// Every failed attempt with an ID is still recorded as StatusFailed in the outcome
// record (PendingOutcome), exactly as in v0.3.0; deciding when to report it is the SDK's.

// Retained binary
func (u *Updater) PreviousBinary() (path, sha256, version string, ok bool)
// ok == false on absence, a corrupt record, or a missing file (the latter two log ERROR).
// Match sha256 case-insensitively against the command's checksum, then Update(ctx, req, FileSource(path)).

// Cooldown query
func (u *Updater) InCooldownFor(targetVersion string, now time.Time) (bool, time.Duration, error)
// Under CooldownRolledBackVersion a refusal is StatusRefusedCooldown with Error
// "version <v> was rolled back less than <d> ago; refusing it for another <r>".
// A corrupt outcome record fails every record write under this scope, including Commit's.
```
