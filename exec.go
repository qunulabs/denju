package denju

// execSelf swaps the current process image for the binary at path, replaying
// argv and env verbatim. On Unix it is syscall.Exec, which never returns on
// success: the image is replaced inside the SAME PID, so systemd's Restart=,
// launchd's KeepAlive, and any container restart policy are never consulted -
// from the supervisor's point of view nothing happened. On Windows there is no
// equivalent and it returns an error.
//
// It is a package var so tests can intercept the image swap instead of actually
// replacing the running test binary.
var execSelf func(path string, argv []string, env []string) error = defaultExecSelf
