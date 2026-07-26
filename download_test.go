package denju

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slowSource blocks until its context is done, to exercise the download bound.
type slowSource struct{}

func (slowSource) Fetch(ctx context.Context, _ Request, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}

func downloadTestUpdater(t *testing.T) *Updater {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "current")
	u, err := New(Config{Namespace: "app", Version: "1.4.0", BinaryPath: bin})
	noErr(t, err, "New")
	return u
}

// The staged file must land in the TARGET BINARY'S directory. os.Rename is only
// atomic within one filesystem, so staging in the system temp directory would
// silently turn the swap into a cross-device copy - and would let a download too
// large for the destination fail halfway through the swap instead of before it.
func TestDownload_StagesBesideTheTargetBinary(t *testing.T) {
	u := downloadTestUpdater(t)
	payload := []byte("new bytes")
	src := &fakeSource{payload: payload}

	staged, err := u.download(context.Background(), Request{SHA256: sha256Hex(payload)}, src)
	noErr(t, err, "download")

	eq(t, filepath.Dir(staged), filepath.Dir(u.binaryPath), "the staged file sits beside the binary")
	isTrue(t, strings.HasPrefix(filepath.Base(staged), ".app-update-"),
		"the staged file carries the namespaced temp prefix")
	eq(t, readFileString(t, staged), string(payload), "the staged content")
}

func TestDownload_ChecksumMismatchRemovesTheStagedFile(t *testing.T) {
	u := downloadTestUpdater(t)
	src := &fakeSource{payload: []byte("new bytes")}

	_, err := u.download(context.Background(), Request{SHA256: sha256Hex([]byte("different"))}, src)
	wantErrContaining(t, err, "checksum", "download with a bad digest")

	entries, err := os.ReadDir(filepath.Dir(u.binaryPath))
	noErr(t, err, "read the install directory")
	eq(t, len(entries), 1, "only the binary should remain")
}

func TestDownload_SourceErrorRemovesTheStagedFile(t *testing.T) {
	u := downloadTestUpdater(t)
	src := &fakeSource{err: errors.New("the stream broke")}

	_, err := u.download(context.Background(), Request{SHA256: "irrelevant"}, src)
	wantErrContaining(t, err, "the stream broke", "download with a failing source")

	entries, err := os.ReadDir(filepath.Dir(u.binaryPath))
	noErr(t, err, "read the install directory")
	eq(t, len(entries), 1, "only the binary should remain")
}

// A source that never finishes must not hold an update open indefinitely.
func TestDownload_TimesOut(t *testing.T) {
	u := downloadTestUpdater(t)
	u.cfg.DownloadTimeout = 30 * time.Millisecond

	_, err := u.download(context.Background(), Request{SHA256: "irrelevant"}, slowSource{})
	wantErrContaining(t, err, "download the update", "download that overruns")

	entries, err := os.ReadDir(filepath.Dir(u.binaryPath))
	noErr(t, err, "read the install directory")
	eq(t, len(entries), 1, "only the binary should remain")
}

// The caller's context must be able to abort a download - a program shutting
// down should not be held up by a transfer it no longer needs.
func TestDownload_HonoursTheCallerContext(t *testing.T) {
	u := downloadTestUpdater(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := u.download(ctx, Request{SHA256: "irrelevant"}, slowSource{})
	wantErr(t, err, "download with a cancelled context")
}

// The Source sees a plain io.Writer and nothing else: denju owns the file, the
// hashing, the permissions and the cleanup.
func TestDownload_SourceOnlySeesAWriter(t *testing.T) {
	u := downloadTestUpdater(t)
	payload := []byte("chunk-one chunk-two chunk-three")

	var writes int
	src := writerFuncSource(func(w io.Writer) error {
		for _, part := range strings.Fields(string(payload)) {
			writes++
			if _, err := io.WriteString(w, part); err != nil {
				return err
			}
			if writes < 3 {
				if _, err := io.WriteString(w, " "); err != nil {
					return err
				}
			}
		}
		return nil
	})

	staged, err := u.download(context.Background(), Request{SHA256: sha256Hex(payload)}, src)
	noErr(t, err, "download from a chunked source")
	eq(t, writes, 3, "every chunk was written")
	eq(t, readFileString(t, staged), string(payload), "chunks are concatenated verbatim")
}

// writerFuncSource adapts a plain function into a Source.
type writerFuncSource func(io.Writer) error

func (f writerFuncSource) Fetch(_ context.Context, _ Request, w io.Writer) error { return f(w) }
