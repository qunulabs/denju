// Command basic is a complete, runnable sketch of a long-running program that
// updates itself with denju.
//
// It is a sketch in one respect only: the "control plane" is a stub that returns
// a hardcoded update, because a real one is the part denju deliberately knows
// nothing about. Everything else - where each call goes, what has to happen
// before what, and who decides the program is healthy - is exactly what a real
// program does.
//
//	go run ./examples/basic
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qunulabs/denju"
	"github.com/qunulabs/denju/httpsource"
)

// version is stamped at build time: go build -ldflags "-X main.version=1.4.0"
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	srv := &http.Server{Addr: ":8080", Handler: routes()}

	u, err := denju.New(denju.Config{
		Namespace: "basic",
		Version:   version,
		Log:       denju.SlogLogger(log),

		// Runs in the DOWNLOADED binary, as a child process, before anything is
		// swapped. Keep it cheap and side-effect free: the current version is
		// still running and still owns the port, the state directory, and
		// whatever locks the program holds.
		Selftest: loadConfig,

		// Runs once the update is certain to go ahead. This is where the program
		// stops serving.
		Drain: srv.Shutdown,

		// Refuse a second update for half an hour after one completes, so a
		// control plane that can hand out two different versions cannot bounce
		// this program between them.
		Cooldown: 30 * time.Minute,
	})
	if err != nil {
		return err
	}

	// (1) FIRST. This process may have been started as an update helper or as a
	// selftest child, in which case it must do that and nothing else. Anything
	// before this line runs in those processes too, so keep it passive.
	if code, isRole := u.RunProcessRole(); isRole {
		os.Exit(code)
	}

	// (2) Before the port is bound. A rollback restart hands off to a successor
	// that needs it.
	if err := u.Repair(); err != nil {
		return fmt.Errorf("resolve an interrupted update: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("serving", "version", version, "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the server stopped", "err", err)
			stop()
		}
	}()

	// (3) Arm the deadline for an update this process might be the result of. A
	// no-op when there isn't one, so it is called unconditionally.
	u.Attest(ctx, 5*time.Minute)

	// Tell whoever asked for it how the last update went. The outcome usually
	// belongs to a process that no longer exists, which is why it is on disk.
	reportLastUpdate(ctx, log, u)

	// This program is now serving, which is all "healthy" means here. A real one
	// would wait for something more convincing - a registration accepted, a
	// health probe passing, one real request completed.
	if err := u.Commit(); err != nil {
		log.Error("could not commit the update", "err", err)
	}

	watchForUpdates(ctx, log, u)

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// watchForUpdates polls a control plane and applies whatever it is told to.
func watchForUpdates(ctx context.Context, log *slog.Logger, u *denju.Updater) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				available, ok := checkForUpdate()
				if !ok {
					continue
				}
				// Update does not return when it succeeds: this process either
				// becomes the new binary or exits so a helper can install it. A
				// returned result always means the update did not happen.
				res := u.Update(ctx, available.request, available.source)
				log.Warn("the update did not proceed",
					"status", res.Status, "reason", res.Error)
			}
		}
	}()
}

// reportLastUpdate delivers the outcome of an update this process may not have
// performed, and clears it once delivery is confirmed.
func reportLastUpdate(ctx context.Context, log *slog.Logger, u *denju.Updater) {
	out, err := u.PendingOutcome()
	if err != nil {
		log.Error("could not read the update record", "err", err)
		return
	}
	if out == nil {
		return
	}

	log.Info("reporting the last update",
		"id", out.ID, "status", out.Status,
		"from", out.FromVersion, "to", out.ToVersion, "error", out.Error)

	if err := deliverOutcome(ctx, out); err != nil {
		// Keep it: the next start will try again. Reporting twice is harmless;
		// never reporting leaves an operator wondering why a host is on an old
		// version.
		log.Warn("could not deliver the outcome; keeping it for the next start", "err", err)
		return
	}
	if err := u.MarkReported(); err != nil {
		log.Error("could not mark the outcome reported", "err", err)
		return
	}
	u.CleanupReported()
}

// --- everything below stands in for a real deployment ---------------------

type availableUpdate struct {
	request denju.Request
	source  denju.Source
}

// checkForUpdate stands in for a control plane.
//
// A real implementation returns a digest it can vouch for. denju checks that the
// bytes match this digest and nothing else, so whatever authenticates it - a
// signed manifest, an authenticated RPC - is where the security of the whole
// mechanism actually lives.
func checkForUpdate() (availableUpdate, bool) {
	return availableUpdate{}, false
}

// deliverOutcome stands in for reporting to a control plane.
func deliverOutcome(context.Context, *denju.Outcome) error { return nil }

// loadConfig stands in for the program's own startup validation. It is what the
// selftest runs inside a binary that has not been trusted yet, so it should
// touch nothing the running version owns.
func loadConfig() error { return nil }

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, version)
	})
	return mux
}

// unused keeps the httpsource import meaningful in this sketch: a real
// checkForUpdate would build its Source like this.
var _ = func(url string) denju.Source {
	return httpsource.New(url, httpsource.WithMaxBytes(512<<20))
}
