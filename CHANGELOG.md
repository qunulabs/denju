# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the
major version is 0, the public API may change between minor releases.

## [Unreleased]

### Added

- `ResumableSource` and `Request.Offset` - a source that declares it honours the offset
  keeps an interrupted download in a partial named after the request, and the next
  `Update` of the same request resumes from it after re-hashing what is on disk.
  `Config.PartialRetention` (default 24 hours) bounds how long a partial is kept.

  Only a broken transfer keeps the partial. A failure to write it (a full disk) deletes
  it, as v0.3.0 deleted its temp file, and carries no failure kind. A delete that fails is
  logged at ERROR, and the error says the partial could not be deleted.
- `ErrDownloadFailed`, `ErrResumeRejected`, `ErrResumedChecksumMismatch`,
  `ErrChecksumMismatch` and `Result.Cause` - failure kinds for `errors.Is`. `Result.Error`
  text is unchanged.

  A digest mismatch on a download that resumed from a non-zero offset is
  `ErrResumedChecksumMismatch`, and `errors.Is(err, ErrChecksumMismatch)` is false for it:
  the kept bytes, not the artifact, may be what is wrong. The rule for a caller: **retry
  once from zero; a second mismatch is permanent** (it is then `ErrChecksumMismatch`).
- `FileSource` - a resumable source over a local file. It refuses anything but a regular
  file.
- `Config.RetainPrevious` and `Updater.PreviousBinary` - a commit keeps the replaced
  binary as `<binary>.<ns>-previous` with a record of its version and SHA-256, so moving
  back to it needs no download. One generation is kept.

  It is created by the image that commits — the update's target — and needs denju
  v0.4.0 or later there. A commit by an image on an older denju deletes the replaced
  binary instead of keeping it. A retention that fails at the commit is retried by
  `CleanupReported` and by `Repair` on every start, and `Repair` removes the leftover
  journal once it succeeds.
- `Config.CooldownScope`, `CooldownRolledBackVersion` and `Updater.InCooldownFor` - a
  cooldown that holds only the version most recently rolled back.

  The hold needs denju v0.4.0 or later with `CooldownRolledBackVersion` on **both**
  images. The image rolled back **from** (the update's target) writes it: an attestation
  failure and a crash loop record the rollback, with the hold, before restoring. The
  image rolled back **to** enforces it when the next command arrives and carries it
  forward; an image on an older denju enforces nothing and drops the hold on its next
  record write (`MarkReported` included, through the 0.3.0 `Outcome` shape). The restored
  image never adds a hold to a rollback already recorded, so a v0.4.0 program moved back
  to a build on denju v0.3.0 that then fails is **not** held, and would accept the same
  move again at once: only the control plane stops that loop.

  A Windows update whose helper died before the swap is recorded as failed and holds
  nothing under this scope: the target never ran. A helper that is alive but fails the
  swap itself is still recorded as a rollback, so under this scope that version is held
  for one cooldown window although it never ran.
- `Outcome.RolledBackVersion` and `Outcome.RolledBackAt`, maintained under that scope.

### Changed

- `New` rejects `Config.CooldownScope = CooldownRolledBackVersion` paired with a zero or
  negative `Config.Cooldown`, and rejects a `CooldownScope` value it does not know.
- `Update` refuses a `Request` that arrives with `Offset` already set; only denju sets it.
- `Result` gains `Cause`, non-nil on every failure. A consumer comparing two `Result`
  values with `==` now sees a difference it did not before.

### Fixed

- `Updater.Repair` no longer re-records a decided update's outcome over a later attempt
  recorded while the journal was still on disk. Re-recording restarted the cooldown at a
  full window, reported the old outcome a second time, and overwrote the later attempt;
  it also turned a dead Windows handoff, recorded as failed, into a rollback.

## [0.3.0]

### Added

- `Result.ProgramIntact` - distinguishes an update refused with the program untouched from
  one that got past the point of no return and could not be undone. Callers should
  terminate when it is false.

### Fixed

- A commit that cannot be journalled is no longer reversed, and a rollback that fails to
  restore is no longer reported as a rollback.

## [0.2.0]

### Added

- `Updater.PendingAttestation` and the `Pending` type — report whether an update is
  waiting for a verdict from this process, and what it was.

  `Updater.Attest` remains the usual way to resolve one and needs no such check. This is
  for a caller that judges health its own way — a registration accepted, a probe that has
  to pass repeatedly, a real request served end to end — and therefore has to know whether
  to begin that work at all. Without it the only options were to run the health policy on
  every ordinary start, which is wasteful and surprising to whoever wrote the check, or to
  give up a bespoke policy for the built-in deadline.

  It is a query and changes nothing.

## [0.1.2]

### Fixed

- `Updater.Repair` now records an outcome for a journal that already carries a verdict but
  has no matching record, instead of leaving it unreported. Such a journal was a dead end:
  `PendingOutcome` returned nothing, so the outcome was never delivered and neither the
  journal nor the rollback copy was ever cleaned up.

  It arises whenever the record and the journal come apart — a record removed or a
  `Config.RecordPath` that moved — and, once, for every program adopting denju in place of
  an update mechanism of its own: the journal describing the update that installs the
  adopting build was written by a version that kept no record, so that update went
  unreported on every host.

  An outcome that was already recorded is left untouched, so the ordinary path, where the
  deciding code wrote the record itself, is unaffected and the cooldown anchor does not
  move.

## [0.1.1]

### Fixed

- `Commit` and the attestation deadline can no longer resolve one update two ways. The
  verdict is now serialised end to end rather than only at the point the deadline is
  disarmed, which left a window in which both took effect — producing a journal reading
  `committed` over a binary that had been restored, or a single command recorded as both
  `succeeded` and `rolled_back`.
- A data race in the test suite, where the update rig shared an unsynchronised slice with
  a drain callback that had overrun its timeout and been abandoned.

### Changed

- `Config.Log` is documented as requiring a `Logger` that is safe for concurrent use, and
  `Config.Drain` as requiring a callback that stays safe after it overruns `DrainTimeout`
  and is abandoned. Neither is a behaviour change; both were already true.

## [0.1.0]

First release.

### Added

- `Updater.Update` — download, verify, selftest, drain, swap and hand over to a new
  binary. Exec-in-place on Unix; detached helper with service-control-manager restart on
  Windows.
- `Updater.Repair` — startup reconciliation of an interrupted update, with a crash-loop
  guard and stale-journal collection.
- `Updater.Attest`, `Commit` and `Rollback` — a deadline for the new version to prove
  itself, and the caller's verdict either way.
- `Updater.Restart` — caller-requested restart of the current binary, driven through a
  journal separate from the update journal.
- `Updater.RunProcessRole` — dispatch for the two roles a process can be started in, the
  Windows update helper and the selftest child. Returns an exit code rather than calling
  `os.Exit`.
- `Updater.PendingOutcome`, `MarkReported` and `CleanupReported` — a durable record of
  an update's outcome, so a successor process can report what its predecessor did.
- `Config.Cooldown` — optional refusal window after a completed update, for control
  planes that can command a downgrade or re-issue to a program that just rolled back.
  Disabled by default.
- `Source` — the caller's one-method view of the outside world.
- `httpsource` — an optional `Source` over plain HTTP, with an opt-in size limit. Keeps
  `net/http` out of the core.
- `Logger` — a plain function type, with a `SlogLogger` adapter. Defaults to silence
  rather than to any global logger.

[Unreleased]: https://github.com/qunulabs/denju/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/qunulabs/denju/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/qunulabs/denju/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/qunulabs/denju/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/qunulabs/denju/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/qunulabs/denju/releases/tag/v0.1.0
