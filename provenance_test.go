package denju

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoProvenanceLeaks keeps this repository publishable.
//
// denju was extracted from two closed-source programs, and the code carries
// their shape: comments that explained a swap in terms of "the agent", error
// strings naming a product, environment variables spelled after a brand. None of
// that is anyone else's business, and a library that mentions the systems it came
// from is a library nobody else will adopt.
//
// This runs as a test rather than a CI-only grep so that it fails on the machine
// where the mistake is made, not two minutes later in a pull request.
func TestNoProvenanceLeaks(t *testing.T) {
	// Terms that must not appear anywhere in the source or documentation.
	forbidden := []string{
		"sentinel",
		"qshield",
		"qai",
		"qx-agent",
		"qnulabs",
		"qnu labs",
		".lic",
		"heartbeat",
		"drm",
		"license file",
	}

	// Only this file, which has to name the terms in order to look for them.
	// Nothing else is exempt - not even the naming-contract test, which uses
	// invented namespaces precisely so that it need not be.
	exempt := map[string]bool{"provenance_test.go": true}

	root := "."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".md" && ext != ".yml" && ext != ".yaml" {
			return nil
		}
		if exempt[filepath.Base(path)] {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lower := strings.ToLower(string(data))
		for _, term := range forbidden {
			if idx := strings.Index(lower, term); idx >= 0 {
				line := 1 + strings.Count(lower[:idx], "\n")
				t.Errorf("%s:%d contains %q - denju must not name the systems it was extracted from",
					path, line, term)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
}

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
