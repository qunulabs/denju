package denju

import (
	"os"
	"strings"
	"testing"
)

// TestNoTestDependencies keeps the module graph a consumer inherits down to one
// entry, and that one only on Windows.
//
// A library that a program depends on for its ability to start at all should not
// pull an assertion framework into everybody's build.
func TestNoTestDependencies(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	noErr(t, err, "read go.mod")

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") ||
			strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "go ") ||
			line == "require (" || line == ")" {
			continue
		}
		if !strings.HasPrefix(line, "require golang.org/x/sys") &&
			!strings.HasPrefix(line, "golang.org/x/sys") {
			t.Errorf("unexpected dependency in go.mod: %q", line)
		}
	}
}
