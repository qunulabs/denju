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
download.go   staging, hashing, digest check
state.go      the journal: state, paths, writeState/readState, goos + pidAlive seams
swap.go       preflightCheck, swapBinary, restoreOldBinary, copyBinary, sha256File
restart.go    Restart, rollbackAndRestart, relaunchViaHelper, osExit seam
repair.go     Repair and every branch it resolves
attest.go     Attest, Commit, Rollback
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
file.

**5. A drain error aborts; a drain TIMEOUT does not.** This is the surprising half and the
one most likely to be "fixed" by a future reader. A program wedged on stuck work must
still be able to receive the fix for whatever wedged it. Abandoned work is usually
recoverable; an update that can never be applied needs a human on the host.

**6. Repair runs before any lock or port.** A crash-loop rollback restarts the process,
and the successor needs whatever this one holds.

**7. Repair must RECORD what it concludes.** Rolling back is half the job. The record is
the cooldown's only anchor and the only input a report has, and the process that started
the update is gone, so nothing else can write it. Observed live without it: the restored
old version came up, found no record, accepted the same broken binary again, and looped —
five rollbacks and six swaps in forty seconds, each a full download, with the control
plane never told a thing.

**8. Repair must not RE-record an outcome it already recorded.** It runs on every start,
and each rewrite would push the cooldown anchor forward, turning one failed update into
an indefinite refusal.

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

**15. The Windows handshake precedes the drain.** The helper is spawned and has proved it
started *before* the program stops serving, so a helper that cannot start aborts the
update at no cost. This is the opposite order from Unix, on purpose.

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

---

## Provenance

denju was extracted from two closed-source programs. `TestNoProvenanceLeaks` scans every
`.go`, `.md` and workflow file for terms that would name them, and fails on the machine
where the mistake is made rather than in a pull request two minutes later.

If you port more code in from either origin, **rewrite the comments** rather than copying
them. Several described a specific product's architecture in prose that reads as generic
until you notice it is describing a load balancer nobody here has. Every occurrence of
"agent" becomes "program"; "the backend" becomes "the caller".

The naming-contract test uses *invented* namespaces, not the real ones. It loses nothing:
the shapes are frozen for every namespace equally.

---

## Testing

`go test ./...`. No test dependencies — stdlib `testing` only, with small assertion
helpers in `helpers_test.go`. `TestNoTestDependencies` keeps it that way: a library a
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
