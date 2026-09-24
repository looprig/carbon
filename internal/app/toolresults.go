package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools"
)

// toolresults.go wires READABLE tool-result retention (I2.2) into the pooled
// serve path: a tool result larger than the loop's preview is retained in full
// (up to the capture ceiling) as a SessionStore tool-result object in the SAME
// harness journal store the rig journals into, and the model can page it back
// with read_tool_result. Factory's object route serves the same object to the
// session's browser (browser/factory.go, internal/toolresultobjects).
//
// The TUI and headless paths do not wire it: they keep the preview-only
// behaviour (an oversized result is shaped and the elided middle is gone).

// toolResultSpillDirName is the spill base under the pooled launcher's data
// root. Each session spills under <base>/<runtime session id> while a capture
// uploads, and harness removes it afterwards. It is a sibling of the
// session-workspaces tree, never inside a workspace, so a workspace checkpoint
// can never archive a capture spill (harness refuses an overlap anyway).
const toolResultSpillDirName = "tool-result-spill"

// carbonToolLimits is the ONE declaration of Carbon's loop tool limits.
//
// ResultBytes is finite: it is the model's preview, and a result larger than
// it is what gets retained. With zero, nothing would ever be elided or
// retained. CaptureBytes is left zero, which harness resolves to
// loop.DefaultToolResultCaptureBytes (8 MiB); Factory's D7 check receives this
// same declared value through ServeToolResultCaptureBytes.
func carbonToolLimits() loop.ToolLimits {
	return loop.ToolLimits{Iterations: 100, Calls: 200, ResultBytes: 50 * 1024}
}

// ServeToolResultCaptureBytes is the capture ceiling Carbon's loops declare
// (loop.ToolLimits.CaptureBytes; zero means harness's default). The browser
// composition checks it against Factory's object verification ceiling before
// composing the object route, so a capture Factory could never serve fails the
// composition rather than every read.
func ServeToolResultCaptureBytes() int { return carbonToolLimits().CaptureBytes }

// toolResultRetention is one tenant's readable retention: the tenant journal
// store's tool-result objects and the launcher's spill base.
type toolResultRetention struct {
	objects   loop.ToolResultObjects
	spillBase string
}

// rigOptions wires retention into the rig. A nil retention wires nothing.
func (r *toolResultRetention) rigOptions() []rig.Option {
	if r == nil {
		return nil
	}
	return []rig.Option{rig.WithToolResultObjects(r.objects, r.spillBase)}
}

// toolDefinitions is read_tool_result, registered exactly where retention is
// wired: harness refuses to define a loop with the reader and no objects.
func (r *toolResultRetention) toolDefinitions() []tool.Definition {
	if r == nil {
		return nil
	}
	return []tool.Definition{tools.ReadToolResultDefinition()}
}

// prepareToolResultSpillBase creates the spill base owner-only if it is
// absent and then checks it the way harness will at the first capture: a real
// directory (not a symlink) with no group or other write. A base that fails is
// refused, not repaired -- by the time a repair ran, an entry could already
// have been planted -- and it is refused HERE, at open, rather than at the
// first oversized tool result, which is where harness would find it.
func prepareToolResultSpillBase(dataDir string) (string, error) {
	base := filepath.Join(dataDir, toolResultSpillDirName)
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", &StoreInitError{Stage: "tool-result-spill", Cause: err}
	}
	info, err := os.Lstat(base)
	if err != nil {
		return "", &StoreInitError{Stage: "tool-result-spill", Cause: err}
	}
	if !info.IsDir() {
		return "", &StoreInitError{Stage: "tool-result-spill", Cause: fmt.Errorf("%s is not a directory (mode %s)", base, info.Mode().Type())}
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return "", &StoreInitError{Stage: "tool-result-spill", Cause: fmt.Errorf("%s is writable by group or other (mode %04o)", base, perm)}
	}
	return base, nil
}
