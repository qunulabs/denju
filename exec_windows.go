//go:build windows

package denju

import "errors"

// defaultExecSelf reports that exec-in-place is unavailable on Windows, which
// has no equivalent of syscall.Exec. Windows restarts go through the detached
// helper instead (see helper.go); goos gates every caller, so this is only ever
// reached as a programming-error guard.
func defaultExecSelf(_ string, _ []string, _ []string) error {
	return errors.New("exec-in-place is not available on windows")
}
