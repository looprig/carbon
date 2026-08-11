package sessionruntime

import (
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
)

// Topology is the immutable set of loop definitions and roots captured by a Lifecycle.
// Callers must pass already-validated, uniquely named definitions.
type Topology struct {
	Definitions  []loop.Definition
	Primers      []identity.AgentName
	ActivePrimer identity.AgentName
}

func cloneTopology(in Topology) Topology {
	return Topology{
		Definitions:  append([]loop.Definition(nil), in.Definitions...),
		Primers:      append([]identity.AgentName(nil), in.Primers...),
		ActivePrimer: in.ActivePrimer,
	}
}

func (t Topology) definition(name identity.AgentName) (loop.Definition, bool) {
	for _, definition := range t.Definitions {
		if definition.Name() == name {
			return definition, true
		}
	}
	return loop.Definition{}, false
}
