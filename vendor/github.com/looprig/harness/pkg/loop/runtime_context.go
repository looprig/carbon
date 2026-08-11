package loop

import (
	"context"

	"github.com/looprig/core/content"
)

// RuntimeContextProvider yields the volatile per-turn context blocks (date/cwd/git)
// the loop appends at the turn tail. Implementations must be cheap and non-fatal:
// a failure degrades (fewer or no blocks), never errors the turn. The returned
// slice may be empty (or nil) — the loop appends nothing in that case.
//
// The interface lives in the engine-generic loop package so the loop can depend on
// it without importing any concrete provider; a default implementation is wired at
// the product composition root, keeping this package free of os/exec.
type RuntimeContextProvider interface {
	Blocks(ctx context.Context) []content.Block
}
