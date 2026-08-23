package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
	mcpharness "github.com/looprig/mcp/pkg/harness"
	"github.com/looprig/tui"
	"github.com/looprig/tui/sessionadapter"
)

// RuntimeAgent keeps provider and policy knowledge in Carbon while embedding the stable
// session adapter used by the TUI data plane. It OWNS the session's executor-set closers
// (through access) and its MCP composition closers (mgr, adopter, both nil when the
// session has no mcp.json), and supplies the synchronous, session-fixed presentation
// metadata (profile name, workspace root, permission diagnostics) the TUI displays. The
// access profile is fixed at Open; there is no in-session authority mutation surface.
type RuntimeAgent struct {
	*sessionadapter.Adapter
	sess              session.SessionController
	root              string
	access            *sessionAccess
	mgr               *mcpharness.Manager
	adopter           *mcpharness.Adopter
	recorder          *mcpNoticeRecorder
	primerAlias       string
	primerEfforts     []model.Effort
	primerCandidates  []PrimerCandidate
	credentialRuntime *credentialRuntime
	credentialLease   *credentialRegistryLease
	closeMu           sync.Mutex
	closeDone         chan struct{}
	closeErr          error
	closing           bool
	closed            bool
}

// newRuntimeAgentWithPrimerCandidates builds a RuntimeAgent with no MCP composition. It is
// newWithClientUsingStores' constructor: that path (New's headless composition root, a
// separate construction seam from openRuntimeAgent used by SessionStoreFactory and
// newSessionOverStores) does not wire MCP -- see openRuntimeAgent's own doc for where that
// wiring lives.
func newRuntimeAgentWithPrimerCandidates(adapter *sessionadapter.Adapter, sess session.SessionController, root string, access *sessionAccess, primerAlias string, primerEfforts []model.Effort, primerCandidates []PrimerCandidate) *RuntimeAgent {
	return newRuntimeAgentWithMCP(adapter, sess, root, access, nil, nil, nil, primerAlias, primerEfforts, primerCandidates)
}

// newRuntimeAgentWithMCP is openRuntimeAgent's constructor: mgr, adopter, and recorder are the
// session's MCP composition closers/sinks, all nil when the session was opened with no
// mcp.json. recorder follows the same mgr/adopter path from mcpSessionAssembly so the notices
// it captured during construction remain reachable through MCPNotices for the life of the
// session, instead of being discarded with mcpSessionAssembly's local variables.
func newRuntimeAgentWithMCP(adapter *sessionadapter.Adapter, sess session.SessionController, root string, access *sessionAccess, mgr *mcpharness.Manager, adopter *mcpharness.Adopter, recorder *mcpNoticeRecorder, primerAlias string, primerEfforts []model.Effort, primerCandidates []PrimerCandidate) *RuntimeAgent {
	return &RuntimeAgent{
		Adapter:          adapter,
		sess:             sess,
		root:             root,
		access:           access,
		mgr:              mgr,
		adopter:          adopter,
		recorder:         recorder,
		primerAlias:      primerAlias,
		primerEfforts:    append([]model.Effort(nil), primerEfforts...),
		primerCandidates: append([]PrimerCandidate(nil), primerCandidates...),
	}
}

// MCPNotices returns the notices captured by the session's MCP Reporter
// (tool-name collisions, adoption failures -- see mcpharness.NoticeKind), or
// nil when the session has no MCP composition at all. This is a plain,
// read-only accessor over the already-bounded, already-safe recorder
// (mcp.go's mcpNoticeRecorder); it does not publish notices anywhere or
// invent a new delivery mechanism -- a caller (CLI diagnostics, a future TUI
// surface) polls it directly.
func (a *RuntimeAgent) MCPNotices() []mcpharness.Notice {
	if a.recorder == nil {
		return nil
	}
	return a.recorder.Notices()
}

// Close shuts the session down, then releases its MCP composition (if any), then its
// executor sets, in that order. The adapter is closed FIRST (stopping any in-flight loop
// that could still use an executor or an MCP tool); the adopter stops next (it only reacts
// to the now-stopped session's own idle events); the MCP manager closes its connections
// third; the executor sets close LAST, removing their owned scratch HOME directories and
// revoking their grant keys and egress proxies. mgr and adopter are nil-safe (a session
// opened with no mcp.json has neither), and RuntimeAgent.Close is linearized and
// idempotent so a close/logout race cannot release a source before the adapter drains.
func (a *RuntimeAgent) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.closeMu.Lock()
	if a.closed {
		err := a.closeErr
		a.closeMu.Unlock()
		return err
	}
	if a.closing {
		done := a.closeDone
		a.closeMu.Unlock()
		<-done
		a.closeMu.Lock()
		err := a.closeErr
		a.closeMu.Unlock()
		return err
	}
	a.closing = true
	a.closeDone = make(chan struct{})
	done := a.closeDone
	a.closeMu.Unlock()

	var err error
	if a.Adapter != nil {
		err = a.Adapter.Close(ctx)
	}
	if a.adopter != nil {
		if closeErr := a.adopter.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if a.mgr != nil {
		if closeErr := a.mgr.Close(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if a.access != nil {
		if closeErr := a.access.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	// The inference/session adapter and its executor-backed access set are
	// drained before the credential source and its catalog/store. This is the
	// reverse dependency order required by the credential lifecycle.
	if a.credentialRuntime != nil {
		a.credentialRuntime.endSession()
		closeErr := error(nil)
		if a.credentialLease != nil {
			closeErr = a.credentialLease.Release()
		} else {
			closeErr = a.credentialRuntime.Close()
		}
		if closeErr != nil && err == nil {
			err = closeErr
		}
	}
	a.closeMu.Lock()
	a.closeErr = err
	a.closed = true
	a.closing = false
	close(done)
	a.closeMu.Unlock()
	return err
}

// SessionPresentation supplies the TUI's synchronous session security context: the fixed
// access profile name, the workspace root, and the manual out-of-catalog permission
// diagnostics surfaced by the workspace permission-store load. The TUI reads it at screen
// construction and on cross-session resume so it always displays THIS session's context.
func (a *RuntimeAgent) SessionPresentation() tui.SessionPresentation {
	presentation := tui.SessionPresentation{WorkspaceRoot: a.root}
	if a.access != nil {
		presentation.ProfileName = a.access.profileName
		presentation.PermissionDiagnostics = a.access.diagnostics
		if presentation.WorkspaceRoot == "" {
			presentation.WorkspaceRoot = a.access.workspace
		}
	}
	return presentation
}

func (a *RuntimeAgent) LoopRuntimeOptions(_ context.Context, loopID uuid.UUID) (tui.LoopRuntimeOptions, error) {
	handle, ok := a.sess.Loop(loopID)
	if !ok {
		return tui.LoopRuntimeOptions{}, fmt.Errorf("carbon: loop %s is unavailable", loopID)
	}
	options := tui.LoopRuntimeOptions{}
	if catalog, ok := handle.(loop.ModeCatalog); ok {
		for _, mode := range catalog.Modes() {
			label := string(mode)
			if label == "" {
				label = "Default"
			}
			options.Modes = append(options.Modes, tui.ModeOption{ID: tui.ModeID(mode), Label: label})
		}
	}
	selectedModel := handle.Model()
	if len(a.primerCandidates) == 0 {
		publicID := a.publicModelID(selectedModel)
		options.Models = []tui.ModelOption{{
			ID:       tui.ModelID(publicID),
			Provider: string(selectedModel.Provider),
			Label:    publicID,
		}}
	} else {
		options.Models = make([]tui.ModelOption, 0, len(a.primerCandidates))
		for _, c := range a.primerCandidates {
			options.Models = append(options.Models, tui.ModelOption{
				ID:          tui.ModelID(c.Alias),
				Provider:    string(c.Model.Provider),
				Label:       c.Alias,
				Description: c.Description,
			})
		}
	}
	efforts := a.primerEfforts
	if current, ok := currentPrimerCandidate(a.primerCandidates, selectedModel); ok {
		efforts = current.Efforts
	} else if len(efforts) == 0 && selectedModel.Caps.Thinking {
		efforts = []model.Effort{
			model.EffortNone, model.EffortMinimal, model.EffortLow, model.EffortMedium,
			model.EffortHigh, model.EffortXHigh, model.EffortMax,
		}
	}
	for _, effort := range efforts {
		label := string(effort)
		if label == "" {
			label = "Model default"
		}
		options.Efforts = append(options.Efforts, tui.EffortOption{ID: tui.EffortID(effort), Label: label})
	}
	return options, nil
}

func (a *RuntimeAgent) SetMode(ctx context.Context, loopID uuid.UUID, id tui.ModeID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("carbon: loop %s is unavailable", loopID)
	}
	mode := loop.ModeName(id)
	if len(a.primerEfforts) != 0 {
		switch mode {
		case "quick":
			if !containsPrimerEffort(a.primerEfforts, model.EffortLow) {
				return fmt.Errorf("carbon: mode %q is not admitted by the configured primer", id)
			}
		case "deep":
			if !containsPrimerEffort(a.primerEfforts, model.EffortMax) {
				return fmt.Errorf("carbon: mode %q is not admitted by the configured primer", id)
			}
		}
	}
	return controller.SetMode(ctx, mode)
}

func (a *RuntimeAgent) SetModel(ctx context.Context, loopID uuid.UUID, id tui.ModelID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("carbon: loop %s is unavailable", loopID)
	}
	if len(a.primerCandidates) == 0 {
		selectedModel := controller.Model()
		if a.publicModelID(selectedModel) != string(id) {
			return fmt.Errorf("carbon: model choice %q is stale or unknown", id)
		}
		return controller.Change(ctx, loop.ChangeModel(selectedModel))
	}
	candidate, ok := findPrimerCandidate(a.primerCandidates, string(id))
	if !ok {
		return fmt.Errorf("carbon: model choice %q is stale or unknown", id)
	}
	currentModel := controller.Model()
	changes := []loop.Change{loop.ChangeModel(candidate.Model)}
	if !containsPrimerEffort(candidate.Efforts, currentModel.Sampling.Effort) {
		changes = append(changes, loop.ChangeEffort(candidate.DefaultEffort))
	}
	if err := controller.Change(ctx, changes...); err != nil {
		return fmt.Errorf("carbon: switch to model %q: %w", id, err)
	}
	return nil
}

func (a *RuntimeAgent) SetEffort(ctx context.Context, loopID uuid.UUID, id tui.EffortID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("carbon: loop %s is unavailable", loopID)
	}
	effort := model.Effort(id)
	if !effort.Valid() {
		return fmt.Errorf("carbon: effort choice %q is unknown", id)
	}
	admitted := a.primerEfforts
	if current, ok := currentPrimerCandidate(a.primerCandidates, controller.Model()); ok {
		admitted = current.Efforts
	}
	if len(admitted) != 0 && !containsPrimerEffort(admitted, effort) {
		return fmt.Errorf("carbon: effort choice %q is not admitted by the configured primer", id)
	}
	return controller.Change(ctx, loop.ChangeEffort(effort))
}

func containsPrimerEffort(efforts []model.Effort, wanted model.Effort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}

func findPrimerCandidate(candidates []PrimerCandidate, alias string) (PrimerCandidate, bool) {
	for _, c := range candidates {
		if c.Alias == alias {
			return c, true
		}
	}
	return PrimerCandidate{}, false
}

func currentPrimerCandidate(candidates []PrimerCandidate, current model.Model) (PrimerCandidate, bool) {
	for _, c := range candidates {
		if runtimeModelKeyFor(c.Model) == runtimeModelKeyFor(current) {
			return c, true
		}
	}
	return PrimerCandidate{}, false
}

func modelID(value model.Model) string { return string(value.Provider) + "/" + value.Name }

func (a *RuntimeAgent) publicModelID(value model.Model) string {
	if c, ok := currentPrimerCandidate(a.primerCandidates, value); ok {
		return c.Alias
	}
	if a.primerAlias != "" {
		return a.primerAlias
	}
	return modelID(value)
}

var (
	_ tui.Agent             = (*RuntimeAgent)(nil)
	_ tui.RuntimeCatalog    = (*RuntimeAgent)(nil)
	_ tui.RuntimeController = (*RuntimeAgent)(nil)
	_ tui.SessionPresenter  = (*RuntimeAgent)(nil)
)
