// This file defines which Loops may consume a Session-scoped binding.
//
// Visibility is not ownership. A Session owns one connection to a server, and
// that is a fact about credentials, process lifetime, and state; which Loops may
// see the server's capabilities is a separate policy decision, and this type is
// where it is made (design §Binding model and scope).
//
// The selector fails closed by construction: its zero value permits nothing, so
// a binding whose Visibility a caller forgot to set is invisible rather than
// universal. Reaching every Loop requires saying AllLoops() out loud.

package mcpharness

import (
	"fmt"
	"slices"

	"github.com/looprig/core/uuid"
)

// selectorMode is how a LoopSelector decides. The zero mode selects nothing,
// which is what makes the zero LoopSelector deny.
type selectorMode uint8

const (
	// selectNone is the zero mode: a selector nobody built. It permits nothing.
	selectNone selectorMode = iota
	// selectAll permits every Loop.
	selectAll
	// selectIDs permits the Loops whose IDs are listed.
	selectIDs
	// selectNames permits the Loops whose names are listed.
	selectNames
)

// LoopSelector decides which Loops may consume a Session-scoped binding. It is
// an immutable value: build one with AllLoops, Loops, or Named and copy it
// freely.
//
// The zero value permits no Loop. That is deliberate — see the file comment.
type LoopSelector struct {
	mode  selectorMode
	ids   []uuid.UUID
	names []string
}

// AllLoops permits every Loop in the Session, including Loops created later.
func AllLoops() LoopSelector { return LoopSelector{mode: selectAll} }

// Loops permits exactly the Loops with these IDs.
//
// Identity is the precise selector: a Loop ID is minted by the Session and
// cannot be spoofed by a delegate that happens to share a name.
func Loops(ids ...uuid.UUID) LoopSelector {
	return LoopSelector{mode: selectIDs, ids: slices.Clone(ids)}
}

// Named permits exactly the Loops with these names, matched exactly and
// case-sensitively (as pkg/client's ToolFilter matches tool names).
//
// Names are how a composition root talks about Loops it has not created yet, so
// this is the selector for static configuration. It is weaker than Loops: a name
// is a label the application chose, not an identity the Session minted.
func Named(names ...string) LoopSelector {
	return LoopSelector{mode: selectNames, names: slices.Clone(names)}
}

// Permits reports whether the Loop identified by loopID and name may consume the
// binding. It never returns true for a selector nobody built.
//
// A Loop must present both identifiers because the caller does not know which
// kind of selector it is asking; the selector uses the one it selects on and
// ignores the other. An empty name never matches a named selector even if the
// selector somehow lists an empty name, and a zero loopID never matches an ID
// selector: absent identity is not identity.
func (s LoopSelector) Permits(loopID uuid.UUID, name string) bool {
	switch s.mode {
	case selectAll:
		return true
	case selectIDs:
		if loopID.IsZero() {
			return false
		}
		return slices.Contains(s.ids, loopID)
	case selectNames:
		if name == "" {
			return false
		}
		return slices.Contains(s.names, name)
	case selectNone:
		return false
	default:
		return false
	}
}

// isZero reports whether nobody built this selector.
func (s LoopSelector) isZero() bool { return s.mode == selectNone }

// String returns a bounded, safe description for diagnostics. It names the mode
// and the population size, never the members: a selector may list dozens of IDs
// and this is a log line, not a dump.
func (s LoopSelector) String() string {
	switch s.mode {
	case selectAll:
		return "all-loops"
	case selectIDs:
		return fmt.Sprintf("loops(%d)", len(s.ids))
	case selectNames:
		return fmt.Sprintf("named(%d)", len(s.names))
	case selectNone:
		return "none"
	default:
		return "invalid"
	}
}

// validate reports whether the selector can ever permit anything and whether its
// members are well formed.
//
// An empty ID or name set is rejected rather than treated as "permits nothing":
// a Session-scoped binding no Loop can see is dead configuration — a connection
// that is opened, authenticated, and maintained for nobody — and the place to
// discover that is Validate, not a support ticket about a server that "does not
// work". A caller that really wants a binding nobody sees does not declare it.
func (s LoopSelector) validate() error {
	switch s.mode {
	case selectAll:
		return nil
	case selectIDs:
		if len(s.ids) == 0 {
			return fmt.Errorf("visibility: Loops() selects no Loop")
		}
		for i, id := range s.ids {
			if id.IsZero() {
				return fmt.Errorf("visibility: Loops() entry %d is the zero UUID", i)
			}
		}
		return nil
	case selectNames:
		if len(s.names) == 0 {
			return fmt.Errorf("visibility: Named() selects no Loop")
		}
		for i, name := range s.names {
			if name == "" {
				return fmt.Errorf("visibility: Named() entry %d is empty", i)
			}
		}
		return nil
	case selectNone:
		return fmt.Errorf("visibility: no selector was built (use AllLoops, Loops, or Named)")
	default:
		return fmt.Errorf("visibility: unknown selector mode %d", s.mode)
	}
}
