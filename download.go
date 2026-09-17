package denju

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// keptSuffix is appended to a failed resumable download's error, so the log
// line says the bytes survived.
const keptSuffix = "; the partial download is kept for the next attempt"

// discardedSuffix is appended when a resumable download's partial is deleted
// on failure, so the log line says the next attempt starts from byte zero. A
// plain Source's temp file is never announced, exactly as in v0.3.0.
const discardedSuffix = "; the partial download was discarded"

// stagingWriter is what a download writes the arrived bytes through. A package
// var so a test can make those writes fail: a full disk cannot be produced
// portably, and the paths that handle one are the ones worth proving.
var stagingWriter = func(f *os.File) io.Writer { return f }

// removePartial deletes a failed resumable download's partial. A package var so
// a test can make the delete fail, as another handle holding the file does on
// Windows.
var removePartial = os.Remove

// download stages the new binary next to the one it will replace and verifies
// its digest, returning the staged path.
//
// The staged file is created in the TARGET BINARY'S directory, not in the
// system temp directory, for two reasons: the swap that follows is an
// os.Rename, which is only atomic within one filesystem, and staging elsewhere
// would silently turn the swap into a copy across a mount boundary. It also
// means a download too large for the destination fails here, while the running
// program is still untouched, rather than halfway through the swap.
//
// A ResumableSource stages into a partial named after the request, kept across
// a failed transfer (downloadResumable). Any other Source stages into a fresh
// temp file deleted on any failure, exactly as before resuming existed
// (downloadFresh). Either way every partial left by a DIFFERENT request is
// deleted first. On success the caller owns the staged file.
func (u *Updater) download(ctx context.Context, req Request, src Source) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.DownloadTimeout)
	defer cancel()

	if rs, ok := src.(ResumableSource); ok {
		u.discardOtherPartials(u.partialPath(req))
		return u.downloadResumable(ctx, req, rs)
	}
	u.discardOtherPartials("")
	return u.downloadFresh(ctx, req, src)
}

// downloadFresh is the v0.3.0 path: a random temp file, deleted on any failure.
func (u *Updater) downloadFresh(ctx context.Context, req Request, src Source) (string, error) {
	dir := filepath.Dir(u.binaryPath)
	tmp, err := os.CreateTemp(dir, u.n.tmpDownload)
	if err != nil {
		return "", fmt.Errorf("create a staging file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	h := sha256.New()
	w := &localWriter{w: stagingWriter(tmp)}
	if err := src.Fetch(ctx, req, io.MultiWriter(w, h)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		if w.err != nil {
			// The same wording as v0.3.0's for a failed Fetch, but with no kind,
			// because a full disk is not a transfer worth retrying. It differs
			// from v0.3.0 in one case: v0.3.0 appended "(after <timeout>)" when
			// the download deadline had also passed, and this does not.
			return "", fmt.Errorf("download the update: %w", err)
		}
		return "", u.fetchFailure(ctx, err, "")
	}
	// fsync before the digest is trusted: the bytes have to be on the device,
	// not merely in the page cache, because the process that verified them is
	// about to be replaced by one that only sees what survived.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("flush the downloaded update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("finalize the downloaded update: %w", err)
	}
	if err := checkDigest(h, req.SHA256); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return tmpPath, nil
}

// downloadResumable stages the artifact in a partial named after the request,
// resuming from whatever an earlier attempt at the same request left behind.
//
// The prefix already on disk is re-hashed before a single new byte is
// written, so the digest check at the end covers every byte of the file, old
// and new alike. Nothing written earlier is trusted on its own.
//
// What happens to the partial is the contract a caller retries against:
//   - The transfer fails (Fetch returns an error that is not a local write
//     failure): it is flushed and KEPT, and the next attempt resumes.
//   - Writing what arrived fails (a full disk, a file-size limit): it is
//     deleted, as v0.3.0 deleted its temp file, and the failure carries no
//     download kind. Keeping it would hold the disk that just ran out, and every
//     retry would fail at the same byte.
//   - The source rejects the offset (ErrResumeRejected): it is deleted and the
//     attempt fails; the next attempt starts from byte zero.
//   - The complete file fails the digest: it is deleted and the attempt fails;
//     the next attempt starts from byte zero. A download that resumed from a
//     non-zero offset fails with ErrResumedChecksumMismatch rather than
//     ErrChecksumMismatch, because the kept bytes, not the artifact, may be what
//     is wrong.
//   - Flushing or closing it after a successful Fetch fails: it is deleted.
//   - Nothing has written to it for PartialRetention: it is deleted before
//     this attempt, which starts from byte zero.
//
// Each deletion that fails is logged at ERROR, and the error says the partial
// could not be deleted rather than that it was discarded (discardPartial).
func (u *Updater) downloadResumable(ctx context.Context, req Request, src ResumableSource) (string, error) {
	path := u.partialPath(req)

	if info, err := os.Stat(path); err == nil {
		if age := time.Since(info.ModTime()); age > u.cfg.PartialRetention {
			if err := os.Remove(path); err != nil {
				return "", fmt.Errorf("discard a stale partial download: %w", err)
			}
			u.log(LevelWarn, "discarded a stale partial download; starting from the beginning",
				"bytes", info.Size(), "age", age.Round(time.Second), "id", req.ID)
		}
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("open a partial download in %s: %w", filepath.Dir(path), err)
	}
	h := sha256.New()
	// Reading to the end both replays the hash and leaves the file position at
	// the end, so the bytes Fetch writes next are appended.
	offset, err := io.Copy(h, f)
	if err != nil {
		_ = f.Close()
		return "", fmt.Errorf("read back the partial download: %w", err)
	}
	if offset > 0 {
		u.log(LevelInfo, "resuming a partial download", "offset", offset, "id", req.ID)
	}

	req.Offset = offset
	w := &localWriter{w: stagingWriter(f)}
	if err := src.Fetch(ctx, req, io.MultiWriter(w, h)); err != nil {
		if w.err != nil {
			// Checked first: whatever the source made of it, the bytes could not
			// be stored, and that is the failure to report.
			_ = f.Close()
			return "", fmt.Errorf("write the downloaded update: %w%s", w.err, u.discardPartial(path))
		}
		if errors.Is(err, ErrResumeRejected) {
			_ = f.Close()
			return "", withKind(ErrResumeRejected, fmt.Errorf(
				"download the update: the source refused to resume from byte %d: %w%s", offset, err, u.discardPartial(path)))
		}
		// Flushed so what is kept is really on the device. If a power cut loses
		// part of it anyway, the digest check at the end of a later attempt still
		// refuses the result - the error is deliberately not escalated here.
		_ = f.Sync()
		_ = f.Close()
		return "", u.fetchFailure(ctx, err, keptSuffix)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("flush the downloaded update: %w%s", err, u.discardPartial(path))
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("finalize the downloaded update: %w%s", err, u.discardPartial(path))
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, req.SHA256) {
		if offset > 0 {
			return "", withKind(ErrResumedChecksumMismatch, fmt.Errorf(
				"the downloaded update does not match its checksum after resuming from byte %d: expected %s, got %s%s",
				offset, req.SHA256, got, u.discardPartial(path)))
		}
		return "", withKind(ErrChecksumMismatch, fmt.Errorf(
			"the downloaded update does not match its checksum: expected %s, got %s%s", req.SHA256, got, u.discardPartial(path)))
	}
	return path, nil
}

// discardPartial deletes a failed resumable download's partial and returns the
// suffix its error ends with.
//
// The partial's name is fixed per request, so one that survives a failed delete
// is found again by the next attempt: a rejected offset is asked for again, and
// a bad partial fails the digest again. An error saying it was discarded would
// send an operator the wrong way, so a failed delete is logged at ERROR and the
// suffix says what really happened. There is no retry: on Windows the usual
// cause is another handle on the file, which clears when that handle closes.
func (u *Updater) discardPartial(path string) string {
	if err := removePartial(path); err != nil {
		u.log(LevelError, "could not delete the partial download; the next attempt at this request will find it again",
			"path", path, "err", err)
		return "; the partial download could not be deleted: " + err.Error()
	}
	return discardedSuffix
}

// localWriter remembers the first error writing to the staging file.
//
// A write error reaches denju only through the source's Fetch, which may wrap
// it, word it as its own, or return something else entirely. Recording it here
// is what lets a full disk be told apart from a broken transfer, whatever the
// source did with it.
type localWriter struct {
	w   io.Writer
	err error
}

func (l *localWriter) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if err != nil && l.err == nil {
		l.err = err
	}
	return n, err
}

// fetchFailure words a failed transfer, tagged ErrDownloadFailed. suffix is
// appended when bytes survive for the next attempt; for a plain Source it is
// empty and the text is exactly v0.3.0's. A local write failure never comes
// here: it is not a transfer failure and carries no kind.
func (u *Updater) fetchFailure(ctx context.Context, err error, suffix string) error {
	if ctx.Err() != nil {
		return withKind(ErrDownloadFailed, fmt.Errorf("download the update: %w (after %s)%s", err, u.cfg.DownloadTimeout, suffix))
	}
	return withKind(ErrDownloadFailed, fmt.Errorf("download the update: %w%s", err, suffix))
}

// checkDigest compares the running hash with the promised digest, tagged
// ErrChecksumMismatch when they differ. It serves the plain Source, whose text
// is exactly v0.3.0's; a resumable download words its own mismatch, because
// deleting its partial can fail and a resumed mismatch has its own kind.
func checkDigest(h hash.Hash, want string) error {
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return withKind(ErrChecksumMismatch, fmt.Errorf("the downloaded update does not match its checksum: expected %s, got %s", want, got))
	}
	return nil
}
