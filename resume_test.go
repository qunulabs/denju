package denju

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// resumingSource serves payload from req.Offset, as a server honouring a ranged
// read does. failAfter > 0 breaks the stream after that many bytes of the NEXT
// call only; reject makes it refuse the offset outright. Every offset it is
// asked for is kept, because the offset is the whole point.
type resumingSource struct {
	payload   []byte
	failAfter int
	reject    bool
	offsets   []int64
}

func (s *resumingSource) ResumesFromOffset() {}

func (s *resumingSource) Fetch(_ context.Context, req Request, w io.Writer) error {
	s.offsets = append(s.offsets, req.Offset)
	if s.reject || req.Offset > int64(len(s.payload)) {
		return fmt.Errorf("%w: the artifact changed", ErrResumeRejected)
	}
	rest := s.payload[req.Offset:]
	if s.failAfter > 0 && s.failAfter < len(rest) {
		n := s.failAfter
		s.failAfter = 0
		if _, err := w.Write(rest[:n]); err != nil {
			return err
		}
		return errors.New("the stream broke")
	}
	_, err := w.Write(rest)
	return err
}

// plainOffsetSource is an ordinary Source that records the offset it sees.
type plainOffsetSource struct {
	payload []byte
	offsets *[]int64
}

func (s plainOffsetSource) Fetch(_ context.Context, req Request, w io.Writer) error {
	*s.offsets = append(*s.offsets, req.Offset)
	_, err := w.Write(s.payload)
	return err
}

var resumePayload = []byte("0123456789abcdefghijklmnopqrstuvwxyz")

func resumeRequest() Request {
	return Request{ID: "cmd-7", TargetVersion: "1.5.0", SHA256: sha256Hex(resumePayload)}
}

func resumeTestUpdater(t *testing.T) (*Updater, *captureLog) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	writeBinary(t, bin, "current")
	log := &captureLog{}
	u, err := New(Config{Namespace: "app", Version: "1.4.0", BinaryPath: bin, Log: log.Logger()})
	noErr(t, err, "New")
	return u, log
}

func eqOffsets(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("offsets asked for = %v, want %v", got, want)
	}
}

// A broken stream keeps what arrived. Deleting it - v0.3.0's behaviour - is what
// made every retry start again from zero over a link that had just failed.
func TestDownloadResumable_FailedFetchKeepsThePartial(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	src := &resumingSource{payload: resumePayload, failAfter: 10}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrDownloadFailed), "a broken stream is a download failure")
	isFalse(t, errors.Is(err, ErrResumeRejected), "and not a rejected resume")
	hasSubstr(t, err.Error(), "kept for the next attempt", "the error says the bytes were kept")
	eq(t, readFileString(t, u.partialPath(req)), "0123456789", "the bytes received are kept")
}

// The next attempt at the SAME request asks for the bytes it lacks, and the
// prefix plus the remainder is the artifact.
func TestDownloadResumable_ResumesFromTheBytesOnDisk(t *testing.T) {
	u, log := resumeTestUpdater(t)
	req := resumeRequest()
	src := &resumingSource{payload: resumePayload, failAfter: 10}

	_, err := u.download(context.Background(), req, src)
	wantErr(t, err, "the first attempt breaks")

	staged, err := u.download(context.Background(), req, src)
	noErr(t, err, "the second attempt resumes")
	eq(t, staged, u.partialPath(req), "the completed partial is what gets staged")
	eq(t, readFileString(t, staged), string(resumePayload), "prefix and remainder form the artifact")
	eqOffsets(t, src.offsets, 0, 10)
	isTrue(t, log.contains("resuming a partial download"), "the resume is logged")
}

// Nothing already on disk is trusted on its own: the digest covers the prefix,
// and a partial that fails it is deleted so the next attempt starts clean.
func TestDownloadResumable_ACorruptPrefixFailsTheDigestAndIsDeleted(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, []byte("X123456789"), 0o600), "seed a corrupt prefix")
	src := &resumingSource{payload: resumePayload}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrResumedChecksumMismatch), "a corrupt prefix is a mismatch after resuming")
	isFalse(t, errors.Is(err, ErrChecksumMismatch), "and not proof that the artifact is wrong")
	hasSubstr(t, err.Error(), "after resuming from byte 10", "the error says the download resumed")
	hasSubstr(t, err.Error(), "; the partial download was discarded", "the error says the bytes are gone")
	eqOffsets(t, src.offsets, 10)
	isFalse(t, exists(path), "a partial that fails the digest is deleted")
}

// A download that started from byte zero and still fails the digest proves the
// artifact is wrong. Only a RESUMED mismatch gets the kind a caller retries.
func TestDownloadResumable_AMismatchFromByteZeroIsAChecksumMismatch(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	req.SHA256 = sha256Hex([]byte("some other artifact"))
	src := &resumingSource{payload: resumePayload}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrChecksumMismatch), "a mismatch from byte zero is a checksum mismatch")
	isFalse(t, errors.Is(err, ErrResumedChecksumMismatch), "and not a resumed one")
	eqOffsets(t, src.offsets, 0)
	isFalse(t, exists(u.partialPath(req)), "a partial that fails the digest is deleted")
}

// errNoSpace stands in for a full disk.
var errNoSpace = errors.New("no space left on device")

// refusingWriter refuses every write, as a file on a full disk does.
type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, errNoSpace }

// withRefusedStagingWrites makes every write to a staging file fail.
func withRefusedStagingWrites(t *testing.T) {
	t.Helper()
	prev := stagingWriter
	stagingWriter = func(*os.File) io.Writer { return refusingWriter{} }
	t.Cleanup(func() { stagingWriter = prev })
}

// A failure to WRITE what arrived is not a broken transfer. Keeping the partial
// would hold the disk that just ran out for up to PartialRetention, and every
// retry would fail at the same byte, so it is deleted - as v0.3.0 deleted its
// temp file - and the failure carries no download kind that invites a retry.
func TestDownloadResumable_ALocalWriteFailureDiscardsThePartial(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
	withRefusedStagingWrites(t)
	src := &resumingSource{payload: resumePayload}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, errNoSpace), "the write failure is the cause")
	isFalse(t, errors.Is(err, ErrDownloadFailed), "a local write failure is not a transfer worth resuming")
	isFalse(t, errors.Is(err, ErrResumeRejected), "nor a rejected resume")
	isFalse(t, errors.Is(err, ErrChecksumMismatch), "nor a checksum mismatch")
	isFalse(t, strings.Contains(err.Error(), "kept for the next attempt"), "the error must not say the bytes were kept")
	hasSubstr(t, err.Error(), "; the partial download was discarded", "the error says the bytes are gone")
	isFalse(t, exists(path), "the partial is deleted")
}

// The same holds for a plain Source, whose message stays exactly v0.3.0's.
func TestDownload_ALocalWriteFailureCarriesNoDownloadKind(t *testing.T) {
	u := downloadTestUpdater(t)
	withRefusedStagingWrites(t)
	src := &fakeSource{payload: []byte("new bytes")}

	_, err := u.download(context.Background(), Request{SHA256: sha256Hex([]byte("new bytes"))}, src)
	isTrue(t, errors.Is(err, errNoSpace), "the write failure is the cause")
	isFalse(t, errors.Is(err, ErrDownloadFailed), "a local write failure is not a transfer failure")
	eq(t, err.Error(), "download the update: no space left on device", "the plain-source message is unchanged")

	entries, err := os.ReadDir(filepath.Dir(u.binaryPath))
	noErr(t, err, "read the install directory")
	eq(t, len(entries), 1, "only the binary should remain")
}

// withFailingPartialDeletes makes every delete of a failed download's partial
// fail, as another handle holding the file does on Windows.
func withFailingPartialDeletes(t *testing.T) error {
	t.Helper()
	errHeld := errors.New("the file is in use by another process")
	prev := removePartial
	removePartial = func(string) error { return errHeld }
	t.Cleanup(func() { removePartial = prev })
	return errHeld
}

// A partial whose name is fixed per request and that survives a failed delete
// is found again by the next attempt, so an error claiming it was discarded
// sends an operator the wrong way. The failure is logged at ERROR and the error
// says what really happened.
func TestDownloadResumable_AFailedDeleteIsLoggedAndNotClaimed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, src *resumingSource)
		kind    error // nil = no download kind
	}{
		{
			name:    "a rejected offset",
			arrange: func(_ *testing.T, src *resumingSource) { src.reject = true },
			kind:    ErrResumeRejected,
		},
		{
			name:    "a resumed checksum mismatch",
			arrange: func(_ *testing.T, src *resumingSource) { src.payload = []byte("0123456789 is not the artifact") },
			kind:    ErrResumedChecksumMismatch,
		},
		{
			name:    "a local write failure",
			arrange: func(t *testing.T, _ *resumingSource) { withRefusedStagingWrites(t) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, log := resumeTestUpdater(t)
			req := resumeRequest()
			path := u.partialPath(req)
			noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
			src := &resumingSource{payload: resumePayload}
			tc.arrange(t, src)
			errHeld := withFailingPartialDeletes(t)

			_, err := u.download(context.Background(), req, src)
			wantErr(t, err, "download")
			if tc.kind != nil {
				isTrue(t, errors.Is(err, tc.kind), "the failure keeps its kind")
			}
			isFalse(t, strings.Contains(err.Error(), "discarded"), "the error must not claim the partial was discarded")
			hasSubstr(t, err.Error(), "the partial download could not be deleted: "+errHeld.Error(), "the error says why")
			isTrue(t, log.contains("error self-update: could not delete the partial download"), "logged at ERROR")
			isTrue(t, exists(path), "the partial is still there")
		})
	}
}

// A source that cannot continue from the offset is a distinct, visible outcome:
// the partial goes, the attempt fails, and the NEXT attempt starts from zero.
// denju does not quietly restart inside the same attempt.
func TestDownloadResumable_RejectedOffsetDiscardsThePartial(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
	src := &resumingSource{payload: resumePayload, reject: true}

	_, err := u.download(context.Background(), req, src)
	isTrue(t, errors.Is(err, ErrResumeRejected), "a refused offset is a rejected resume")
	isFalse(t, errors.Is(err, ErrDownloadFailed), "and not an ordinary transfer failure")
	hasSubstr(t, err.Error(), "byte 10", "the error names the offset that was refused")
	isFalse(t, exists(path), "the partial is discarded")
	eqOffsets(t, src.offsets, 10)

	src.reject = false
	staged, err := u.download(context.Background(), req, src)
	noErr(t, err, "the next attempt")
	eq(t, readFileString(t, staged), string(resumePayload), "the next attempt fetched everything")
	eqOffsets(t, src.offsets, 10, 0)
}

func TestDownloadResumable_PartialRetention(t *testing.T) {
	for _, tc := range []struct {
		name     string
		age      time.Duration
		want     []int64
		wantWarn bool
	}{
		{name: "a fresh partial is resumed", age: 30 * time.Minute, want: []int64{10}},
		{name: "a stale partial is discarded", age: 2 * time.Hour, want: []int64{0}, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, log := resumeTestUpdater(t)
			u.cfg.PartialRetention = time.Hour
			req := resumeRequest()
			path := u.partialPath(req)
			noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
			backdate(t, path, tc.age)
			src := &resumingSource{payload: resumePayload}

			staged, err := u.download(context.Background(), req, src)
			noErr(t, err, "download")
			eq(t, readFileString(t, staged), string(resumePayload), "the artifact")
			eqOffsets(t, src.offsets, tc.want...)
			eq(t, log.contains("discarded a stale partial download"), tc.wantWarn, "stale discard logged")
		})
	}
}

// One download is in flight at a time, so a partial for any other request is
// superseded and only holds disk.
func TestDownloadResumable_DiscardsPartialsOfOtherRequests(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	other := u.partialPath(Request{ID: "cmd-6", TargetVersion: req.TargetVersion, SHA256: req.SHA256})
	noErr(t, os.WriteFile(other, resumePayload[:10], 0o600), "seed another request's partial")
	src := &resumingSource{payload: resumePayload}

	_, err := u.download(context.Background(), req, src)
	noErr(t, err, "download")
	isFalse(t, exists(other), "the superseded partial is discarded")
	eqOffsets(t, src.offsets, 0)
}

// An ordinary Source behaves exactly as in v0.3.0: offset zero, a random temp
// file, and no partial left behind - including one some earlier resumable
// attempt at the same request left.
func TestDownload_PlainSourceIgnoresPartials(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, resumePayload[:10], 0o600), "seed a partial")
	var offsets []int64

	staged, err := u.download(context.Background(), req, plainOffsetSource{payload: resumePayload, offsets: &offsets})
	noErr(t, err, "download")
	isTrue(t, strings.HasPrefix(filepath.Base(staged), ".app-update-"), "a plain source stages exactly as before")
	isFalse(t, exists(path), "a plain source leaves no partial behind")
	eqOffsets(t, offsets, 0)
}

// A BinaryPath nobody cleaned - a doubled separator, a "." element - names the
// same file. The request's own partial must still be recognised as its own, or
// every attempt deletes it as "another request's" and resume never happens.
func TestDownloadResumable_UncleanBinaryPathStillResumes(t *testing.T) {
	dir := t.TempDir()
	sep := string(filepath.Separator)
	bin := dir + sep + "." + sep + sep + "prog"
	writeBinary(t, bin, "current")
	log := &captureLog{}
	u, err := New(Config{Namespace: "app", Version: "1.4.0", BinaryPath: bin, Log: log.Logger()})
	noErr(t, err, "New")
	req := resumeRequest()
	src := &resumingSource{payload: resumePayload, failAfter: 10}

	_, err = u.download(context.Background(), req, src)
	wantErr(t, err, "the first attempt breaks")

	staged, err := u.download(context.Background(), req, src)
	noErr(t, err, "the second attempt resumes")
	eq(t, readFileString(t, staged), string(resumePayload), "prefix and remainder form the artifact")
	eqOffsets(t, src.offsets, 0, 10)
	isFalse(t, log.contains("discarded a partial download for another request"), "its own partial is not another request's")
}

// A partial that already holds every byte (spec D section 9 item 8c: offset ==
// total size) is asked for from its end, fetches nothing, and still has to pass
// the digest over the whole file before it is staged.
func TestDownloadResumable_ACompletePartialFetchesNothing(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	req := resumeRequest()
	path := u.partialPath(req)
	noErr(t, os.WriteFile(path, resumePayload, 0o600), "seed a complete partial")
	src := &resumingSource{payload: resumePayload}

	staged, err := u.download(context.Background(), req, src)
	noErr(t, err, "download")
	eq(t, staged, path, "the complete partial is what gets staged")
	eq(t, readFileString(t, staged), string(resumePayload), "nothing was appended")
	eqOffsets(t, src.offsets, int64(len(resumePayload)))
}

// Only a file shaped exactly like a partial of THIS namespace is one. A nested
// namespace ("app" and "app-download-x" are both valid) shares the prefix, and
// its partials are not this Updater's to delete; nor is a file without the
// suffix, or a directory.
func TestPartialFiles_MatchesOnlyThisNamespacesPartials(t *testing.T) {
	u, _ := resumeTestUpdater(t)
	key := partialKey(Request{ID: "cmd-6"})
	own := u.partialPath(Request{ID: "cmd-6"})
	noErr(t, os.WriteFile(own, []byte("x"), 0o600), "seed this namespace's partial")
	nested := u.binaryPath + ".app-download-x-download-" + key + ".partial"
	noErr(t, os.WriteFile(nested, []byte("x"), 0o600), "seed a nested namespace's partial")
	noSuffix := u.binaryPath + ".app-download-" + key
	noErr(t, os.WriteFile(noSuffix, []byte("x"), 0o600), "seed a file without the suffix")
	notAKey := u.binaryPath + ".app-download-notahexkey.partial"
	noErr(t, os.WriteFile(notAKey, []byte("x"), 0o600), "seed a file without a key")
	shortKey := u.binaryPath + ".app-download-abcdef.partial"
	noErr(t, os.WriteFile(shortKey, []byte("x"), 0o600), "seed a file with a hex key of the wrong length")
	otherSuffix := u.binaryPath + ".app-download-" + key + ".notpart"
	noErr(t, os.WriteFile(otherSuffix, []byte("x"), 0o600), "seed a file with a key and another suffix")
	notHex := u.binaryPath + ".app-download-zzzzzzzzzzzzzzzz.partial"
	noErr(t, os.WriteFile(notHex, []byte("x"), 0o600), "seed a file with a sixteen-character key that is not hex")
	dir := u.binaryPath + ".app-download-" + partialKey(Request{ID: "cmd-5"}) + ".partial"
	noErr(t, os.Mkdir(dir, 0o755), "seed a directory")

	got, err := u.partialFiles()
	noErr(t, err, "partialFiles")
	if !slices.Equal(got, []string{own}) {
		t.Errorf("partialFiles = %v, want only %v", got, own)
	}
}
