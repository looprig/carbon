package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
	"github.com/looprig/tui"
	"github.com/looprig/tui/sessionadapter"
)

// RuntimeAgent keeps provider and policy knowledge in CodeRig while embedding the stable
// session adapter used by the TUI data plane. It OWNS the session's executor-set closers
// (through access) and supplies the synchronous, session-fixed presentation metadata
// (profile name, workspace root, permission diagnostics) the TUI displays. The access
// profile is fixed at Open; there is no in-session authority mutation surface.
type RuntimeAgent struct {
	*sessionadapter.Adapter
	sess             session.SessionController
	root             string
	access           *sessionAccess
	primerAlias      string
	primerEfforts    []model.Effort
	primerCandidates []PrimerCandidate
}

func newRuntimeAgentWithPrimerCandidates(adapter *sessionadapter.Adapter, sess session.SessionController, root string, access *sessionAccess, primerAlias string, primerEfforts []model.Effort, primerCandidates []PrimerCandidate) *RuntimeAgent {
	return &RuntimeAgent{
		Adapter:          adapter,
		sess:             sess,
		root:             root,
		access:           access,
		primerAlias:      primerAlias,
		primerEfforts:    append([]model.Effort(nil), primerEfforts...),
		primerCandidates: append([]PrimerCandidate(nil), primerCandidates...),
	}
}

// Close shuts the session down and then releases the session's executor sets exactly once.
// The adapter is closed FIRST (stopping any in-flight loop that could still use an executor),
// then the executor sets are closed, removing their owned scratch HOME directories and
// revoking their grant keys and egress proxies.
func (a *RuntimeAgent) Close(ctx context.Context) error {
	err := a.Adapter.Close(ctx)
	if a.access != nil {
		if closeErr := a.access.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
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
		return tui.LoopRuntimeOptions{}, fmt.Errorf("coderig: loop %s is unavailable", loopID)
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
		options.Models = []tui.ModelOption{{ID: tui.ModelID(publicID), Label: publicID}}
	} else {
		options.Models = make([]tui.ModelOption, 0, len(a.primerCandidates))
		for _, c := range a.primerCandidates {
			options.Models = append(options.Models, tui.ModelOption{ID: tui.ModelID(c.Alias), Label: c.Alias, Description: c.Description})
		}
	}
	efforts := a.primerEfforts
	if current, ok := currentPrimerCandidate(a.primerCandidates, selectedModel); ok {
		efforts = current.Efforts
	} else if len(efforts) == 0 && selectedModel.Caps.Thinking {
		efforts = []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}
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
		return fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	mode := loop.ModeName(id)
	if len(a.primerEfforts) != 0 {
		switch mode {
		case "quick":
			if !containsPrimerEffort(a.primerEfforts, model.EffortLow) {
				return fmt.Errorf("coderig: mode %q is not admitted by the configured primer", id)
			}
		case "deep":
			if !containsPrimerEffort(a.primerEfforts, model.EffortMax) {
				return fmt.Errorf("coderig: mode %q is not admitted by the configured primer", id)
			}
		}
	}
	return controller.SetMode(ctx, mode)
}

func (a *RuntimeAgent) SetModel(ctx context.Context, loopID uuid.UUID, id tui.ModelID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	if len(a.primerCandidates) == 0 {
		selectedModel := controller.Model()
		if a.publicModelID(selectedModel) != string(id) {
			return fmt.Errorf("coderig: model choice %q is stale or unknown", id)
		}
		return controller.Change(ctx, loop.ChangeModel(selectedModel))
	}
	candidate, ok := findPrimerCandidate(a.primerCandidates, string(id))
	if !ok {
		return fmt.Errorf("coderig: model choice %q is stale or unknown", id)
	}
	changes := []loop.Change{loop.ChangeModel(candidate.Model)}
	if currentEffort := controller.Model().Sampling.Effort; !containsPrimerEffort(candidate.Efforts, currentEffort) {
		changes = append(changes, loop.ChangeEffort(candidate.DefaultEffort))
	}
	if err := controller.Change(ctx, changes...); err != nil {
		var transportErr *loop.ContextTransportBindingError
		if errors.As(err, &transportErr) {
			return fmt.Errorf("coderig: model choice %q uses a different provider/endpoint than the active session; switching transports mid-session is not supported: %w", id, err)
		}
		return err
	}
	return nil
}

func (a *RuntimeAgent) SetEffort(ctx context.Context, loopID uuid.UUID, id tui.EffortID) error {
	controller, ok := a.sess.LoopController(loopID)
	if !ok {
		return fmt.Errorf("coderig: loop %s is unavailable", loopID)
	}
	effort := model.Effort(id)
	if !effort.Valid() {
		return fmt.Errorf("coderig: effort choice %q is unknown", id)
	}
	if len(a.primerEfforts) != 0 && !containsPrimerEffort(a.primerEfforts, effort) {
		return fmt.Errorf("coderig: effort choice %q is not admitted by the configured primer", id)
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
