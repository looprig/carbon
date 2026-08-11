package rig

import (
	"context"
	"errors"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/workspacestore"
)

// NewSession brings up a brand-new live session. WithSeedSnapshot optionally
// materializes and commits a seed before any loop starts.
func (r *Rig) NewSession(ctx context.Context, opts ...SessionOption) (session.SessionController, error) {
	resolved, err := resolveSessionOptions(opts)
	if err != nil {
		return nil, err
	}
	return r.newSession(ctx, resolved.seed)
}

func (r *Rig) newSession(ctx context.Context, seed workspacestore.Ref) (session.SessionController, error) {
	runtime, err := r.lifecycle.NewSession(ctx, seed)
	if err != nil {
		return nil, mapRunError(err)
	}
	return runtime, nil
}

func (r *Rig) RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error) {
	runtime, err := r.lifecycle.RestoreSession(ctx, id)
	if err != nil {
		return nil, mapRestoreError(err)
	}
	return runtime, nil
}

func mapProcessNotificationsUnsupported(err error) (error, bool) {
	var unsupported *sessionruntime.ProcessServicesUnsupportedError
	if errors.As(err, &unsupported) {
		return &LifecycleError{Kind: LifecycleProcessNotificationsUnsupported, Cause: err}, true
	}
	return nil, false
}

func mapRestoreError(err error) error {
	if mapped, ok := mapProcessNotificationsUnsupported(err); ok {
		return mapped
	}
	return err
}

func mapRunError(err error) error {
	if mapped, ok := mapProcessNotificationsUnsupported(err); ok {
		return mapped
	}
	var run *sessionruntime.NewSessionError
	if !errors.As(err, &run) {
		return &LifecycleError{Kind: LifecycleSessionFailed, Cause: err}
	}
	kinds := map[sessionruntime.NewSessionErrorKind]LifecycleErrorKind{
		sessionruntime.NewSessionContextDone:        LifecycleContextDone,
		sessionruntime.NewSessionIDGenerationFailed: LifecycleIDGenerationFailed,
		sessionruntime.NewSessionLeaseFailed:        LifecycleLeaseFailed,
		sessionruntime.NewSessionJournalFailed:      LifecycleJournalFailed,
		sessionruntime.NewSessionAppenderFailed:     LifecycleAppenderFailed,
		sessionruntime.NewSessionRuntimeFailed:      LifecycleSessionFailed,
	}
	return &LifecycleError{Kind: kinds[run.Kind], Cause: run.Cause}
}
