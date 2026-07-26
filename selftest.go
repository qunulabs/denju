package denju

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// selftestStderrTail is how many trailing bytes of the child's stderr are kept
// to explain a failure.
const selftestStderrTail = 2 * 1024

// runSelftestChild makes the staged binary executable and runs it as a child
// with the program's own args and working directory plus <NS>_SELFTEST=1. The
// child must exit zero within the timeout. A non-zero exit or a timeout returns
// an error carrying the tail of the child's stderr, which becomes the
// operator-facing reason the update was refused.
//
// This is the cheapest possible guard against the worst outcome: a wrong-arch,
// truncated or otherwise unrunnable binary being installed over a working
// program on a machine nobody can reach. At this point the running program is
// untouched, so a failed selftest costs nothing.
func runSelftestChild(n names, timeout time.Duration, stagedPath string, args []string, cwd string) error {
	// chmod 0700: the download temp file is 0600, so the child could not
	// execute it.
	if err := os.Chmod(stagedPath, 0o700); err != nil {
		return fmt.Errorf("make the staged update executable: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, stagedPath, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), n.envSelftest+"=1")
	tail := &tailBuffer{max: selftestStderrTail}
	cmd.Stderr = tail

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("update selftest timed out after %s", timeout)
	}
	if err != nil {
		reason := string(tail.bytes())
		if reason == "" {
			reason = err.Error()
		}
		return fmt.Errorf("update selftest failed: %s", reason)
	}
	return nil
}

// tailBuffer is an io.Writer that retains only the last max bytes written, so a
// runaway child cannot balloon memory while we keep the tail that explains it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte { return t.buf }
