package loopruntime

import (
	"sort"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
)

// externalGeneration is ONE source's currently installed external toolset. tools are
// the live built instances the next turn will run; identities is the durable identity
// projection that was journaled when this generation was installed (kept so a later
// replacement from a DIFFERENT source can be checked for name collisions without
// re-describing live tools on the actor goroutine, which may perform I/O).
type externalGeneration struct {
	generation string
	tools      []tool.InvokableTool
	identities []event.ExternalToolIdentity
}

// externalSlots is the actor-owned external tool slot, keyed by source. It is mutated
// ONLY by the actor (no locks), exactly like effectiveConfig. It starts EMPTY on both
// New and NewRestored: external tools are live resources (an MCP connection cannot be
// rebuilt from journal bytes), so a restored loop comes up with nothing installed and
// the composing application re-installs. That is why the durable
// LoopExternalToolsetChanged is never folded back into this map at restore.
type externalSlots map[string]externalGeneration

// clone returns a shallow copy of the slot map. applyReplaceExternalTools mutates the
// COPY and installs it only after the durable append succeeds, so a failed append
// cannot leave a half-swapped slot behind.
func (s externalSlots) clone() externalSlots {
	out := make(externalSlots, len(s))
	for name, generation := range s {
		out[name] = generation
	}
	return out
}

// sources returns the slot's source names in deterministic (sorted) order. Composition
// order feeds the model-facing tool list, and a map's iteration order is randomized —
// without this the toolset advertised to the model would differ run to run, which would
// make prompt caching and any request digest unstable.
func (s externalSlots) sources() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// collides reports the first tool name in identities that is already installed by a
// source OTHER than the replacing one. The replaced source's own generation is skipped:
// replacing a slot with a toolset that reuses its own names is the normal case, not a
// collision.
func (s externalSlots) collides(replacing string, identities []event.ExternalToolIdentity) (string, bool) {
	for _, source := range s.sources() {
		if source == replacing {
			continue
		}
		installed := make(map[string]struct{}, len(s[source].identities))
		for _, id := range s[source].identities {
			installed[id.Name] = struct{}{}
		}
		for _, id := range identities {
			if _, exists := installed[id.Name]; exists {
				return id.Name, true
			}
		}
	}
	return "", false
}

// externalTools returns every installed source's tools, concatenated in sorted-source
// order. The returned slice is freshly allocated, so the caller may hand it to a turn
// without aliasing actor state.
func (s externalSlots) externalTools() []tool.InvokableTool {
	var out []tool.InvokableTool
	for _, source := range s.sources() {
		out = append(out, s[source].tools...)
	}
	return out
}

// composeRegistry builds a turn-ready registry: the mode's immutable DECLARED tools
// first, then the external slot's tools. Declared tools lead so the definition's own
// toolset keeps a stable position in the model-facing list regardless of what is
// installed externally.
//
// It always allocates a new backing array rather than appending onto declared: the
// declared slice comes from BoundMode.Tools via cloneBoundMode, and appending in place
// could write into a shared array. Shadowing is impossible here by construction — every
// caller has already refused a replacement whose names collide with a declared tool —
// so this is a pure concatenation, not a merge.
func composeRegistry(declared []tool.InvokableTool, slots externalSlots) []tool.InvokableTool {
	external := slots.externalTools()
	if len(external) == 0 {
		return append([]tool.InvokableTool(nil), declared...)
	}
	out := make([]tool.InvokableTool, 0, len(declared)+len(external))
	out = append(out, declared...)
	out = append(out, external...)
	return out
}
