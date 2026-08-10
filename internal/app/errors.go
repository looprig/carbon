package app

// WorkspaceRootError is returned during construction when the workspace root (the
// process working directory) cannot be resolved, so the file tools cannot be
// confined to a known root. Cause carries the underlying os.Getwd error.
type WorkspaceRootError struct{ Cause error }

func (e *WorkspaceRootError) Error() string {
	if e.Cause == nil {
		return "carbon: cannot resolve workspace root"
	}
	return "carbon: cannot resolve workspace root: " + e.Cause.Error()
}

func (e *WorkspaceRootError) Unwrap() error { return e.Cause }

// StoreInitError is returned by NewSessionStoreFactory when the on-disk session store cannot
// be opened. Stage names which layer failed (the fsstore backend, the sessionstore facade, or
// the workspace store) so the composition root can report a precise, actionable failure;
// Cause carries the underlying error. Persistence is the whole point of the CLI, so a store it
// cannot open fails loud at startup. It is errors.As-recoverable.
type StoreInitError struct {
	Stage string
	Cause error
}

func (e *StoreInitError) Error() string {
	if e.Cause == nil {
		return "carbon: cannot open session store (" + e.Stage + ")"
	}
	return "carbon: cannot open session store (" + e.Stage + "): " + e.Cause.Error()
}

func (e *StoreInitError) Unwrap() error { return e.Cause }

// StoreClosedError reports an operation attempted after a session-store factory
// reached its terminal Close state.
type StoreClosedError struct{}

func (*StoreClosedError) Error() string { return "carbon: session store factory is closed" }
