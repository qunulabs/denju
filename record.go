package denju

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Status is how an update attempt ended.
//
// The values are the strings persisted in the outcome record, so they are part
// of the on-disk contract and must not be renamed.
type Status string

// Update outcomes.
const (
	// StatusSucceeded means the new version was installed and attested healthy.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the update did not happen. The program is still running
	// the version it was running before, untouched.
	StatusFailed Status = "failed"
	// StatusRolledBack means the new version was installed, failed to prove
	// itself, and the previous version was restored.
	StatusRolledBack Status = "rolled_back"
	// StatusRefusedCooldown means the update was declined because another one
	// completed too recently. See [Config.Cooldown].
	StatusRefusedCooldown Status = "refused_cooldown"
)

// Outcome is the durable memory of the last update attempt.
//
// It exists because a successful update destroys the process that performed it:
// there is no return value to inspect and no error to bubble up. The outcome is
// written by one process and read back by its successor, which reports it
// onward and then calls [Updater.MarkReported].
//
// It carries two independent things, and keeping them apart is the whole point
// of the shape. At and the fields describing the attempt are the LAST ATTEMPT,
// which is what gets reported - and a refused attempt is an attempt, so it
// overwrites them. CooldownAt is the anchor the cooldown measures from, which
// only a COMPLETED attempt may move.
type Outcome struct {
	// At is when this attempt finished - completed or refused alike.
	At time.Time `json:"at"`
	// ID echoes [Request.ID], so a report can be tied back to what asked for it.
	ID string `json:"command_id"`
	// FromVersion and ToVersion describe the move that was attempted.
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	// Status is how it ended; Error carries the cause when it did not succeed.
	Status Status `json:"status"`
	Error  string `json:"error,omitempty"`
	// Reported is set once the caller has confirmed delivery. The record itself
	// stays on disk afterwards, because the cooldown still needs its timestamp.
	Reported bool `json:"reported"`
	// CooldownAt is when the last attempt that actually COMPLETED finished -
	// succeeded, failed or rolled back. The cooldown measures from here, and a
	// refusal carries it forward untouched. Zero means no update has ever
	// completed, so nothing is being held back.
	//
	// Maintained by the store; do not set it by hand.
	CooldownAt time.Time `json:"cooldown_at"`
}

// store reads and writes the outcome record.
type store struct {
	path       string
	tmpPattern string
	cooldown   time.Duration
}

// load returns the stored outcome, or nil when there is none.
//
// A record that cannot be parsed is reported as an error rather than silently
// ignored: it would otherwise disable the cooldown without anyone noticing,
// which is the exact failure the cooldown exists to prevent.
func (s *store) load() (*Outcome, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the update record: %w", err)
	}
	var o Outcome
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("the update record at %s is corrupt: %w", s.path, err)
	}
	return &o, nil
}

// save atomically writes the record.
func (s *store) save(o *Outcome) error {
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the update record: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, s.tmpPattern)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write the update record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush the update record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize the update record: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write the update record: %w", err)
	}
	return nil
}

// record persists the outcome of an attempt and maintains the cooldown anchor.
//
// Every write of an attempt goes through here rather than save, because the
// anchor rule is easy to get wrong and getting it wrong is not visible: the
// anchor moves only for an attempt that actually COMPLETED (succeeded, failed or
// rolled back). A refusal must still be recorded - the caller is owed the
// outcome - but it neither extends the cooldown nor clears it; it inherits the
// existing anchor. A refusal that reset the anchor to its own timestamp would
// extend the cooldown forever; one that dropped it would let a caller bypass the
// cooldown simply by retrying.
func (s *store) record(o *Outcome) error {
	if o.Status == StatusRefusedCooldown {
		prev, err := s.load()
		if err != nil {
			return err
		}
		if prev != nil {
			o.CooldownAt = prev.CooldownAt
		}
	} else {
		o.CooldownAt = o.At
	}
	return s.save(o)
}

// inCooldown reports whether an update completed recently enough to refuse
// another, and how long is left. It reads the anchor record maintains, so
// refusals in between are invisible here - they neither extend the window nor
// cut it short.
func (s *store) inCooldown(now time.Time) (bool, time.Duration, error) {
	if s.cooldown <= 0 {
		return false, 0, nil
	}
	o, err := s.load()
	if err != nil {
		return false, 0, err
	}
	if o == nil || o.CooldownAt.IsZero() {
		return false, 0, nil
	}
	elapsed := now.Sub(o.CooldownAt)
	// A negative elapsed means the record is dated in the future - a clock that
	// jumped backwards, or a record copied from another host. Holding an update
	// back until wall-clock time catches up could mean holding it for years, so
	// an unusable anchor is treated as no anchor.
	if elapsed < 0 || elapsed >= s.cooldown {
		return false, 0, nil
	}
	return true, s.cooldown - elapsed, nil
}

// pending returns the outcome that still has to be reported, or nil.
func (s *store) pending() (*Outcome, error) {
	o, err := s.load()
	if err != nil {
		return nil, err
	}
	if o == nil || o.Reported || o.ID == "" {
		return nil, nil
	}
	return o, nil
}

// markReported records that the outcome reached its destination. The record
// stays on disk because the cooldown still needs its timestamp.
func (s *store) markReported() error {
	o, err := s.load()
	if err != nil {
		return err
	}
	if o == nil || o.Reported {
		return nil
	}
	o.Reported = true
	return s.save(o)
}
