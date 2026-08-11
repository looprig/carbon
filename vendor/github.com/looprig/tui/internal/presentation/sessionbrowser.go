package presentation

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/looprig/core/uuid"
)

type SessionID = uuid.UUID

// SessionSummary is secret-free presentation metadata for one resumable session.
type SessionSummary struct {
	ID           SessionID
	Title        string
	State        string
	CreatedAt    time.Time
	LastActiveAt time.Time
}

// SessionBrowser is a process-scoped optional capability, deliberately separate from Agent.
type SessionBrowser interface {
	ListSessions(context.Context) ([]SessionSummary, error)
	ResumeSession(context.Context, SessionID) (Agent, error)
}

type Option func(*screenOptions)
type screenOptions struct {
	sessionBrowser SessionBrowser
	presentation   SessionPresentation
	// presentationSet distinguishes an explicit WithSessionPresentation(SessionPresentation{})
	// call from an omitted option: both leave presentation at its zero value, but only the
	// former must suppress New's SessionPresenter fallback (see WithSessionPresentation).
	presentationSet bool
}

func WithSessionBrowser(browser SessionBrowser) Option {
	return func(options *screenOptions) { options.sessionBrowser = browser }
}

type sessionsListedMsg struct {
	sessions []SessionSummary
	err      error
}

func listSessionsCmd(ctx context.Context, browser SessionBrowser) tea.Cmd {
	return func() tea.Msg {
		sessions, err := browser.ListSessions(ctx)
		return sessionsListedMsg{sessions: sessions, err: err}
	}
}
