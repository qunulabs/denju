//go:build !windows

package denju

import "errors"

// The detached-helper machinery is Windows-only: Unix updates and restarts go
// through exec-in-place and never spawn a helper. These stubs exist so the
// portable flow compiles everywhere. They are unreachable in production (goos
// gates every call site) and tests override the seams to exercise the Windows
// flow on a Unix host.

func defaultSpawnDetached(_ string, _ []string, _ string, _ []string) (int, error) {
	return 0, errors.New("detached helper spawn is only available on windows")
}

func defaultStartService(_ string) error {
	return errors.New("the service control manager is only available on windows")
}

// defaultServiceIdentity reports "no service" rather than an error: on Unix a
// program is never service-control-manager-hosted, and the update flow reads
// this as "restart by exec", which is correct.
func defaultServiceIdentity() (string, error) {
	return "", nil
}
