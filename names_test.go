package denju

import (
	"strings"
	"testing"
)

// TestNamingContract pins every name denju derives from a namespace.
//
// This is a compatibility lock, not a unit test. The derived names are the
// interface between one deployed version of a program and the next: the version
// being replaced writes them, and the version replacing it has to find them.
// Change any shape below and a program that ships the new denju will, during the
// changeover, fail to resolve an update that was in flight - no commit, no
// outcome reported, the rollback copy left on disk for good, and on Windows a
// helper process with nothing to hand over to.
//
// The namespaces used here are invented. It is the SHAPES that are frozen, and
// they are frozen for every namespace equally, so nothing is gained by testing
// the real ones.
func TestNamingContract(t *testing.T) {
	cases := []struct {
		namespace string

		envMode     string
		envState    string
		envSelftest string

		sufState   string
		sufRestart string
		sufHelper  string
		sufDiscard string
		sufLog     string
		sufRecord  string

		tmpState     string
		tmpDownload  string
		tmpPermCheck string
		tmpRecord    string

		logPrefix string

		prePartial string
		extPartial string

		sufPrevious       string
		sufPreviousRecord string
		tmpPrevious       string
	}{
		{
			namespace: "acme",

			envMode:     "ACME_UPDATE_MODE",
			envState:    "ACME_UPDATE_STATE",
			envSelftest: "ACME_SELFTEST",

			sufState:   ".acme-update.state.json",
			sufRestart: ".acme-restart.state.json",
			sufHelper:  ".acme-updater",
			sufDiscard: ".acme-update.discard",
			sufLog:     ".acme-update.log",
			sufRecord:  ".acme-record.json",

			tmpState:     ".acme-state-*",
			tmpDownload:  ".acme-update-*",
			tmpPermCheck: ".acme-permcheck-*",
			tmpRecord:    ".acme-record-*",

			logPrefix: "acme-updater: ",

			prePartial: ".acme-download-",
			extPartial: ".partial",

			sufPrevious:       ".acme-previous",
			sufPreviousRecord: ".acme-previous.json",
			tmpPrevious:       ".acme-previous-*",
		},
		{
			// A dashed namespace, because an environment variable cannot carry a
			// dash: the derivation has to translate it, and every deployed
			// program with a dashed namespace depends on it translating the same
			// way forever.
			namespace: "widget-co",

			envMode:     "WIDGET_CO_UPDATE_MODE",
			envState:    "WIDGET_CO_UPDATE_STATE",
			envSelftest: "WIDGET_CO_SELFTEST",

			sufState:   ".widget-co-update.state.json",
			sufRestart: ".widget-co-restart.state.json",
			sufHelper:  ".widget-co-updater",
			sufDiscard: ".widget-co-update.discard",
			sufLog:     ".widget-co-update.log",
			sufRecord:  ".widget-co-record.json",

			tmpState:     ".widget-co-state-*",
			tmpDownload:  ".widget-co-update-*",
			tmpPermCheck: ".widget-co-permcheck-*",
			tmpRecord:    ".widget-co-record-*",

			logPrefix: "widget-co-updater: ",

			prePartial: ".widget-co-download-",
			extPartial: ".partial",

			sufPrevious:       ".widget-co-previous",
			sufPreviousRecord: ".widget-co-previous.json",
			tmpPrevious:       ".widget-co-previous-*",
		},
	}

	for _, tc := range cases {
		t.Run(tc.namespace, func(t *testing.T) {
			n := newNames(tc.namespace)
			for _, f := range []struct{ field, got, want string }{
				{"envMode", n.envMode, tc.envMode},
				{"envState", n.envState, tc.envState},
				{"envSelftest", n.envSelftest, tc.envSelftest},
				{"sufState", n.sufState, tc.sufState},
				{"sufRestart", n.sufRestart, tc.sufRestart},
				{"sufHelper", n.sufHelper, tc.sufHelper},
				{"sufDiscard", n.sufDiscard, tc.sufDiscard},
				{"sufLog", n.sufLog, tc.sufLog},
				{"sufRecord", n.sufRecord, tc.sufRecord},
				{"tmpState", n.tmpState, tc.tmpState},
				{"tmpDownload", n.tmpDownload, tc.tmpDownload},
				{"tmpPermCheck", n.tmpPermCheck, tc.tmpPermCheck},
				{"tmpRecord", n.tmpRecord, tc.tmpRecord},
				{"logPrefix", n.logPrefix, tc.logPrefix},
				{"prePartial", n.prePartial, tc.prePartial},
				{"extPartial", n.extPartial, tc.extPartial},
				{"sufPrevious", n.sufPrevious, tc.sufPrevious},
				{"sufPreviousRecord", n.sufPreviousRecord, tc.sufPreviousRecord},
				{"tmpPrevious", n.tmpPrevious, tc.tmpPrevious},
			} {
				if f.got != f.want {
					t.Errorf("%s = %q, want %q (this is an on-disk compatibility break)", f.field, f.got, f.want)
				}
			}
		})
	}
}

// TestNamingContractPaths pins the full file paths derived beside a binary,
// including the .exe the Windows helper copy must carry to be executable at all.
func TestNamingContractPaths(t *testing.T) {
	const bin = "/opt/app/myprog"

	t.Run("unix", func(t *testing.T) {
		withGOOS(t, "linux")
		expectPaths(t, newNames("acme").pathsFor(bin), paths{
			State:          "/opt/app/myprog.acme-update.state.json",
			Restart:        "/opt/app/myprog.acme-restart.state.json",
			RollbackBinary: "/opt/app/myprog.old",
			HelperCopy:     "/opt/app/myprog.acme-updater",
			Discard:        "/opt/app/myprog.acme-update.discard",
			HelperLog:      "/opt/app/myprog.acme-update.log",
			Record:         "/opt/app/myprog.acme-record.json",
			Previous:       "/opt/app/myprog.acme-previous",
			PreviousRecord: "/opt/app/myprog.acme-previous.json",
		})
	})

	t.Run("windows", func(t *testing.T) {
		withGOOS(t, "windows")
		expectPaths(t, newNames("acme").pathsFor(bin), paths{
			State:          "/opt/app/myprog.acme-update.state.json",
			Restart:        "/opt/app/myprog.acme-restart.state.json",
			RollbackBinary: "/opt/app/myprog.old",
			HelperCopy:     "/opt/app/myprog.acme-updater.exe",
			Discard:        "/opt/app/myprog.acme-update.discard",
			HelperLog:      "/opt/app/myprog.acme-update.log",
			Record:         "/opt/app/myprog.acme-record.json",
			// No .exe on Windows: the retained binary is never executed in place.
			Previous:       "/opt/app/myprog.acme-previous",
			PreviousRecord: "/opt/app/myprog.acme-previous.json",
		})
	})
}

func expectPaths(t *testing.T, got, want paths) {
	t.Helper()
	for _, f := range []struct{ field, got, want string }{
		{"State", got.State, want.State},
		{"Restart", got.Restart, want.Restart},
		{"RollbackBinary", got.RollbackBinary, want.RollbackBinary},
		{"HelperCopy", got.HelperCopy, want.HelperCopy},
		{"Discard", got.Discard, want.Discard},
		{"HelperLog", got.HelperLog, want.HelperLog},
		{"Record", got.Record, want.Record},
		{"Previous", got.Previous, want.Previous},
		{"PreviousRecord", got.PreviousRecord, want.PreviousRecord},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q (this is an on-disk compatibility break)", f.field, f.got, f.want)
		}
	}
}

// The rollback copy is the one derived name with no namespace in it. That is
// deliberate and load-bearing: it is the file an operator reaches for at three
// in the morning when everything else has failed, and "<binary>.old" is the name
// they will guess.
func TestNamingContract_RollbackCopyIsNamespaceFree(t *testing.T) {
	for _, ns := range []string{"acme", "widget-co", "x"} {
		p := newNames(ns).pathsFor("/opt/app/myprog")
		eq(t, p.RollbackBinary, "/opt/app/myprog.old", "rollback copy for namespace "+ns)
	}
}

func TestValidateNamespace(t *testing.T) {
	valid := []string{"a", "z9", "acme", "widget-co", "app2"}
	for _, ns := range valid {
		if err := validateNamespace(ns); err != nil {
			t.Errorf("validateNamespace(%q) = %v, want nil", ns, err)
		}
	}

	// Anything outside this set could produce a path that escapes the binary's
	// directory, or a variable name a shell cannot set.
	invalid := []string{"", "-lead", "Upper", "has space", "dot.ted", "sl/ash", "under_score", "..", "tra\\il"}
	for _, ns := range invalid {
		if err := validateNamespace(ns); err == nil {
			t.Errorf("validateNamespace(%q) = nil, want an error", ns)
		}
	}
}

// TestPartialKey pins how a partial download is named after its request. It is
// an on-disk contract like the rest of this file: a program that changes it
// orphans every partial on disk across the changeover.
func TestPartialKey(t *testing.T) {
	const sha = "9f2c1e0d4a6b8c3f5e7d9a1b2c4e6f8a0b1c3d5e7f9a1b3c5d7e9f0a2b4c6d8e"
	base := Request{ID: "cmd-42", TargetVersion: "1.4.0", SHA256: sha}

	eq(t, partialKey(base), "1d0e5a656aa7af44", "the key for a fixed request")

	upper := base
	upper.SHA256 = strings.ToUpper(sha)
	eq(t, partialKey(upper), partialKey(base), "a digest is the same digest in either case")

	withOffset := base
	withOffset.Offset = 99
	eq(t, partialKey(withOffset), partialKey(base), "the offset is not part of the request's identity")

	for name, r := range map[string]Request{
		"id":             {ID: "cmd-43", TargetVersion: base.TargetVersion, SHA256: sha},
		"target version": {ID: base.ID, TargetVersion: "1.4.1", SHA256: sha},
		"digest":         {ID: base.ID, TargetVersion: base.TargetVersion, SHA256: strings.Repeat("0", 64)},
	} {
		isTrue(t, partialKey(r) != partialKey(base), "a different "+name+" is a different partial")
	}

	u, err := New(Config{Namespace: "acme", Version: "1.0.0", BinaryPath: "/opt/app/myprog"})
	noErr(t, err, "New")
	eq(t, u.partialPath(base), "/opt/app/myprog.acme-download-1d0e5a656aa7af44.partial", "the partial's path")
}
