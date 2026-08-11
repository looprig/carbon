package tool

import "sync"

// NewWorkspaceObservations returns an empty concurrency-safe observation set for
// one Loop binding.
func NewWorkspaceObservations() WorkspaceObservations {
	return &workspaceObservations{states: make(map[string]*workspaceObservationState)}
}

type workspaceObservationState struct {
	mu  sync.Mutex
	obs FileObservation
}

type workspaceObservations struct {
	mu     sync.Mutex
	states map[string]*workspaceObservationState
}

func (o *workspaceObservations) state(path string) *workspaceObservationState {
	o.mu.Lock()
	defer o.mu.Unlock()
	state, ok := o.states[path]
	if !ok {
		state = &workspaceObservationState{}
		o.states[path] = state
	}
	return state
}

func (o *workspaceObservations) WithPath(path string, fn func(*FileObservation) error) error {
	state := o.state(path)
	state.mu.Lock()
	defer state.mu.Unlock()
	return fn(&state.obs)
}

func (o *workspaceObservations) InvalidateAll() {
	o.mu.Lock()
	states := make([]*workspaceObservationState, 0, len(o.states))
	for _, state := range o.states {
		states = append(states, state)
	}
	o.mu.Unlock()
	for _, state := range states {
		state.mu.Lock()
		state.obs = FileObservation{}
		state.mu.Unlock()
	}
}
