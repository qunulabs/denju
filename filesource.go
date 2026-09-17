package denju

import (
	"context"
	"fmt"
	"io"
	"os"
)

// FileSource returns a [ResumableSource] that reads the artifact from a file on
// the local filesystem.
//
// It exists for installing the binary kept by [Config.RetainPrevious]: pass it
// the path [Updater.PreviousBinary] reports, and moving back to that version
// reads no byte from any network. denju verifies what it reads against
// [Request.SHA256] exactly as for any other source, so pointing it at the wrong
// file fails the digest check rather than installing anything.
//
// An offset past the end of the file is rejected with [ErrResumeRejected]; an
// offset exactly at the end writes nothing and succeeds. Anything but a regular
// file is refused: a device such as /dev/zero would never end.
func FileSource(path string) ResumableSource { return localFile{path: path} }

type localFile struct{ path string }

func (localFile) ResumesFromOffset() {}

func (s localFile) Fetch(ctx context.Context, req Request, w io.Writer) error {
	f, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", s.path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", s.path)
	}
	if req.Offset > info.Size() {
		return fmt.Errorf("%w: %s is %d bytes, fewer than the %d already downloaded",
			ErrResumeRejected, s.path, info.Size(), req.Offset)
	}
	if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s to byte %d: %w", s.path, req.Offset, err)
	}
	// "copy", not "read": the failure may be on either side, and a full disk is
	// not the source file's fault.
	if _, err := io.Copy(w, ctxReader{ctx: ctx, r: f}); err != nil {
		return fmt.Errorf("copy %s: %w", s.path, err)
	}
	return nil
}

// ctxReader stops a local copy when its context ends. A file read does not
// block the way a network read does, so checking between reads is enough to
// honour DownloadTimeout and a caller shutting down.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
