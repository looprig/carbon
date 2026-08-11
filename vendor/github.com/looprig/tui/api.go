// Package tui exposes the reusable Looprig terminal interface.
package tui

import (
	"context"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/tui/internal/presentation"
)

// Public presentation contracts retain their original type identity through aliases.
type (
	EventStream         = presentation.EventStream
	Agent               = presentation.Agent
	OpenAgent           = presentation.OpenAgent
	AgentBanner         = presentation.AgentBanner
	AgentHolder         = presentation.AgentHolder
	TerminalErrorHolder = presentation.TerminalErrorHolder
	HandoffFinalizer    = presentation.HandoffFinalizer
	Screen              = presentation.Screen
	DisplayProjection   = presentation.DisplayProjection
	RestoreBacklogError = presentation.RestoreBacklogError
	RuntimeCatalog      = presentation.RuntimeCatalog
	RuntimeController   = presentation.RuntimeController
	ModeID              = presentation.ModeID
	ModelID             = presentation.ModelID
	EffortID            = presentation.EffortID
	ModeOption          = presentation.ModeOption
	ModelOption         = presentation.ModelOption
	EffortOption        = presentation.EffortOption
	LoopRuntimeOptions  = presentation.LoopRuntimeOptions
	SessionPresentation = presentation.SessionPresentation
	SessionPresenter    = presentation.SessionPresenter
	SessionID           = presentation.SessionID
	SessionSummary      = presentation.SessionSummary
	SessionBrowser      = presentation.SessionBrowser
	Option              = presentation.Option
	Status              = presentation.Status
	ToolStatus          = presentation.ToolStatus
	ToolCallView        = presentation.ToolCallView
)

const (
	StatusIdle         = presentation.StatusIdle
	StatusRunning      = presentation.StatusRunning
	StatusInterrupting = presentation.StatusInterrupting
	StatusResetting    = presentation.StatusResetting

	ToolRunning   = presentation.ToolRunning
	ToolOK        = presentation.ToolOK
	ToolError     = presentation.ToolError
	ToolCancelled = presentation.ToolCancelled
)

// New constructs the interactive terminal screen.
func New(ctx context.Context, agent Agent, open OpenAgent, banner AgentBanner, options ...Option) Screen {
	return presentation.New(ctx, agent, open, banner, options...)
}

func WithSessionBrowser(browser SessionBrowser) Option {
	return presentation.WithSessionBrowser(browser)
}

// WithSessionPresentation supplies the synchronous, consumer-filled session metadata
// (workspace root, fixed access profile name, permission diagnostics) the screen
// captures at construction and displays. Diagnostics are surfaced before the first
// permission gate; the profile is shown as session metadata, never a mutable control.
func WithSessionPresentation(p SessionPresentation) Option {
	return presentation.WithSessionPresentation(p)
}

// FoldDisplay rebuilds the display projection from session events.
func FoldDisplay(events []event.Event) DisplayProjection {
	return presentation.FoldDisplay(events)
}

// AllLoopsEventFilter subscribes to enduring and ephemeral events from every loop.
func AllLoopsEventFilter() event.EventFilter {
	return presentation.AllLoopsEventFilter()
}

// RenderStatusLine renders a standalone status indicator.
func RenderStatusLine(status Status) string {
	return presentation.RenderStatusLine(status)
}
