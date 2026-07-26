// Package denju lets a long-running Go program replace its own binary and hand
// over to the new version, safely, on Unix and Windows.
//
// It owns the mechanics that are easy to get subtly wrong: a write-ahead journal
// so an interrupted update can always be resolved, an atomic swap during which
// the binary is never absent, exec-in-place on Unix so no supervisor ever sees a
// restart, a detached helper plus service-control-manager restart on Windows,
// automatic rollback when the new version does not prove itself, and startup
// repair with a crash-loop guard.
//
// It owns none of the policy. denju never learns how an update was announced or
// how its outcome is reported: no HTTP, no gRPC, no TLS, no certificates, no
// protobuf, no authentication. The caller supplies the bytes through a [Source],
// decides what "healthy" means, and reports the outcome wherever it likes. denju
// also never terminates the process on its own account - exit codes are returned
// to the caller.
//
// # The two gates
//
// An update passes two independent checks, and understanding the difference
// between them is most of understanding this package.
//
// The first is the selftest, and it runs BEFORE anything is swapped. denju
// executes the freshly downloaded binary as a child process with the selftest
// environment variable set; that child runs [Config.Selftest] and must exit
// zero. The running program is completely untouched, so a binary that is the
// wrong architecture, truncated, or unrunnable costs nothing but a log line and
// a deleted temp file. This is the cheapest guard against the worst outcome: an
// unstartable binary installed over a working one on a machine nobody can reach.
//
// The second is attestation, and it runs AFTER the swap, in the new image. The
// new version is running for real; the caller decides it works and calls
// [Updater.Commit], or decides it does not and calls [Updater.Rollback]. If
// neither happens before the deadline armed by [Updater.Attest], denju restores
// the previous binary and restarts into it. What counts as proof is the caller's
// to define - registering with a control plane, passing a health probe, serving
// a request - because only the caller knows.
//
// # Wiring
//
// Three calls have to be in the right place in main. Everything else is
// event-driven.
//
//	u, err := denju.New(denju.Config{
//		Namespace: "myapp",
//		Version:   buildVersion,
//		Selftest:  func() error { return config.Load() },
//		Log:       denju.SlogLogger(slog.Default()),
//	})
//	if err != nil {
//		return err
//	}
//
//	// 1. First thing in main. This process may be an update helper or a
//	//    selftest child rather than a normal start.
//	if code, isRole := u.RunProcessRole(); isRole {
//		os.Exit(code)
//	}
//
//	// 2. Before acquiring any lock or binding any port: a rollback restart
//	//    hands off to a successor that needs them.
//	if err := u.Repair(); err != nil {
//		return err
//	}
//
//	// 3. Once serving, arm the deadline for an update that is mid-flight.
//	u.Attest(ctx, 5*time.Minute)
//
// Then trigger an update whenever the caller learns of one:
//
//	res := u.Update(ctx, denju.Request{
//		ID:            "cmd-42",
//		TargetVersion: "1.4.0",
//		SHA256:        digest,
//		TargetOS:      "linux",
//		TargetArch:    "amd64",
//	}, src)
//
// [Updater.Update] does not return when it succeeds: on Unix the process image
// has been replaced, and on Windows the process has exited so a helper can take
// over. A returned [Result] therefore always describes an update that did not
// happen.
//
// # Integrity
//
// denju verifies the SHA-256 that the caller passes in [Request.SHA256] against
// the bytes it writes to disk, and verifies that [Request.TargetOS] and
// [Request.TargetArch] match the running process. That is all it verifies. It
// does not check signatures, does not validate code signing, and places no trust
// in any transport. Establishing that a digest is authentic is the caller's job.
// See SECURITY.md.
//
// # Compatibility
//
// [Config.Namespace] determines the name of every file denju writes beside the
// binary and every environment variable it sets. Those names are part of the
// on-disk contract between one version of a program and the next, so changing
// the namespace of a deployed program orphans any update that is in flight
// during the changeover. Choose it once.
package denju
