package denju

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A compile-time proof: the retained-binary source must resume like any other.
var _ ResumableSource = FileSource("")

func writeSourceFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact")
	noErr(t, os.WriteFile(path, []byte(content), 0o600), "write the source file")
	return path
}

func TestFileSource_WritesTheWholeFileFromZero(t *testing.T) {
	path := writeSourceFile(t, "0123456789")
	var buf bytes.Buffer

	noErr(t, FileSource(path).Fetch(context.Background(), Request{}, &buf), "Fetch")
	eq(t, buf.String(), "0123456789", "the whole file")
}

func TestFileSource_WritesFromTheOffset(t *testing.T) {
	path := writeSourceFile(t, "0123456789")

	for _, tc := range []struct {
		offset int64
		want   string
	}{
		{offset: 4, want: "456789"},
		{offset: 10, want: ""}, // exactly complete: nothing left to send, and not an error
	} {
		var buf bytes.Buffer
		noErr(t, FileSource(path).Fetch(context.Background(), Request{Offset: tc.offset}, &buf), "Fetch")
		eq(t, buf.String(), tc.want, "bytes from the offset")
	}
}

func TestFileSource_OffsetPastTheEndIsRejected(t *testing.T) {
	path := writeSourceFile(t, "0123456789")
	var buf bytes.Buffer

	err := FileSource(path).Fetch(context.Background(), Request{Offset: 11}, &buf)
	isTrue(t, errors.Is(err, ErrResumeRejected), "an offset past the end cannot be resumed")
	eq(t, buf.Len(), 0, "nothing is written")
}

// A missing file is an ordinary failure, not a rejected resume: it says nothing
// about whether the bytes already downloaded are good.
func TestFileSource_MissingFileIsNotARejectedResume(t *testing.T) {
	var buf bytes.Buffer
	err := FileSource(filepath.Join(t.TempDir(), "gone")).Fetch(context.Background(), Request{}, &buf)
	isTrue(t, errors.Is(err, fs.ErrNotExist), "the cause is reachable")
	isFalse(t, errors.Is(err, ErrResumeRejected), "and it is not a rejected resume")
}

func TestFileSource_HonoursCancellation(t *testing.T) {
	path := writeSourceFile(t, "0123456789")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer

	err := FileSource(path).Fetch(ctx, Request{}, &buf)
	isTrue(t, errors.Is(err, context.Canceled), "a cancelled copy stops")
}

// Through the Updater, a FileSource is staged and verified like any source: a
// file that is not the promised artifact fails the digest.
func TestFileSource_IsVerifiedLikeAnyOtherSource(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	path := writeSourceFile(t, "not the artifact")

	_, err := u.download(context.Background(), resumeRequest(), FileSource(path))
	isTrue(t, errors.Is(err, ErrChecksumMismatch), "the wrong file is refused by the digest")
}

// Only a regular file is an artifact. A device such as /dev/zero would fill the
// partial until the download timed out, and a directory would fail as a
// retryable-looking read error.
func TestFileSource_RefusesAnythingButARegularFile(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()

	err := FileSource(dir).Fetch(context.Background(), Request{}, &buf)
	wantErrContaining(t, err, "is not a regular file", "Fetch of a directory")
	eq(t, buf.Len(), 0, "nothing is written")
}

// A copy can fail on either side. Wording every failure as a read of the source
// file blames the file for a full disk.
func TestFileSource_DoesNotBlameTheFileForAFailedWrite(t *testing.T) {
	path := writeSourceFile(t, "0123456789")

	err := FileSource(path).Fetch(context.Background(), Request{}, refusingWriter{})
	isTrue(t, errors.Is(err, errNoSpace), "the write failure is reachable")
	isFalse(t, strings.HasPrefix(err.Error(), "read "), "the error must not say the source file could not be read")
}
