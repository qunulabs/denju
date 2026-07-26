# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While the
major version is 0, the public API may change between minor releases.

## [Unreleased]

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

[Unreleased]: https://github.com/qunulabs/denju/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/qunulabs/denju/releases/tag/v0.1.0
