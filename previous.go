package denju

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// previousRecord describes the binary kept by Config.RetainPrevious. It lives
// beside that binary, is deleted before the binary is replaced, and is written
// only once its binary is in place - so a record on disk always describes the
// file next to it.
//
// The JSON field names are an on-disk contract between consecutive versions of
// a program, like the journal's.
type previousRecord struct {
	Version    string    `json:"version"`
	SHA256     string    `json:"sha256"`
	RetainedAt time.Time `json:"retained_at"`
}

// PreviousBinary reports the binary kept by [Config.RetainPrevious]: its path,
// its hex SHA-256 and the version it was. ok is false when nothing is retained.
//
// Compare sha256 with an update's digest; when they match, install it with
// [FileSource] and no byte crosses a network. The digest is re-checked during
// the install like any other download, so a retained file that has changed on
// disk is refused rather than installed.
//
// It has no error to return, so a record that cannot be read, or a record whose
// binary is gone, is logged at ERROR and reported as nothing retained - never
// as something retained.
func (u *Updater) PreviousBinary() (path, sha256, version string, ok bool) {
	rec, err := readPreviousRecord(u.paths.PreviousRecord)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			u.log(LevelError, "could not read the record of the retained previous binary", "err", err)
		}
		return "", "", "", false
	}
	if !exists(u.paths.Previous) {
		u.log(LevelError, "the retained previous binary is missing although its record is present",
			"path", u.paths.Previous)
		return "", "", "", false
	}
	return u.paths.Previous, rec.SHA256, rec.Version, true
}

func readPreviousRecord(path string) (*previousRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the record of the retained previous binary: %w", err)
	}
	var rec previousRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("the record of the retained previous binary at %s is corrupt: %w", path, err)
	}
	// A record that parses but names no version or no digest ({} or null
	// among them) describes nothing an update could be matched against, so it
	// is as corrupt as one that does not parse.
	if rec.Version == "" || rec.SHA256 == "" {
		return nil, fmt.Errorf("the record of the retained previous binary at %s is corrupt: it has no version or no sha256", path)
	}
	return &rec, nil
}

// writePreviousRecord atomically writes the record: temp file, fsync, rename,
// best-effort directory sync - the journal's discipline.
func (u *Updater) writePreviousRecord(version, sum string) error {
	data, err := json.MarshalIndent(previousRecord{Version: version, SHA256: sum, RetainedAt: time.Now()}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the record of the retained previous binary: %w", err)
	}
	dir := filepath.Dir(u.paths.PreviousRecord)
	tmp, err := os.CreateTemp(dir, u.n.tmpPrevious)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write the record of the retained previous binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("flush the record of the retained previous binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("finalize the record of the retained previous binary: %w", err)
	}
	if err := os.Rename(tmpPath, u.paths.PreviousRecord); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write the record of the retained previous binary: %w", err)
	}
	syncDir(dir)
	return nil
}

// retainPrevious keeps a committed update's rollback copy as the retained
// previous binary, and records what it is, from the journal's OldVersion and
// OldSHA256.
//
// It is idempotent and resumable, because the commit is not the only place it
// runs: a process can stop anywhere inside it, and Repair and CleanupReported
// run it again for as long as the committed journal exists. The order is what
// makes that safe. The old record is deleted BEFORE the binary it describes is
// replaced, and the new record written only AFTER its binary is in place, so no
// record ever describes a file other than the one beside it. The worst
// interrupted state is a retained binary with no record, which PreviousBinary
// reports as nothing and the next call finishes - after checking by content that
// it really is the journal's old binary.
func (u *Updater) retainPrevious(j *state) error {
	p := u.paths
	if exists(p.RollbackBinary) {
		if err := os.Remove(p.PreviousRecord); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the record of the binary being replaced: %w", err)
		}
		// An atomic rename-over: the older generation is replaced in one step.
		if err := os.Rename(p.RollbackBinary, p.Previous); err != nil {
			return fmt.Errorf("retain the previous binary: %w", err)
		}
		return u.writePreviousRecord(j.OldVersion, j.OldSHA256)
	}
	_, recErr := readPreviousRecord(p.PreviousRecord)
	if recErr == nil {
		return nil // retained and recorded already
	}
	if !exists(p.Previous) {
		return nil // nothing was kept for this update
	}
	sum, err := sha256File(p.Previous)
	if err != nil {
		return fmt.Errorf("hash the retained previous binary: %w", err)
	}
	if !strings.EqualFold(sum, j.OldSHA256) {
		// No record can describe it, so nothing can ever install it: it is only
		// disk. Deleted, and said so.
		//
		// A record that is present but unreadable goes first: left behind, it
		// would make PreviousBinary log ERROR on every call for good. First,
		// because the reverse order interrupted would leave that record with no
		// binary, which this function never revisits.
		if !errors.Is(recErr, fs.ErrNotExist) {
			if err := os.Remove(p.PreviousRecord); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove an unreadable record of the retained previous binary: %w", err)
			}
			u.log(LevelWarn, "deleted an unreadable record of the retained previous binary",
				"path", p.PreviousRecord, "err", recErr)
		}
		if err := os.Remove(p.Previous); err != nil {
			return fmt.Errorf("remove a retained binary no record describes: %w", err)
		}
		u.log(LevelWarn, "deleted a retained binary that no record describes", "path", p.Previous)
		return nil
	}
	return u.writePreviousRecord(j.OldVersion, j.OldSHA256)
}

// releaseRollbackCopy disposes of the rollback copy once an update is proven.
// Without RetainPrevious it is deleted, as it always was. With it, it becomes
// the retained previous binary - and a failure is logged, not returned: the
// update IS committed, and the journal stays. CleanupReported tries again, and
// so does Repair on every later start, which removes the journal itself once
// retention succeeds and the outcome is on record.
func (u *Updater) releaseRollbackCopy(j *state) {
	if !u.cfg.RetainPrevious {
		_ = os.Remove(u.paths.RollbackBinary)
		return
	}
	if err := u.retainPrevious(j); err != nil {
		u.log(LevelError, "could not retain the previous binary; it stays as the rollback copy and is retried",
			"err", err)
	}
}
