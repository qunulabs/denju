package denju

import "testing"

// The exec seam must forward everything verbatim. argv[0] in particular: a
// program that inspects its own invocation, or a supervisor matching on the
// command line, must see exactly what it saw before the update.
func TestExecSelf_ForwardsVerbatim(t *testing.T) {
	var gotPath string
	var gotArgv, gotEnv []string

	prev := execSelf
	execSelf = func(path string, argv []string, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	}
	t.Cleanup(func() { execSelf = prev })

	wantArgv := []string{"/opt/prog/prog", "--config", "/etc/prog.yaml"}
	wantEnv := []string{"PATH=/usr/bin", "HOME=/root"}
	noErr(t, execSelf("/opt/prog/prog", wantArgv, wantEnv), "execSelf")

	eq(t, gotPath, "/opt/prog/prog", "path")
	eq(t, len(gotArgv), len(wantArgv), "len(argv)")
	for i := range wantArgv {
		eq(t, gotArgv[i], wantArgv[i], "argv element")
	}
	eq(t, len(gotEnv), len(wantEnv), "len(env)")
	for i := range wantEnv {
		eq(t, gotEnv[i], wantEnv[i], "env element")
	}
}
