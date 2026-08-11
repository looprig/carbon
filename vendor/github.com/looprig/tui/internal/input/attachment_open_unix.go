//go:build !windows

package input

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens clean read-only with O_NOFOLLOW (refusing a symlinked
// final path component) and O_NONBLOCK (so an open of a FIFO with no writer
// connected yet returns immediately instead of blocking the synchronous
// caller forever — readAttachment's later IsRegular check then rejects the
// FIFO). ELOOP (or EMLINK, which some platforms report for the identical
// condition) is reported as an error wrapping errAttachmentSymlink; every
// other error — including a definitive not-found — is returned exactly as
// os.OpenFile produced it, so the caller can keep classifying it with
// errors.Is(err, os.ErrNotExist) exactly as it would against a plain
// os.OpenFile call.
func openNoFollow(clean string) (*os.File, error) {
	// #nosec G304 -- clean is validated by denyReason + classification before
	// this call, and this open additionally refuses to follow a
	// final-path-component symlink (O_NOFOLLOW).
	f, err := os.OpenFile(clean, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("%w: %w", errAttachmentSymlink, err)
		}
		return nil, err
	}
	return f, nil
}
