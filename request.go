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
	// failure. An implementation only has to produce bytes or an error.
	//
	// Fetch may be slow; it is bounded by [Config.DownloadTimeout] and by the
	// context passed to [Updater.Update].
	Fetch(ctx context.Context, req Request, w io.Writer) error
}
