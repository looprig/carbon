package app

import (
	"context"

	"github.com/looprig/tui"
)

type sessionBrowser struct {
	factory *SessionStoreFactory
	cfg     Config
}

// SessionBrowser returns the process-scoped TUI capability backed by this factory's shared
// catalog and store. It remains valid while individual agents are closed and replaced.
func (f *SessionStoreFactory) SessionBrowser(cfg Config) tui.SessionBrowser {
	return &sessionBrowser{factory: f, cfg: cfg}
}

func (b *sessionBrowser) ListSessions(ctx context.Context) ([]tui.SessionSummary, error) {
	metas, err := b.factory.List(ctx)
	if err != nil {
		return nil, err
	}
	b.factory.mu.Lock()
	current := b.factory.currentSession
	b.factory.mu.Unlock()
	summaries := make([]tui.SessionSummary, 0, len(metas))
	for _, meta := range metas {
		if meta.SessionID == current {
			continue
		}
		state := string(meta.State)
		if state == "" {
			state = string(meta.Status)
		}
		summaries = append(summaries, tui.SessionSummary{
			ID:           meta.SessionID,
			Title:        meta.Title,
			State:        state,
			CreatedAt:    meta.CreatedAt,
			LastActiveAt: meta.LastActiveAt,
		})
	}
	return summaries, nil
}

func (b *sessionBrowser) ResumeSession(ctx context.Context, id tui.SessionID) (tui.Agent, error) {
	agent, err := b.factory.Open(ctx, SessionSelector{Resume: id}, b.cfg)
	if err != nil {
		return nil, err
	}
	return agent, nil
}

var _ tui.SessionBrowser = (*sessionBrowser)(nil)
