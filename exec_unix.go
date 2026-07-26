//go:build unix

package denju

import "syscall"

// defaultExecSelf replaces the running program image with the binary at path,
// inside the same PID, via syscall.Exec. It never returns on success - every
// goroutine and the old program image are destroyed atomically and control jumps
// into the new binary's entry point.
//
// Advisory file locks held on descriptors opened by Go are released
// automatically: Go opens files with O_CLOEXEC, so those descriptors do not
// survive the exec and the successor is free to acquire them.
func defaultExecSelf(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}
