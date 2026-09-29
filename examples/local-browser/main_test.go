package localbrowser

import (
	"os"
	"testing"

	"github.com/looprig/sandbox"
)

// TestMain initializes the sandbox first, exactly as an embedding
// application's main must (see README.md): browser.Start builds sandboxed
// executors, which Linux refuses without a prior sandbox.Init.
func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
}
