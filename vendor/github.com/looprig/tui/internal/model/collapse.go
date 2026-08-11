package model

// DisplayID is the stable identity of one rendered transcript entry.
type DisplayID uint64

// collapseState is the modern viewport's thinking-fold state: a global default over
// every committed entry plus per-entry overrides of that default. Because the modern
// buffer re-renders from the committed transcript each frame, a change here is
// RETROACTIVE — an already-built entry re-folds to match the new state on the next
// frame (design §Collapse / expand). It is held BY VALUE inside the model, so its
// override map is cloned on write (mirroring transcriptModel.recordLoopLive/loopLive):
// a collapseState copied into a model copy never mutates the other's map.
type CollapseState struct {
	// globalCollapsed is the fold applied to every entry with no override. It starts
	// TRUE — thinking renders folded by default (dense; expansion is reversible) — and
	// ToggleAll (ctrl+t) flips it authoritatively over the whole buffer.
	globalCollapsed bool
	// overrides is the per-entry override of globalCollapsed, keyed by the entry's
	// displayID; the value is "collapsed?" for that entry. An entry ABSENT from the map
	// follows globalCollapsed. Toggle sets one entry (cloning the map on write);
	// ToggleAll clears it, so the global toggle stays authoritative and retroactive.
	overrides map[DisplayID]bool
}

// newCollapseState returns the initial state: globalCollapsed true (thinking starts
// folded) and no per-entry overrides.
func NewCollapseState() CollapseState {
	return CollapseState{globalCollapsed: true}
}

// Effective reports whether entry id renders COLLAPSED: its per-entry override when one
// is present, else the global default. It is a value receiver (read-only) — the render
// path calls it per entry every frame — and is nil-map safe (a missing key reads the
// global).
func (c CollapseState) Effective(id DisplayID) bool {
	if v, ok := c.overrides[id]; ok {
		return v
	}
	return c.globalCollapsed
}

// GloballyCollapsed reports the default applied to entries without an override.
func (c CollapseState) GloballyCollapsed() bool { return c.globalCollapsed }

// ToggleAll flips the global default AND clears every per-entry override, so the global
// toggle (ctrl+t) is authoritative and retroactive: an entry previously toggled open
// re-follows the new global. Clearing REPLACES the map field with nil rather than
// mutating it in place, so a state copied into a model copy never observes the change
// through a shared backing map (clone-on-write). It is a pointer receiver (matching the
// viewportModel mutator seam convention).
func (c *CollapseState) ToggleAll() {
	c.globalCollapsed = !c.globalCollapsed
	c.overrides = nil
}

// Toggle flips exactly entry id relative to its CURRENT effective state and records the
// result as a per-entry override (a header click on that entry). The override map is
// cloned on write (value-copy contract) so a state copied into a model copy never
// mutates the other's overrides. It is a pointer receiver.
func (c *CollapseState) Toggle(id DisplayID) {
	next := make(map[DisplayID]bool, len(c.overrides)+1)
	for k, v := range c.overrides {
		next[k] = v
	}
	next[id] = !c.Effective(id)
	c.overrides = next
}
