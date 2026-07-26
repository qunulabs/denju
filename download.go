package denju

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// download stages the new binary next to the one it will replace and verifies
// its digest, returning the staged path.
//
// The temp file is created in the TARGET BINARY'S directory, not in the system
// temp directory, for two reasons: the swap that follows is an os.Rename, which
// is only atomic within one filesystem, and staging elsewhere would silently
// turn the swap into a copy across a mount boundary. It also means a download
// too large for the destination fails here, while the running program is still
// untouched, rather than halfway through the swap.
//
// Anything that goes wrong removes the temp file. On success the caller owns it.
func (u *Updater) download(ctx context.Context, req Request, src Source) (string, error) {
	dir := filepath.Dir(u.binaryPath)
	tmp, err := os.CreateTemp(dir, u.n.tmpDownload)
	if err != nil {
		return "", fmt.Errorf("create a staging file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	ctx, cancel := context.WithTimeout(ctx, u.cfg.DownloadTimeout)
	defer cancel()

	h := sha256.New()
	if err := src.Fetch(ctx, req, io.MultiWriter(tmp, h)); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return "", fmt.Errorf("download the update: %w (after %s)", err, u.cfg.DownloadTimeout)
		}
		return "", fmt.Errorf("download the update: %w", err)
	}
	// fsync before the digest is trusted: the bytes have to be on the device,
	// not merely in the page cache, because the process that verified them is
	// about to be replaced by one that only sees what survived.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("flush the downloaded update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("finalize the downloaded update: %w", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, req.SHA256) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("the downloaded update does not match its checksum: expected %s, got %s", req.SHA256, got)
	}
	return tmpPath, nil
}
