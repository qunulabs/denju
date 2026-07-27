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
making it executable, and deleting it on any failure.

For a plain HTTPS GET, `github.com/qunulabs/denju/httpsource` is about forty lines and
keeps `net/http` out of the core:

```go
src := httpsource.New(url, httpsource.WithMaxBytes(512<<20))
```

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

**`Cooldown`** (off by default) refuses a new update for a while after one completes.
Turn it on if your control plane can command a downgrade, or can re-issue an update to a
program that just rolled one back — without it, those are unbounded loops that
re-download the binary every cycle.

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
