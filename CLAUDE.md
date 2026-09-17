# denju — Claude Context

**Project**: `github.com/qunulabs/denju`. An Apache-2.0 Go library that lets a
long-running program replace its own binary and hand over to the new version, on Unix
and Windows.

Read [README.md](README.md) first for the public API and how a consumer wires it up.
This file is about the invariants — the reasons the code is shaped the way it is, and
the specific things that look like tidying opportunities but are not.

---

## Scope

**denju owns** the write-ahead journal, artifact path derivation, preflight, the atomic
swap, restore, exec-in-place on Unix, the detached helper and service-control-manager
restart on Windows, startup repair with a crash-loop guard, the pre-swap selftest
subprocess protocol, post-swap attestation with a deadline, the durable outcome record,
and the optional cooldown.

**denju owns nothing on a wire.** No HTTP, no gRPC, no TLS, no certificates, no
protobuf, no authentication, in the core package. The caller supplies bytes through a
`Source`, decides what "healthy" means, and reports the outcome itself. `httpsource` is
a separate package precisely so `net/http` stays out of the core.

**denju never calls `os.Exit` on its own account.** Exit codes are returned to the
caller (`RunProcessRole`). The single exception is `osExit(0)` inside the Windows
handover, where "this process stops so the helper can continue" *is* the operation — and
it is a package var so tests observe it instead of dying.

---

## Layout

```
config.go     Config, Namespace-derived names, validation, defaults
updater.go    Updater, New, RunProcessRole, ResolveBinary, record accessors
update.go     Updater.Update — the trigger, and both platform paths
errors.go     failure kinds for Result.Cause, kindError
download.go   fresh and resumable staging, hashing, digest check
partial.go    partial download naming, listing and discarding
filesource.go FileSource, the local resumable source
previous.go   RetainPrevious: retention, its record, PreviousBinary
state.go      the journal: state, paths, writeState/readState, goos + pidAlive seams
swap.go       preflightCheck, swapBinary, restoreOldBinary, copyBinary, sha256File
restart.go    Restart, rollbackAndRestart, relaunchViaHelper, osExit seam
repair.go     Repair and every branch it resolves
attest.go     Attest, PendingAttestation, Commit, Rollback
record.go     Outcome, store, the cooldown anchor rule
selftest.go   the selftest child protocol, tailBuffer
helper.go     the Windows helper role, polling seams, platform seams
log.go        Logger, Level, SlogLogger
exec*.go      the image-swap seam (unix: syscall.Exec; windows: an error)
detach_*.go   detached spawn, service control manager (windows), stubs elsewhere
httpsource/   optional Source over HTTP
examples/     a runnable sketch of a consumer
```

Everything except `httpsource` is one package. That is deliberate: the flow is driven in
tests through package vars (`goos`, `pidAlive`, `execSelf`, `osExit`, `spawnDetached`,
`startService`, `serviceIdentity`, the poll intervals), and splitting it would mean
exporting those seams to nobody's benefit.

---

## Invariants

These are load-bearing. Each one is here because getting it wrong produced a real
outage, or would produce one that is invisible until it matters.

**1. The binary is never absent.** The swap hardlinks `B → B.old` and then does a
*single* atomic rename over `B`. It does not rename `B` aside and move the new one in:
that leaves a window with nothing at `B`, and a host that loses power in that window has
no program and no way to repair itself, because repair runs from the very binary that is
missing.

**2. Every step is journaled before it happens, and fsync'd.** Temp file → `Sync` →
`Close` → `Rename` → best-effort directory sync. Repair's determinism depends on that
ordering: it must never see a phase claiming something that has not happened yet.

**3. The restore precedes the journal write in a rollback.** Journaling `rolledback`
first would, if the restore then failed, leave a journal claiming a rollback that never
happened — the next start would report a rollback while the *new* binary is still
running, and cleanup would delete `B.old`, destroying the only copy of the old version.

**4. Everything that can fail cheaply happens while the program is untouched.** Platform
check, cooldown, preflight, download, digest, and a selftest that actually *executes* the
new binary — all before the drain. The common failures cost a log line and a deleted temp
file. The one exception is deliberate: a broken transfer through a `ResumableSource` keeps
its partial for the retry. A failure to *write* what arrived (a full disk) is not a broken
transfer — it deletes the partial like any other staged file and carries no failure kind,
because keeping it would hold the disk that just ran out.

**5. A drain error aborts; a drain TIMEOUT does not.** This is the surprising half and the
one most likely to be "fixed" by a future reader. A program wedged on stuck work must
still be able to receive the fix for whatever wedged it. Abandoned work is usually
recoverable; an update that can never be applied needs a human on the host.

**6. Repair runs before any lock or port.** A crash-loop rollback restarts the process,
and the successor needs whatever this one holds.

**7. Repair must RECORD what it concludes — including a verdict it merely finds.** Rolling
back is half the job. The record is the cooldown's only anchor and the only input a report
has, and the process that started the update is gone, so nothing else can write it.
Observed live without it: the restored old version came up, found no record, accepted the
same broken binary again, and looped — five rollbacks and six swaps in forty seconds, each
a full download, with the control plane never told a thing.

This extends to a journal that is *already* decided (`repairDecided`). Normally the path
that decided it wrote the record at the same moment, so that call does nothing. It matters
when journal and record have come apart: a record removed or relocated, or — the case every
adopting program hits exactly once — a journal written by a previous version whose update
mechanism was not denju and which kept no record. A decided journal with no record is a
dead end: `PendingOutcome` returns nothing, so the outcome is never reported and the
journal and rollback copy are never cleaned up.

The status must match what the deciding path writes for that phase (`committed` →
`StatusSucceeded`, `rolledback` → `StatusRolledBack`). Diverge and invariant 8's guard
stops matching, which turns "record it once" into "rewrite it on every start".

**8. Repair must not RE-record an outcome it already recorded.** It runs on every start,
and each rewrite would push the cooldown anchor forward, turning one failed update into
an indefinite refusal.

The record holds only the *last* attempt, so "already recorded" cannot mean only "the
last record matches". A decided journal stays until its outcome is reported, and a later
attempt (the refusal of a re-issued command) can replace the decided outcome as the
record meanwhile. `repairDecided` therefore also leaves alone any record written after the
journal took its verdict — the journal's modification time, since nothing rewrites a
decided journal. Without that, a report that failed once turned the next refusal into a
fresh full-length hold and a second report of the old outcome. The same rule keeps a dead
Windows handoff (journaled `rolledback`, recorded `failed`) from being re-recorded as
`rolled_back`, which under `CooldownRolledBackVersion` would hold a version that never ran.

**9. A cooldown refusal is recorded but must not move the anchor.** Reset it to the
refusal's own timestamp and the cooldown extends forever; drop it and the cooldown is
bypassable by simply retrying. `store.record` owns this rule, which is why nothing writes
an attempt through `store.save` directly.

**10. Only the new image may commit.** `pendingAttestation` refuses when
`selfIsNewVersion` is false. An old image reaching there has already been handled by
repair, and letting it "commit" would confirm an update that never took effect.

**11. Context cancellation during attestation is not a verdict.** A program shutting down
mid-window has not failed. Rolling it back on the way out would replace a working binary
during an operation nobody is watching. The journal survives; the next start resumes.

**12. The process environment is never persisted.** It routinely carries credentials, and
a journal outlives its writer. It reaches the successor by inheritance only.

**13. The staged download lands in the target binary's directory.** `os.Rename` is atomic
only within one filesystem. Staging in the system temp directory silently turns the swap
into a cross-device copy, and defers "no space" from before the swap to halfway through
it.

**14. Image identity falls back to SHA-256.** Two builds may carry the same version
string. When `TargetVersion == OldVersion`, `selfIsNewVersion` compares the running
binary's digest against `new_sha256` instead.

**15. A verdict is serialised end to end.** `decide` holds `u.mu` across the *whole*
resolution, not just the timer disarm. Disarming narrows the race between a caller's
`Commit` and the deadline goroutine's `Rollback` but cannot close it — the timer may
already be past its select and inside `Rollback`. Unserialised, both clear the
`pendingAttestation` gate and act on opposite verdicts: a journal reading `committed` over
a binary that was restored, or one command recorded as both `succeeded` and `rolled_back`.
The contended state is the *filesystem*, so `-race` is blind to it and only
`TestAttest_CommitRacingTheDeadlineDecidesOnlyOnce` catches it — reproducibly, at roughly
one collision in ten.

**16. An abandoned drain is concurrent with everything after it.** Invariant 5 means a
callback that overruns is left running, so from that moment it races the swap, the
`BeforeHandoff` callback and the handover. That is the caller's problem to handle and is
documented on `Config.Drain`, but it is also *denju's* problem in tests: it is why the
update rig's step recorder and `captureLog` are both locked, and why `Config.Log` is
specified as safe for concurrent use.

**17. The Windows handshake precedes the drain.** The helper is spawned and has proved it
started *before* the program stops serving, so a helper that cannot start aborts the
update at no cost. This is the opposite order from Unix, on purpose.

**18. A partial is never trusted on its own.** The bytes already on disk are re-hashed
before anything is appended, and the digest check covers the whole file. A partial that
fails it is deleted. Skipping the replay "because the bytes were verified as they
arrived" is wrong: they were never verified, only hashed into a digest that no longer
exists.

A mismatch after resuming from a non-zero offset is `ErrResumedChecksumMismatch`, not
`ErrChecksumMismatch`, and `errors.Is` must not match the two. The kept prefix can be
damaged on the device by a power cut, or a source can ignore the offset, so a resumed
mismatch does not prove the artifact wrong; the caller retries once from zero, and a
second mismatch — now `ErrChecksumMismatch` — is permanent. A delete of a partial that
fails is logged at ERROR and the error must not claim the partial was discarded: its name
is fixed per request, so the next attempt finds it again.

**19. denju does not restart a download inside one attempt.** When a source rejects the
offset (`ErrResumeRejected`), denju — not the source — deletes the partial and fails the
attempt. A silent second transfer would hide a server that cannot serve ranges, and the
retry policy belongs to the caller.

**20. The retained binary's record never describes another file.** Retention deletes the
old record before replacing the binary and writes the new one after. It is resumable
from any point, and runs at the commit, in `CleanupReported`, and in `Repair` on every start
while the committed journal exists. `CleanupReported` keeps the journal while retention
fails — the journal is the only thing that says the rollback copy belongs to a committed
update — but nothing may assume it is called again: the documented wiring calls it only for
a pending outcome, and the outcome was reported on the start whose retention failed. So
`Repair` itself removes the committed journal and the leftovers beside it (the Windows
helper copy and its log) once retention succeeds and the outcome is on record, which also
lets the stale-partial sweep run again. Without `RetainPrevious` none of this happens, and
the files are exactly v0.3.0's.

The rollback copy is released only after the commit is *recorded*. A refused commit (a
record that cannot be written, or a corrupt one under `CooldownRolledBackVersion`) leaves the
update undecided, and the crash counter's eventual rollback needs `.old` to restore from.

**21. Under `CooldownRolledBackVersion` the hold is carried forward by every write.** The
record is the last attempt, so a later attempt that did not carry the hold would erase it.
That makes every write read the previous record first, so a corrupt record fails every
write under that scope. The default scope reads the previous record only for a refusal,
exactly as before v0.4.0.

Two images take part in a hold, and both need v0.4.0 with the scope. The image rolled back
**from** writes it: the attestation verdict and the crash loop both record the rollback
before restoring. The image rolled back **to** enforces it, in its own `Update`, and carries
it forward on every later write. Its `Repair` records the rollback only when nothing has
recorded it — the new version never started (a failed exec on Unix, a failed relaunch by the
Windows helper), or the other image's record write failed — and it never adds a hold to a
rollback that is already recorded. So a v0.3.0 image that fails and rolls
back to a v0.4.0 image leaves that image with no hold: the v0.4.0 image will admit the same
move again at once. Only the control plane stops that loop.

A dead Windows handoff is recorded `failed` and holds nothing under this scope, on purpose:
the target never ran.

---

## The naming contract

Every file and environment variable derives from `Config.Namespace`, in `newNames` and
`names.pathsFor`. `TestNamingContract` pins all of it.

**This is an on-disk contract between consecutive versions of a consumer's program.** The
version being replaced writes the journal that the version replacing it has to read.
Changing a derived shape orphans every update in flight across the changeover: the
successor finds no journal, never commits, never reports, leaves `B.old` behind for good,
and on Windows strands a helper with nothing to hand over to.

Treat any change to `TestNamingContract`, to the JSON tags on `state`, or to the JSON tags
and status strings on `Outcome`, as a breaking change to every deployed consumer — not a
breaking change to denju's API, which is a much smaller thing.

`B.old` carries no namespace, deliberately: it is the file an operator reaches for at
three in the morning, and `<binary>.old` is the name they will guess.

The partial download name (including `partialKey`), the retained binary and its record,
and the JSON tags of `previousRecord` and the new `Outcome` fields are part of the same
contract, pinned by `TestNamingContract`, `TestPartialKey`,
`TestPreviousRecordWireCompatibility` and `TestOutcomeWireCompatibility_RollbackAnchor`.

---

## Testing

`go test ./...`. No test dependencies — stdlib `testing` only, with small assertion
helpers in `helpers_test.go`.

**`-race` is not optional, and CI is the only place it runs.** It needs cgo, which the
Windows development machine has no C toolchain for, so `go test -race` fails locally before
it starts. A red race job is therefore a real defect that was never going to be caught
before the push — never a flake, and never something to re-run until it passes. It has
already caught one: the update rig shared a bare slice with an abandoned drain goroutine,
and it failed on Go 1.26 while passing on 1.24, which is scheduling luck rather than
evidence. `TestNoTestDependencies` keeps it that way: a library a
program depends on for its ability to start at all should not pull an assertion framework
into everybody's build.

The Windows flow is the hardest to reach and the most important to get right, so the
platform seams let it be driven from a Linux host and vice versa — but CI runs both
natively, because only the native run exercises the real service control manager,
`os.Link`, and Windows' refusal to replace a running `.exe`.

`e2e_test.go` compiles real throwaway binaries and updates one into another: a genuine
selftest child, a genuine swap, a genuine repair-and-commit by a second `Updater` standing
in for the successor. Only `execSelf` is stubbed, because a process that exec'd itself
cannot go on to make assertions.

---

## Coding preferences

1. Ask clarification questions during planning. Involve the user in decisions, large and
   small.
2. Validate first; fail fast and loud. Do not paper over bad input.
3. No unnecessary fallbacks or defaults — they create bloat and silent bugs. A default
   that changes behaviour for an existing consumer is worse than no default.
4. Prefer stdlib. A new dependency in the core package needs explicit approval; the point
   of the one-dependency module graph is that consumers inherit almost nothing.
5. Comment the *why*, especially where the code looks wrong and is not. Several
   invariants above exist as comments at their call sites for exactly this reason.
6. Assign every ignored error to `_` explicitly.
7. Long-running goroutines take a `context.Context` and return when it is cancelled.
8. Do not run commands that modify the project or its dependencies (`go get`,
   `go mod tidy`) without asking first.
