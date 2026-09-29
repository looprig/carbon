package browser_test

import (
	"os"
	"testing"

	"github.com/looprig/sandbox"
)

// TestMain initializes the sandbox before anything else runs. browser.Start
// builds sandboxed executors, and on Linux sandbox refuses to construct one
// (ErrInitNotCalled) unless sandbox.Init ran first in the process -- it is
// also the re-exec entry point for the Linux sandbox helper, so it must
// precede flag parsing in m.Run. It is a no-op on other platforms, which is
// why a macOS-only run never noticed it missing.
func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
}
