package denju

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// partialKey names a partial download after the request it belongs to, so the
// next attempt at the SAME request finds it and an attempt at any other request
// does not. It is a digest rather than the fields themselves because Request.ID
// is opaque and may hold characters no filename can.
//
// The digest is lowercased first because denju compares digests
// case-insensitively; Offset is not part of the identity.
func partialKey(req Request) string {
	h := sha256.New()
	_, _ = io.WriteString(h, req.ID)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, req.TargetVersion)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, strings.ToLower(req.SHA256))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// partialPath is where a ResumableSource's download for req is staged. It sits
// beside the binary for the same reason every staged download does: the swap
// that follows is a rename, atomic only within one filesystem.
func (u *Updater) partialPath(req Request) string {
	return u.binaryPath + u.n.prePartial + partialKey(req) + u.n.extPartial
}

// partialFiles lists every partial download beside the binary.
//
// It matches names by prefix and suffix rather than with filepath.Glob, because
// the binary's own name may contain glob metacharacters. What lies between them
// must be a key partialKey could have produced: a nested namespace ("app" and
// "app-download-x" are both valid) shares the prefix, and its partials are not
// this Updater's to delete.
func (u *Updater) partialFiles() ([]string, error) {
	dir := filepath.Dir(u.binaryPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(u.binaryPath) + u.n.prePartial
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || len(name) < len(prefix)+len(u.n.extPartial) ||
			!strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, u.n.extPartial) {
			continue
		}
		if isPartialKey(name[len(prefix) : len(name)-len(u.n.extPartial)]) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// isPartialKey reports whether s has the shape partialKey produces: sixteen
// lowercase hex digits.
func isPartialKey(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// discardOtherPartials deletes every partial download except keep ("" keeps
// none). One download is in flight at a time, so any other partial belongs to a
// request that has been superseded - another command, target or digest - and
// keeping it would only hold disk: an artifact can be hundreds of megabytes.
//
// Best-effort, like every other cleanup here, but never silent.
//
// The comparison is between CLEANED paths. partialPath concatenates onto
// BinaryPath exactly as the caller gave it, while partialFiles rebuilds each
// path with filepath.Join, which cleans; for a BinaryPath such as "dir//prog"
// or "dir/./prog" the raw strings differ and the request's own partial would
// be deleted as another request's on every attempt, so resume would never
// happen. keep == "" cleans to ".", which no listed file equals.
func (u *Updater) discardOtherPartials(keep string) {
	files, err := u.partialFiles()
	if err != nil {
		u.log(LevelWarn, "could not list partial downloads", "err", err)
		return
	}
	for _, f := range files {
		if filepath.Clean(f) == filepath.Clean(keep) {
			continue
		}
		if err := os.Remove(f); err != nil {
			u.log(LevelWarn, "could not discard a partial download for another request", "path", f, "err", err)
			continue
		}
		u.log(LevelInfo, "discarded a partial download for another request", "path", f)
	}
}

// discardStalePartials deletes every partial nothing has written to for longer
// than PartialRetention. Repair calls it only when no update is in flight: with
// a journal present, a completed partial may be the journal's staged binary.
func (u *Updater) discardStalePartials(now time.Time) {
	files, err := u.partialFiles()
	if err != nil {
		u.log(LevelWarn, "could not list partial downloads", "err", err)
		return
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || now.Sub(info.ModTime()) <= u.cfg.PartialRetention {
			continue
		}
		if err := os.Remove(f); err != nil {
			u.log(LevelWarn, "could not discard a stale partial download", "path", f, "err", err)
			continue
		}
		u.log(LevelInfo, "discarded a stale partial download", "path", f, "bytes", info.Size())
	}
}
