package denju

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// selftestEnv is the variable the "app" namespace derives for the selftest role.
// Spelled out rather than computed, so this test also fails if the derivation
// changes underneath it.
const selftestEnv = "APP_SELFTEST"

// TestSelftestHelperProcess is not a real test: it is the child process that
// runSelftestChild spawns. It runs only when APP_SELFTEST=1 - which
// runSelftestChild sets - behaves according to SELFTEST_HELPER_MODE, and exits,
// standing in for a staged binary validating itself offline.
func TestSelftestHelperProcess(t *testing.T) {
	if os.Getenv(selftestEnv) != "1" {
		return
	}
	switch os.Getenv("SELFTEST_HELPER_MODE") {
	case "fail":
		fmt.Fprintln(os.Stderr, "selftest child: configuration is invalid")
		os.Exit(1)
	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		os.Exit(0)
	}
}

func runTestSelftest(timeout time.Duration) error {
	return runSelftestChild(newNames("app"), timeout,
		os.Args[0], []string{"-test.run=TestSelftestHelperProcess"}, "")
}

func TestRunSelftest_Pass(t *testing.T) {
	t.Setenv("SELFTEST_HELPER_MODE", "pass")
	noErr(t, runTestSelftest(30*time.Second), "a child that exits zero passes")
}

// The child's stderr tail becomes the operator-facing reason the update was
// refused, and it is usually the only explanation anyone will ever see.
func TestRunSelftest_NonZeroExitCapturesStderr(t *testing.T) {
	t.Setenv("SELFTEST_HELPER_MODE", "fail")
	err := runTestSelftest(30 * time.Second)
	wantErrContaining(t, err, "update selftest failed", "a child that exits non-zero fails")
	wantErrContaining(t, err, "configuration is invalid", "the child's stderr tail must be carried")
}

func TestRunSelftest_Timeout(t *testing.T) {
	t.Setenv("SELFTEST_HELPER_MODE", "hang")
	err := runTestSelftest(100 * time.Millisecond)
	wantErrContaining(t, err, "timed out", "a child that hangs past the timeout fails")
}

func TestTailBuffer_KeepsLastBytes(t *testing.T) {
	tb := &tailBuffer{max: 4}
	_, _ = tb.Write([]byte("abcdef"))
	_, _ = tb.Write([]byte("gh"))
	eq(t, string(tb.bytes()), "efgh", "tailBuffer keeps only the last bytes")
}

// The selftest role runs the caller's function and reports the verdict as an
// exit code. A nil Selftest still means something: the binary started and got
// this far without crashing.
func TestRunSelftestRole(t *testing.T) {
	t.Run("nil selftest passes", func(t *testing.T) {
		u := testUpdater(t)
		eq(t, u.runSelftestRole(), 0, "exit code")
	})

	t.Run("passing selftest exits zero", func(t *testing.T) {
		u := testUpdater(t, func(c *Config) { c.Selftest = func() error { return nil } })
		eq(t, u.runSelftestRole(), 0, "exit code")
	})

	t.Run("failing selftest exits non-zero", func(t *testing.T) {
		u := testUpdater(t, func(c *Config) {
			c.Selftest = func() error { return errors.New("config is unreadable") }
		})
		eq(t, u.runSelftestRole(), 1, "exit code")
	})
}

// RunProcessRole must route a selftest child into the selftest role, not into
// the program's normal startup.
func TestRunProcessRole_DispatchesTheSelftestChild(t *testing.T) {
	u := testUpdater(t, func(c *Config) {
		c.Selftest = func() error { return errors.New("nope") }
	})
	t.Setenv(u.n.envSelftest, "1")

	code, isRole := u.RunProcessRole()
	isTrue(t, isRole, "a selftest child is a role")
	eq(t, code, 1, "the verdict is the exit code")
}

// The selftest child is the one place a program's own code runs from a binary
// that has not been trusted yet, so the environment marker has to be exact.
func TestSelftestEnvMatchesTheNamingContract(t *testing.T) {
	eq(t, newNames("app").envSelftest, selftestEnv, "derived selftest variable")
	isTrue(t, strings.HasSuffix(selftestEnv, "_SELFTEST"), "the suffix is part of the contract")
}
