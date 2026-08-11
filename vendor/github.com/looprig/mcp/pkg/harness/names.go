// This file owns the one naming question that only exists at this layer: the
// model-facing tool namespace is a *union*, and nothing below it can see the
// union.
//
// Model-facing names are not constructed here. A binding's catalog has already
// assigned every tool a ModelName — qualified, sanitized, length-bounded, and
// digest-suffixed where truncation or a sanitization collision required it (see
// client.ToolSpec.ModelName). Re-deriving a name here would produce a second
// answer to a question that already has one, and the two schemes would agree on
// the easy cases and diverge on exactly the hard ones, which is how a model's
// call ends up routed to a tool the model did not name. So this file consumes
// those names; it never invents one.
//
// What it adds is the check no catalog can perform. A catalog enforces that
// ModelNames are unique within one binding's generation, because that is the
// whole set it can see. A Manager shows a Loop the tools of *many* bindings
// under one namespace, and two bindings can independently produce the same
// ModelName. That is not a hypothetical: binding names permit '_' after the
// first byte (client.Name.Validate), and the qualified form frames its segments
// with "__", so the framing is ambiguous by construction —
//
//	binding "a__b", raw tool "c"    -> "mcp__a__b__c"
//	binding "a",    raw tool "b__c" -> "mcp__a__b__c"
//
// — with no truncation, no sanitization, and no digest collision involved.
// Neither binding is defective, neither catalog can see the other, and the
// collision only becomes visible when the union is assembled. Here.
//
// The table fails closed on that collision rather than letting a last-writer
// win, because the alternative is silent misrouting: one server's tool answering
// under another server's authority, with that server's credentials.

package mcpharness

import (
	"fmt"

	"github.com/looprig/mcp/pkg/client"
)

// toolRef is the raw identity behind one model-facing name: what to call, and
// on which binding.
//
// It is what lookup hands back, and it is deliberately not a client.ToolSpec.
// The table's job is identity, not the tool's schema, description or
// annotations — a caller that needs those has the catalog the spec came from.
type toolRef struct {
	binding string
	rawTool string
}

// DuplicateModelNameError reports two tools that claim one model-facing name.
//
// It is a typed error because the Manager must classify it rather than merely
// report it: the correct response is to refuse the offending binding and mark it
// degraded, leaving the binding that already holds the name serving. A caller
// that could only render this would have to choose between failing the whole
// Session and shipping an ambiguous namespace.
//
// Every field is a name — a binding name (validated by client.Name) or a
// ModelName (already sanitized to [a-zA-Z0-9_-] by the catalog) — so the
// message carries no server-controlled bytes and nothing to redact.
type DuplicateModelNameError struct {
	// ModelName is the contested model-facing name.
	ModelName string
	// Binding is the binding whose tool was rejected.
	Binding string
	// OtherBinding is the binding that already holds the name. It equals
	// Binding when one binding offered the name twice, which a catalog should
	// have prevented; the table still refuses it.
	OtherBinding string
}

func (e *DuplicateModelNameError) Error() string {
	return fmt.Sprintf("mcp: binding %q offers tool name %q, which binding %q already holds; a model-facing name must resolve to exactly one tool",
		e.Binding, e.ModelName, e.OtherBinding)
}

// nameTable is the reverse map from a model-facing name to the tool behind it,
// across every binding in one namespace.
//
// It is retained for one generation of the namespace: a reconfiguration or an
// adopted catalog builds a new table rather than mutating this one, so a table a
// live turn is reading never changes underneath it.
//
// It is not safe for concurrent use. It is built once, under whatever lock the
// Manager already holds for the swap, and read-only thereafter.
type nameTable struct {
	byModel map[string]toolRef
}

// newNameTable returns an empty table.
func newNameTable() *nameTable {
	return &nameTable{byModel: make(map[string]toolRef)}
}

// add records spec under binding and returns nil, or refuses it.
//
// It fails closed on a name already held (*DuplicateModelNameError) and on a
// spec that cannot be a routable identity at all: an empty binding, an empty
// RawName — which is what would go on the wire — or an empty ModelName, which
// means no catalog ever assigned one. Those are defects in the caller rather
// than in a server, since a catalog rejects all three before a ToolSpec exists;
// they are checked because this is the boundary where a zero-valued struct would
// otherwise become a tool that cannot be called and cannot be found.
//
// A rejected tool is not recorded. The table is therefore always a set of names
// that each resolve to exactly one tool, which is the invariant lookup depends
// on.
func (t *nameTable) add(binding string, spec client.ToolSpec) error {
	switch {
	case binding == "":
		return fmt.Errorf("mcp: tool %q has no binding", spec.RawName)
	case spec.RawName == "":
		return fmt.Errorf("mcp: binding %q offers a tool with no raw name", binding)
	case spec.ModelName == "":
		return fmt.Errorf("mcp: binding %q offers tool %q with no model-facing name", binding, spec.RawName)
	}
	if held, taken := t.byModel[spec.ModelName]; taken {
		return &DuplicateModelNameError{
			ModelName:    spec.ModelName,
			Binding:      binding,
			OtherBinding: held.binding,
		}
	}
	t.byModel[spec.ModelName] = toolRef{binding: binding, rawTool: spec.RawName}
	return nil
}

// lookup returns the binding and raw tool name behind a model-facing name.
//
// This is for display and diagnostics — rendering a call in a transcript,
// naming a tool in a log line or a permission prompt. It is never how a call is
// routed: an adapted tool's closure carries its binding and raw name from the
// moment it was built, so the call path has the raw identity in hand and never
// needs to recover it.
//
// That distinction is the reason this returns the pair rather than parsing one.
// A model name is lossy in both directions that matter — sanitization maps many
// raw names onto one, truncation discards bytes, and the "__" framing is
// ambiguous where a binding name contains an underscore (see the file comment) —
// so a name can only be resolved by the table that assigned it. Reparsing one
// would be guesswork about which tool to invoke.
//
// An unknown name fails closed: it resolves to nothing rather than to a guess.
func (t *nameTable) lookup(qualified string) (binding, rawTool string, ok bool) {
	ref, ok := t.byModel[qualified]
	if !ok {
		return "", "", false
	}
	return ref.binding, ref.rawTool, true
}

// len reports how many names the table holds.
func (t *nameTable) len() int { return len(t.byModel) }
