//go:build windows

package denju

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// detachedFlags fully separate the helper from the spawning process: its own
// process group (so no shared Ctrl+C or console events) and no console at all.
const detachedFlags = windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS

// defaultSpawnDetached starts path detached, first requesting a job-object
// breakaway - so a job configured to kill its members on close cannot take the
// helper down along with the process it is replacing - and retrying without it.
// Breakaway is denied unless the job sets JOB_OBJECT_LIMIT_BREAKAWAY_OK, and
// most processes are not in such a job, so the retry is the common path.
func defaultSpawnDetached(path string, args []string, cwd string, env []string) (int, error) {
	pid, err := spawnWithFlags(path, args, cwd, env, detachedFlags|windows.CREATE_BREAKAWAY_FROM_JOB)
	if err != nil {
		pid, err = spawnWithFlags(path, args, cwd, env, detachedFlags)
	}
	return pid, err
}

func spawnWithFlags(path string, args []string, cwd string, env []string, flags uint32) (int, error) {
	cmd := exec.Command(path, args...)
	cmd.Dir = cwd
	cmd.Env = env
	// stdio is deliberately nil: a detached process has no console, and
	// inheriting the old process's handles would tie the helper's lifetime to
	// them. The helper logs to the file beside the binary instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // do not hold a handle; the helper must outlive us
	return pid, nil
}

// defaultStartService asks the service control manager to start the named
// service, so the relaunched process IS the service and keeps its status,
// recovery actions and shutdown handling.
func defaultStartService(name string) error {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(m) }()

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("invalid service name %q: %w", name, err)
	}
	s, err := windows.OpenService(m, namePtr, windows.SERVICE_START)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer func() { _ = windows.CloseServiceHandle(s) }()

	if err := windows.StartService(s, 0, nil); err != nil {
		return fmt.Errorf("start service %q: %w", name, err)
	}
	return nil
}

// defaultServiceIdentity resolves whether this process runs as a Windows service
// and, if so, which one, by enumerating the active win32 services and matching
// our own PID. Failing to resolve while service-hosted is an error, not a shrug:
// an update must know how to bring the program back before it starts taking the
// running binary apart.
func defaultServiceIdentity() (string, error) {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return "", fmt.Errorf("determine whether this process is a Windows service: %w", err)
	}
	if !isSvc {
		return "", nil
	}
	return serviceNameByPID(uint32(os.Getpid()))
}

// serviceNameByPID finds the active service whose hosting process is pid.
func serviceNameByPID(pid uint32) (string, error) {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return "", fmt.Errorf("connect to the service control manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(m) }()

	// Standard two-call pattern: size the buffer, then enumerate.
	var bytesNeeded, servicesReturned, resume uint32
	_ = windows.EnumServicesStatusEx(m, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32,
		windows.SERVICE_ACTIVE, nil, 0, &bytesNeeded, &servicesReturned, &resume, nil)
	if bytesNeeded == 0 {
		return "", fmt.Errorf("could not enumerate Windows services")
	}
	buf := make([]byte, bytesNeeded)
	resume = 0
	if err := windows.EnumServicesStatusEx(m, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32,
		windows.SERVICE_ACTIVE, &buf[0], uint32(len(buf)), &bytesNeeded, &servicesReturned, &resume, nil); err != nil {
		return "", fmt.Errorf("enumerate Windows services: %w", err)
	}

	services := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), servicesReturned)
	for i := range services {
		if services[i].ServiceStatusProcess.ProcessId == pid {
			return windows.UTF16PtrToString(services[i].ServiceName), nil
		}
	}
	return "", fmt.Errorf("running as a Windows service but no active service matches pid %d", pid)
}
