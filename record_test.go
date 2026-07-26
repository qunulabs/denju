package denju

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	return &store{
		path:       filepath.Join(t.TempDir(), "last-update.json"),
		tmpPattern: ".app-record-*",
		cooldown:   testCooldown,
	}
}

// TestStatusWireValues pins the status strings. They are persisted in the
// outcome record and read back by a successor process, so a rename would make
// every existing record unreadable.
func TestStatusWireValues(t *testing.T) {
	eq(t, string(StatusSucceeded), "succeeded", "StatusSucceeded")
	eq(t, string(StatusFailed), "failed", "StatusFailed")
	eq(t, string(StatusRolledBack), "rolled_back", "StatusRolledBack")
	eq(t, string(StatusRefusedCooldown), "refused_cooldown", "StatusRefusedCooldown")
}

// TestOutcomeWireCompatibility pins the record's JSON, which a pre-denju build
// wrote and a denju build has to keep reading.
func TestOutcomeWireCompatibility(t *testing.T) {
	const fixture = `{
  "at": "2026-07-26T18:09:44.117203Z",
  "command_id": "cmd-42",
  "from_version": "1.3.2",
  "to_version": "1.4.0",
  "status": "rolled_back",
  "error": "the new version did not register within 5m0s",
  "reported": false,
  "cooldown_at": "2026-07-26T18:09:44.117203Z"
}`
	s := newTestStore(t)
	noErr(t, os.WriteFile(s.path, []byte(fixture), 0o600), "write the fixture")

	o, err := s.load()
	noErr(t, err, "parse a record written by a pre-denju build")

	eq(t, o.ID, "cmd-42", "command_id")
	eq(t, o.FromVersion, "1.3.2", "from_version")
	eq(t, o.ToVersion, "1.4.0", "to_version")
	eq(t, o.Status, StatusRolledBack, "status")
	eq(t, o.Error, "the new version did not register within 5m0s", "error")
	isFalse(t, o.Reported, "reported")
	eq(t, o.At.UTC().Format(time.RFC3339), "2026-07-26T18:09:44Z", "at")
	eq(t, o.CooldownAt.UTC().Format(time.RFC3339), "2026-07-26T18:09:44Z", "cooldown_at")
}

func TestStore_LoadMissingIsNilNotError(t *testing.T) {
	o, err := newTestStore(t).load()
	noErr(t, err, "load a missing record")
	if o != nil {
		t.Fatalf("expected nil, got %+v", o)
	}
}

func TestStore_SaveLoadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	want := &Outcome{
		At:          time.Now().UTC().Truncate(time.Second),
		ID:          "cmd-1",
		FromVersion: "1.3.0",
		ToVersion:   "1.4.0",
		Status:      StatusSucceeded,
	}
	noErr(t, s.save(want), "save")

	got, err := s.load()
	noErr(t, err, "load")
	isTrue(t, got.At.Equal(want.At), "At round-trips")
	eq(t, got.ID, want.ID, "ID")
	eq(t, got.FromVersion, want.FromVersion, "FromVersion")
	eq(t, got.ToVersion, want.ToVersion, "ToVersion")
	eq(t, got.Status, want.Status, "Status")
}

// A corrupt record must be an error, not a silent nil: treating it as "no
// record" would disable the cooldown, which is the exact failure the cooldown
// exists to prevent.
func TestStore_CorruptRecordIsAnError(t *testing.T) {
	s := newTestStore(t)
	noErr(t, os.WriteFile(s.path, []byte("{nope"), 0o600), "write a corrupt record")

	_, err := s.load()
	wantErrContaining(t, err, "corrupt", "load a corrupt record")
}

// A program that just completed an update refuses another. This is the flap the
// cooldown exists for: a control plane mid-rollout can hand out the old version
// and the new one in turn, and without a cooldown the program bounces between
// them, re-downloading every time.
func TestInCooldown_JustUpdatedIsRefused(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	noErr(t, s.record(&Outcome{At: now.Add(-time.Minute), Status: StatusSucceeded}), "record")

	in, left, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isTrue(t, in, "an update a minute ago is inside the window")
	isTrue(t, math.Abs(left.Seconds()-(testCooldown-time.Minute).Seconds()) < 1, "remaining time")
}

// The common case: a program that has been running for a while moves
// immediately when it is told to.
func TestInCooldown_OldRecordIsNotRefused(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	noErr(t, s.record(&Outcome{At: now.Add(-testCooldown - time.Minute), Status: StatusSucceeded}), "record")

	in, _, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isFalse(t, in, "an expired anchor holds nothing back")
}

func TestInCooldown_NoRecordIsNotRefused(t *testing.T) {
	in, _, err := newTestStore(t).inCooldown(time.Now())
	noErr(t, err, "inCooldown")
	isFalse(t, in, "no record, no cooldown")
}

// A zero Cooldown disables the feature outright. This is the default, and it is
// what keeps denju's behaviour identical for a program that never asked for a
// cooldown.
func TestInCooldown_ZeroCooldownIsDisabled(t *testing.T) {
	s := newTestStore(t)
	s.cooldown = 0
	noErr(t, s.record(&Outcome{At: time.Now(), Status: StatusSucceeded}), "record")

	in, left, err := s.inCooldown(time.Now())
	noErr(t, err, "inCooldown")
	isFalse(t, in, "a zero cooldown never refuses")
	eq(t, left, time.Duration(0), "remaining time")
}

// A refusal is not an update, so it must not extend the cooldown - otherwise
// repeatedly refusing would keep a program stuck forever. With no completed
// update behind it there is no anchor at all, so nothing is held back.
func TestInCooldown_RefusalDoesNotExtend(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	noErr(t, s.record(&Outcome{At: now, Status: StatusRefusedCooldown}), "record")

	in, _, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isFalse(t, in, "a lone refusal holds nothing back")

	// And the refusal must not have invented an anchor of its own.
	o, err := s.load()
	noErr(t, err, "load")
	isTrue(t, o.CooldownAt.IsZero(), "a refusal must not set the cooldown anchor")
}

// A refusal recorded a long time after the update that caused it lets the next
// request through: the anchor it carried forward has expired on its own.
func TestInCooldown_RefusalDoesNotHoldPastTheWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	noErr(t, s.record(&Outcome{At: now.Add(-testCooldown - time.Minute), Status: StatusSucceeded}), "record the update")
	noErr(t, s.record(&Outcome{At: now, Status: StatusRefusedCooldown}), "record the refusal")

	in, _, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isFalse(t, in, "an expired anchor stays expired across a refusal")
}

// The regression that matters: refusing must not CLEAR the cooldown either.
// Recording a refusal has to leave the anchor where the last completed update
// put it, so a second request arriving straight afterwards is refused too.
// Otherwise the cooldown is bypassable by simply retrying, and the flap it
// exists to stop happens on the very next attempt.
func TestInCooldown_RefusalDoesNotClearTheCooldown(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	noErr(t, s.record(&Outcome{
		At: now.Add(-time.Minute), ID: "cmd-1",
		FromVersion: "1.3.0", ToVersion: "1.4.0", Status: StatusSucceeded,
	}), "record the update")

	in, _, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isTrue(t, in, "an update a minute ago must be inside the cooldown")

	// A request arrives and is refused; the refusal is recorded so it can be
	// reported.
	noErr(t, s.record(&Outcome{
		At: now, ID: "cmd-2",
		FromVersion: "1.4.0", ToVersion: "1.3.0", Status: StatusRefusedCooldown,
	}), "record the refusal")

	in, left, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isTrue(t, in, "the cooldown must survive a refusal, not be erased by it")
	isTrue(t, math.Abs(left.Seconds()-(testCooldown-time.Minute).Seconds()) < 1,
		"the remaining time must still count from the completed update, not the refusal")
}

// Preserving the anchor must not cost us the report: a refusal is still an
// outcome the caller is owed, so it becomes the pending one even when the
// outcome it replaces had already been reported.
func TestRecord_RefusalIsStillReported(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()

	noErr(t, s.record(&Outcome{At: now.Add(-time.Minute), ID: "cmd-1", Status: StatusSucceeded}), "record")
	noErr(t, s.markReported(), "markReported")
	pending, err := s.pending()
	noErr(t, err, "pending")
	if pending != nil {
		t.Fatal("a reported outcome is not pending")
	}

	noErr(t, s.record(&Outcome{
		At: now, ID: "cmd-2", Status: StatusRefusedCooldown,
		FromVersion: "1.4.0", ToVersion: "1.3.0",
	}), "record the refusal")

	pending, err = s.pending()
	noErr(t, err, "pending")
	if pending == nil {
		t.Fatal("the refusal is owed to the caller")
	}
	eq(t, pending.ID, "cmd-2", "pending id")
	eq(t, pending.Status, StatusRefusedCooldown, "pending status")
}

// A failed or rolled-back attempt DOES hold the cooldown: a target that cannot
// be installed must not be retried in a tight loop.
func TestInCooldown_FailureHoldsTheCooldown(t *testing.T) {
	for _, status := range []Status{StatusFailed, StatusRolledBack} {
		s := newTestStore(t)
		now := time.Now()
		noErr(t, s.record(&Outcome{At: now.Add(-time.Minute), Status: status}), "record")

		in, _, err := s.inCooldown(now)
		noErr(t, err, "inCooldown")
		isTrue(t, in, string(status)+" must hold the cooldown")
	}
}

// A clock that jumped backwards leaves a record "in the future"; that must not
// read as an indefinite cooldown.
func TestInCooldown_FutureRecordIsNotRefused(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	noErr(t, s.record(&Outcome{At: now.Add(time.Hour), Status: StatusSucceeded}), "record")

	in, _, err := s.inCooldown(now)
	noErr(t, err, "inCooldown")
	isFalse(t, in, "a future-dated anchor is treated as no anchor")
}

func TestPendingAndMarkReported(t *testing.T) {
	s := newTestStore(t)
	noErr(t, s.save(&Outcome{
		At:          time.Now(),
		ID:          "cmd-9",
		FromVersion: "1.3.0",
		ToVersion:   "1.4.0",
		Status:      StatusRolledBack,
		Error:       "did not attest",
	}), "save")

	pending, err := s.pending()
	noErr(t, err, "pending")
	if pending == nil {
		t.Fatal("expected a pending outcome")
	}
	eq(t, pending.ID, "cmd-9", "id")
	eq(t, pending.Status, StatusRolledBack, "status")
	eq(t, pending.Error, "did not attest", "error")

	noErr(t, s.markReported(), "markReported")

	pending, err = s.pending()
	noErr(t, err, "pending after markReported")
	if pending != nil {
		t.Fatal("a reported outcome must not be sent again")
	}

	// The record itself survives - the cooldown still needs its timestamp.
	o, err := s.load()
	noErr(t, err, "load")
	if o == nil {
		t.Fatal("the record must outlive the report")
	}
	isTrue(t, o.Reported, "reported flag")
}

// A record with no request id did not come from anything a report can be tied
// to, so there is nothing to send.
func TestPending_NoRequestIDIsNothingToReport(t *testing.T) {
	s := newTestStore(t)
	noErr(t, s.save(&Outcome{At: time.Now(), Status: StatusSucceeded}), "save")

	pending, err := s.pending()
	noErr(t, err, "pending")
	if pending != nil {
		t.Fatal("expected nothing to report")
	}
}

func TestMarkReported_NoRecordIsANoOp(t *testing.T) {
	noErr(t, newTestStore(t).markReported(), "markReported with no record")
}

// The record is rewritten in place on every attempt, so it must not accumulate
// temp files in a state directory that nothing else cleans.
func TestStore_LeavesNoTempFiles(t *testing.T) {
	s := newTestStore(t)
	noErr(t, s.record(&Outcome{At: time.Now(), Status: StatusSucceeded}), "first")
	noErr(t, s.record(&Outcome{At: time.Now(), Status: StatusFailed}), "second")

	entries, err := os.ReadDir(filepath.Dir(s.path))
	noErr(t, err, "read the directory")
	eq(t, len(entries), 1, "only the record itself should remain")
}
