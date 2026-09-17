package denju

import "errors"

// Failure kinds a caller may need to tell apart. Match them with errors.Is
// against [Result.Cause]. The human-readable text is unaffected and is still in
// [Result.Error].
//
// Where a kind below says the bytes on disk have been deleted and the delete
// itself fails, that failure is logged at ERROR and the error text says the
// partial download could not be deleted.
var (
	// ErrDownloadFailed means the transfer failed: [Source.Fetch] returned an
	// error, or overran [Config.DownloadTimeout]. With a [ResumableSource] the
	// bytes already received are kept, and the next Update for the same request
	// resumes from them; with any other Source nothing is kept.
	//
	// A failure to WRITE what arrived - a full disk, a file-size limit - is not
	// a transfer failure and does not carry this kind, or any other: the staged
	// bytes are deleted whatever the source, and a retry would fail at the same
	// point until the disk has room.
	ErrDownloadFailed = errors.New("denju: the download failed")

	// ErrChecksumMismatch means a complete download that started from byte zero
	// did not hash to [Request.SHA256], so the artifact is not the one promised.
	// Everything written for it has been deleted. There is no partial-trust
	// path. A download that resumed fails with [ErrResumedChecksumMismatch]
	// instead.
	ErrChecksumMismatch = errors.New("denju: the download does not match its checksum")

	// ErrResumedChecksumMismatch means a download that RESUMED from bytes an
	// earlier attempt left on disk did not hash to [Request.SHA256]. The partial
	// has been deleted, so the next attempt starts from byte zero.
	//
	// It is not [ErrChecksumMismatch], and errors.Is does not match the two,
	// because it does not show that the artifact is wrong. The kept bytes may
	// have been damaged on the device - a power cut during the earlier attempt -
	// or the source may not have honoured [Request.Offset]. Retry once from zero:
	// a mismatch on that attempt is ErrChecksumMismatch, and permanent.
	ErrResumedChecksumMismatch = errors.New("denju: the resumed download does not match its checksum")

	// ErrResumeRejected is returned BY a [ResumableSource], wrapped, when it
	// cannot continue from [Request.Offset]: the artifact behind the request is
	// shorter than the offset, or is no longer the artifact the earlier bytes
	// came from. denju then deletes the partial and fails the attempt with this
	// kind; the next attempt starts from byte zero. denju never restarts a
	// transfer on its own inside one attempt.
	ErrResumeRejected = errors.New("denju: the source cannot resume from this offset")
)

// kindError attaches a failure kind to an error WITHOUT changing its text, so
// Result.Error reads exactly as it did before kinds existed - consumers log and
// report that text, and some match on it.
type kindError struct {
	kind error
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }

// Unwrap exposes both the kind and the underlying error, so errors.Is matches
// either.
func (e *kindError) Unwrap() []error { return []error{e.kind, e.err} }

// withKind tags err with kind.
func withKind(kind, err error) error { return &kindError{kind: kind, err: err} }
