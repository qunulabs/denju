package denju

import (
	"context"
	"io"
)

// Request describes one update to apply.
//
// denju does not care where it came from. Fill it in from a control-plane
// message, a manifest, a release feed, a command-line flag - whatever the
// program already trusts.
type Request struct {
	// ID is an opaque identifier echoed back in the [Outcome], so a report can
	// be tied to whatever asked for the update. It may be empty, in which case
	// no outcome is recorded: with nothing to tie a report to there is nothing
	// to report, and no cooldown to hold.
	ID string

	// TargetVersion is the version being moved to. It is compared against
	// [Config.Version] to refuse a pointless update, and recorded in the journal
	// so startup repair can tell the new image from the old one.
	//
	// A version lower than the current one is not an error. Deliberately moving
	// a program backwards is a first-class operation.
	TargetVersion string

	// SHA256 is the hex-encoded SHA-256 of the binary the [Source] will produce.
	// denju hashes what it writes and refuses anything that does not match.
	//
	// This is the ONLY integrity check denju performs. It proves the bytes
	// arrived intact; it proves nothing about where they came from. Establishing
	// that this digest is authentic is the caller's responsibility.
	SHA256 string

	// TargetOS and TargetArch, when set, must equal runtime.GOOS and
	// runtime.GOARCH or the update is refused. Use the Go spellings: "linux",
	// "windows", "darwin", "amd64", "arm64".
	//
	// Leaving them empty skips the check. Setting them is strongly recommended:
	// installing a binary for the wrong platform is not recoverable in place,
	// and it is a mistake a control plane makes exactly once, on every host at
	// the same time.
	TargetOS   string
	TargetArch string

	// Offset is how many bytes of the artifact denju already holds on disk
	// from an earlier attempt at this same request. denju sets it; a caller
	// must leave it zero, and [Updater.Update] refuses a request that arrives
	// with it set.
	//
	// Only a [ResumableSource] ever sees a non-zero Offset. Any other Source
	// always sees zero.
	Offset int64
}

// Result is what [Updater.Update] concluded WHEN IT RETURNS.
//
// A successful update never returns - the process image is replaced on Unix, or
// the process exits so a helper can take over on Windows - so a returned Result
// always describes an update that did not happen. The program is still running
// the version it started with.
type Result struct {
	Status Status
	Error  string

	// Cause is the error behind Error, for errors.Is. It is nil exactly when
	// Error is empty. A failed transfer or a failed digest carries a kind -
	// [ErrDownloadFailed], [ErrResumeRejected], [ErrResumedChecksumMismatch] or
	// [ErrChecksumMismatch] - so a caller can tell a transfer worth retrying from
	// one that is not. Every other failure carries its message and no kind,
	// including the local ones around a download: creating, writing, flushing or
	// closing the staged file, and reading back a partial.
	Cause error

	// ProgramIntact reports whether the program may safely carry on running.
	//
	// True is the ordinary case and covers every refusal: the update was
	// declined or abandoned while the program was completely untouched, and
	// there is nothing to do but log it and continue.
	//
	// False means the update went past the point of no return and could not be
	// undone. The drain has run, [Config.BeforeHandoff] has fired, and the
	// binary on disk is no longer the image this process is running. Whatever
	// the program shut down in preparation for the handover is still shut down,
	// and no handover happened. A caller that carries on past a false is running
	// a program whose shutdown has already taken place - for anything with
	// resources to release, a licence to enforce or a lease to hold, the only
	// correct response is to terminate and let a supervisor start the binary
	// that is actually on disk.
	//
	// Status alone cannot express this. A pre-download refusal and a failed
	// restore after a failed exec are both StatusFailed, and only one of them is
	// survivable.
	//
	// The zero value is false on purpose: a Result nobody filled in must not
	// read as "everything is fine".
	ProgramIntact bool
}

// Source produces the bytes of a new binary.
//
// This is denju's entire view of the outside world. Implement it over HTTP, a
// gRPC stream, a shared filesystem, an object store, a serial link - denju does
// not know and does not care. The [github.com/qunulabs/denju/httpsource]
// subpackage implements it for a plain HTTPS GET.
type Source interface {
	// Fetch writes the new binary to w, which is never nil.
	//
	// denju owns everything around it: creating the temp file in the target
	// binary's directory so the later rename is atomic, hashing what is written
	// against [Request.SHA256], making it executable, and deleting it on any
	// failure - except that a failed transfer from a [ResumableSource] keeps
	// what it wrote, for the next attempt to resume. An implementation only has
	// to produce bytes or an error, and should return a write error from w
	// rather than swallow it.
	//
	// Fetch may be slow; it is bounded by [Config.DownloadTimeout] and by the
	// context passed to [Updater.Update].
	Fetch(ctx context.Context, req Request, w io.Writer) error
}

// ResumableSource is a [Source] that honours [Request.Offset]: its Fetch writes
// the artifact starting at that byte, not at the beginning, or returns an error
// wrapping [ErrResumeRejected] when it cannot.
//
// Implementing it changes what denju does with a failed download. For a plain
// Source a failed Fetch deletes everything written. For a ResumableSource a
// broken transfer keeps the bytes in a partial named after the request - its
// ID, TargetVersion and SHA256 - and the next Update with the same three
// resumes from them, after denju re-hashes what is already on disk. The digest check at the end still
// covers every byte, old and new.
//
// A partial is kept only when the transfer fails. It is deleted when writing
// to it fails (a full disk), when the source rejects its offset, when the
// complete file fails the digest, when flushing or closing it after a
// successful Fetch fails, when a download for a different request starts, and
// when nothing has written to it for [Config.PartialRetention]. A download
// through a plain (non-resumable) Source deletes every partial, the same
// request's included.
//
// Use one [Updater] per binary path, across processes as well as within one:
// two Updaters downloading beside the same binary would each delete the
// other's partial as belonging to a different request.
type ResumableSource interface {
	Source
	// ResumesFromOffset declares that Fetch honours Request.Offset. denju never
	// calls it; implementing it is the declaration.
	ResumesFromOffset()
}
