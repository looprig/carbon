package app

import (
	"errors"
	"fmt"

	"github.com/looprig/fsstore"
)

// LegacyDataRootError refuses a data directory written before fsstore v0.6.0.
// fsstore v0.6.0 changed its on-disk layout (every primitive's leaf gained a
// suffix, so a key and a key beneath it can coexist) and does not migrate: it
// refuses any older root. Carbon does not migrate either. The user must move
// or delete the directory; a fresh one is created on the next start.
//
// It is PERMANENT for that root: nothing a retry does can change the answer.
type LegacyDataRootError struct {
	// Root is the fsstore root that was refused.
	Root string
	// Cause is fsstore's *LegacyLayoutError (it matches fsstore.ErrLegacyLayout).
	Cause error
}

func (e *LegacyDataRootError) Error() string {
	return fmt.Sprintf("carbon: data directory %q was written by an older Carbon and cannot be opened: "+
		"move or delete this directory (pre-v0.6.0 data is not migrated): %v", e.Root, e.Cause)
}

func (e *LegacyDataRootError) Unwrap() error { return e.Cause }

// openFSStore opens one fsstore root for stage. A pre-v0.6.0 root becomes a
// StoreInitError wrapping *LegacyDataRootError; any other failure a plain
// StoreInitError.
func openFSStore(stage, root string) (*fsstore.Store, error) {
	fs, err := fsstore.Open(fsstore.Options{Root: root})
	if err == nil {
		return fs, nil
	}
	if errors.Is(err, fsstore.ErrLegacyLayout) {
		// Root is the path as configured, which is the one the user knows;
		// fsstore's own error (the cause) names its resolved form and the
		// first legacy entry it found.
		return nil, &StoreInitError{Stage: stage, Cause: &LegacyDataRootError{Root: root, Cause: err}}
	}
	return nil, &StoreInitError{Stage: stage, Cause: err}
}

// isLegacyDataRoot reports a permanent legacy-layout refusal.
func isLegacyDataRoot(err error) bool {
	var legacy *LegacyDataRootError
	return errors.As(err, &legacy)
}
