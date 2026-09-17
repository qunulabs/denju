# denju

Self-update and handover for long-running Go programs, on Unix and Windows.

denju owns the mechanics that are easy to get subtly wrong — a write-ahead journal so
an interrupted update can always be resolved, an atomic swap during which the binary is
never absent, exec-in-place on Unix so no supervisor ever sees a restart, a detached
helper plus service-control-manager restart on Windows, automatic rollback when the new
version does not prove itself, and startup repair with a crash-loop guard.

It owns none of the policy. denju never learns how an update was announced or how its
outcome is reported: no HTTP, no gRPC, no TLS, no certificates, no protobuf, no
authentication. You supply the bytes; you decide what "healthy" means; you report the
outcome wherever you like. denju also never terminates your process on its own account.

```go
import "github.com/qunulabs/denju"
```

One dependency (`golang.org/x/sys`, Windows only). Go 1.24+. Apache-2.0.

## The two gates

An update passes two independent checks, and the difference between them is most of
what there is to understand.

**The selftest runs before anything is swapped.** denju executes the freshly downloaded
binary as a child process with the selftest environment variable set; that child runs
your `Selftest` function and must exit zero. The running program is completely
untouched, so a binary that is the wrong architecture, truncated, or unrunnable costs
nothing but a log line and a deleted temp file.

**Attestation runs after the swap, in the new image.** The new version is running for
real. Your program decides it works and calls `Commit`, or decides it does not and calls
`Rollback`. If neither happens before the deadline, denju restores the previous binary
and restarts into it.

## Wiring

Three calls have to be in the right place. Everything else is event-driven.

```go
u, err := denju.New(denju.Config{
    Namespace: "myapp",       // see "The namespace is permanent" below
    Version:   buildVersion,
    Selftest:  func() error { return config.Load() },
    Drain:     func(ctx context.Context) error { return server.Shutdown(ctx) },
    Log:       denju.SlogLogger(slog.Default()),
    Cooldown:  30 * time.Minute,
})
if err != nil {
    return err
}

// 1. First thing in main. This process may be an update helper or a selftest
//    child rather than a normal start.
if code, isRole := u.RunProcessRole(); isRole {
    os.Exit(code)
}

// 2. Before acquiring any lock or binding any port: a rollback restart hands
//    off to a successor that needs them.
if err := u.Repair(); err != nil {
    return err
}

// ... normal startup ...

// 3. Once serving, arm the deadline for an update that is mid-flight. A no-op
//    when there isn't one, so call it unconditionally.
u.Attest(ctx, 5*time.Minute)
```

Report anything the previous process could not, once you can talk to whatever you report
to:

```go
if out, err := u.PendingOutcome(); err == nil && out != nil {
    if err := controlPlane.Report(out); err == nil {
        _ = u.MarkReported()
        u.CleanupReported()
    }
}
```

Then trigger an update whenever you learn of one:

```go
res := u.Update(ctx, denju.Request{
    ID:            "cmd-42",
    TargetVersion: "1.4.0",
    SHA256:        digest,
    TargetOS:      "linux",
    TargetArch:    "amd64",
}, httpsource.New(preSignedURL))
```

`Update` **does not return when it succeeds** — on Unix the process image has been
replaced, and on Windows the process has exited so a helper can take over. A returned
`Result` always describes an update that did not happen.

Not all of those are survivable, and `Status` cannot tell them apart — a refused
download and a failed exec whose rollback *also* failed are both `StatusFailed`.
`ProgramIntact` is what separates them:

```go
res := u.Update(ctx, req, src)
if !res.ProgramIntact {
    // The drain has run, BeforeHandoff has fired, and the binary on disk is no
    // longer this process. Carrying on means running a program that has already
    // shut down.
    log.Error("the update could neither be completed nor undone", "err", res.Error)
    os.Exit(1)
}
log.Warn("update refused", "err", res.Error)
```

It takes an exec failure *and* a failed restore of the previous binary in the same
attempt, so it is rare — and it is the one outcome that must not be logged and
shrugged off. The journal is left on disk so the next start can reconcile it.

Once healthy, accept it:

```go
if err := u.Commit(); err != nil {
    log.Error("could not commit the update", "err", err)
}
```

## Supplying the bytes

`Source` is one method. Implement it over whatever you already have.

```go
type Source interface {
    Fetch(ctx context.Context, req Request, w io.Writer) error
}
```

denju owns everything around it: creating the temp file in the target binary's directory
so the rename that follows is atomic, hashing what is written against `Request.SHA256`,
making it executable, and deleting it on any failure (a `ResumableSource`, below, keeps it
after a broken transfer). Return a write error from `w` rather than swallowing it.

For a plain HTTPS GET, `github.com/qunulabs/denju/httpsource` is about forty lines and
keeps `net/http` out of the core:

```go
src := httpsource.New(url, httpsource.WithMaxBytes(512<<20))
```

### Resuming an interrupted download

A `Source` that also implements `denju.ResumableSource` declares that it honours
`Request.Offset`:

```go
type ResumableSource interface {
    Source
    ResumesFromOffset() // a declaration; denju never calls it
}
```

For such a source a broken transfer keeps the bytes that arrived, in
`<binary>.<ns>-download-<key>.partial`, where the key is derived from the request's
`ID`, `TargetVersion` and `SHA256`. Calling `Update` again with the same three resumes:
denju re-hashes what is on disk, sets `Request.Offset`, and your `Fetch` writes only the
rest. The digest check still covers every byte.

A broken transfer is the only failure that keeps the partial. It is deleted when writing
to it fails (a full disk: keeping it would hold the space that just ran out), when your
source returns an error wrapping `denju.ErrResumeRejected` (it cannot continue from that
offset), when the complete file fails the digest, when flushing or finalizing it after a
successful `Fetch` fails, and when a download for a different request starts. Each of
those ends the attempt: denju never restarts a transfer by itself inside one `Update`, so
it is the *next* call that starts from zero. A partial nothing has written to for
`Config.PartialRetention` (default 24 hours) is deleted at the start of the call that finds
it, which then proceeds from zero itself. If a delete fails — on Windows, another program
holding the file — denju logs it at ERROR and the error says the partial could not be
deleted; the next attempt finds it again.

Leave `Request.Offset` zero — it is denju's. A plain `Source`, `httpsource` included, is
unaffected: it always sees zero and never leaves a partial behind. A download through a
plain `Source` deletes every partial first, including one an earlier resumable attempt at
the same request left.

Use **one `Updater` per binary**, across processes as well as within one. Two Updaters
downloading beside the same binary delete each other's partials as belonging to a
different request, and nothing enforces the rule.

Tell failures apart with `errors.Is` on `Result.Cause`. `Result.Error` carries the same
text as before.

| Kind | Meaning | Retry |
|---|---|---|
| `ErrDownloadFailed` | the transfer failed or timed out | worth retrying; a `ResumableSource` resumes |
| `ErrResumeRejected` | the source cannot continue from the offset | the next attempt starts from zero |
| `ErrResumedChecksumMismatch` | a download that resumed failed the digest: the kept bytes may be damaged, or the source ignored the offset | **retry once from zero**; a second mismatch is `ErrChecksumMismatch` |
| `ErrChecksumMismatch` | a download from byte zero failed the digest: the artifact is wrong | permanent |

The two mismatch kinds are distinct: `errors.Is(err, ErrChecksumMismatch)` is false for a
resumed mismatch. Every other failure, including a local one such as a full disk, carries
no kind.

## What denju verifies

denju checks that the SHA-256 you pass in `Request.SHA256` matches the bytes it writes,
and that `TargetOS`/`TargetArch` match the running process. **That is all it verifies.**
It does not check signatures, does not validate code signing, and places no trust in any
transport.

Establishing that a digest is authentic is your job, and it is the part that matters.
Carry the digest over a channel you already trust — a signed manifest, an authenticated
RPC — and denju will make sure the bytes on disk are the bytes you were promised. See
[SECURITY.md](SECURITY.md).

## The namespace is permanent

`Config.Namespace` determines the name of every file denju writes beside your binary and
every environment variable it sets:

| | |
|---|---|
| journal | `<binary>.<ns>-update.state.json` |
| restart journal | `<binary>.<ns>-restart.state.json` |
| rollback copy | `<binary>.old` |
| helper copy (Windows) | `<binary>.<ns>-updater.exe` |
| discarded binary (Windows) | `<binary>.<ns>-update.discard` |
| helper log (Windows) | `<binary>.<ns>-update.log` |
| outcome record | `<binary>.<ns>-record.json`, or `Config.RecordPath` |
| partial download | `<binary>.<ns>-download-<key>.partial` (a `ResumableSource` only) |
| retained previous binary | `<binary>.<ns>-previous` (`RetainPrevious` only) |
| its record | `<binary>.<ns>-previous.json` |
| environment | `<NS>_UPDATE_MODE`, `<NS>_UPDATE_STATE`, `<NS>_SELFTEST` |

These names are an on-disk contract between consecutive versions of your program: the
version being replaced writes the journal that the version replacing it has to read.
Changing the namespace of a deployed program orphans any update in flight during the
changeover — the successor finds no journal, never commits, never reports, and leaves
the rollback copy behind for good.

Choose it once and leave it alone.

## How it behaves

**On Unix**, the swap hardlinks the old binary to `<binary>.old`, then replaces the
binary with a single atomic rename — the binary is never absent, so a power cut cannot
strand a host with nothing to run. Then `syscall.Exec` replaces the process image inside
the same PID. systemd's `Restart=`, launchd's `KeepAlive` and container restart policies
are never consulted, because from the supervisor's point of view nothing happened.

**On Windows**, which can neither overwrite a running `.exe` nor exec, denju copies
itself aside and spawns that copy fully detached. The copy waits for the original to
exit, performs the same swap, and starts the program again — through the service control
manager when it runs as a service, so the successor *is* the service. The original does
not exit until the helper has proved it started.

**When something goes wrong**, startup repair reads the journal and reconciles the disk
deterministically: an interrupted swap is finished or undone, a new version that keeps
crashing before it can attest is rolled back after `CrashTolerance` starts, and an
orphaned journal from a crash long past is collected.

**`Cooldown`** (off by default) refuses updates for a while. `Config.CooldownScope`
decides which: `CooldownAnyUpdate` (the default, and the only behaviour before v0.4.0)
refuses every update after one completes; `CooldownRolledBackVersion` refuses only the
version most recently rolled back and admits every other target at once, so a corrective
update or a move back to the retained binary is not held unless that version is itself
the one just rolled back. Turn it on if your control plane can re-issue an update to a
program that just rolled it back — without it that is an unbounded loop that re-downloads
the binary every cycle. `InCooldownFor` answers for a particular target.

## Keeping the previous binary

With `Config.RetainPrevious`, a commit keeps the binary the update replaced as
`<binary>.<ns>-previous`, described by `<binary>.<ns>-previous.json`, instead of deleting
it. One generation is kept; the next commit replaces it, and a rollback leaves it alone.

```go
if path, sum, _, ok := u.PreviousBinary(); ok && strings.EqualFold(sum, req.SHA256) {
    u.Update(ctx, req, denju.FileSource(path)) // no network at all
}
```

`FileSource` is verified against `Request.SHA256` like any source. Budget the disk: one
extra binary for good. The rollback copy costs nothing extra on Unix — it is a hardlink
to the running binary — so the peak while an update is in flight is the running binary,
the download in progress and the retained one, plus a full copy of the binary for the
detached helper on Windows. Never run the retained file in place.

If keeping it fails at the commit, the commit still stands: the replaced binary stays as
`<binary>.old`, and `CleanupReported` and every later `Repair` try again. Once it succeeds,
`Repair` removes the leftover journal itself.

The scoped cooldown and the retained binary each depend on particular images having
v0.4.0:

- **The scoped hold needs both images** — the one rolled back from and the one rolled
  back to — on denju v0.4.0 or later with `CooldownRolledBackVersion`.
  - The image rolled back **from** (the update's target) writes the hold. An attestation
    failure and a crash loop both record the rollback, with the hold, before restoring
    the previous binary.
  - The image rolled back **to** reads and enforces the hold when the next command
    arrives, and carries it forward on every later write. An image on an older denju
    enforces nothing, and drops the hold on its next record write (`MarkReported`
    included, through the 0.3.0 `Outcome` shape, which has no field to carry it).
  - The image rolled back to records the rollback itself only when nothing has recorded
    it (the new version never started, or the record write failed), and it never adds a
    hold to a rollback that is already recorded.
  - So when a program on v0.4.0 is moved back to a build on denju v0.3.0, and that build
    fails and is rolled back, the v0.4.0 program is **not** held: the older build recorded
    the rollback without a hold. It will accept the same move again at once. Denju does
    not stop that loop; the control plane has to, for example by pausing the rollout on
    the first failure.
- **The retained binary** is created by the image that **commits** — the update's target.
  It needs denju v0.4.0 or later with `RetainPrevious`; a commit by an image on an older
  denju deletes the replaced binary instead of keeping it.

## Running the tests

```sh
make test        # go test ./... -count=1
make test-race   # with the race detector
make lint        # go vet
```

The suite includes end-to-end tests that compile real throwaway binaries and update one
into another, so the selftest child protocol and the swap are exercised for real rather
than mocked.

## License

Apache-2.0. See [LICENSE](LICENSE).
