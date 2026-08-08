package app

import (
	"sort"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
)

// ACPCatalogInput and CompileACPCatalog keep lower-level ACP composition
// fixtures focused on gateway/native behavior while the product compiler uses
// the raw-entry helper directly. They are test-only because ACP is no longer
// an independently defaulted product catalog.
type ACPCatalogInput struct {
	AgentTypes     []identity.AgentName
	GatewayTargets []ACPGatewaySource
	ClaudeSmall    loop.ModelAlias
	NativeACP      map[string]ACPNativeProfile
	NativeProfiles []ACPNativeProfile
	NativeAuth     []ACPNativeAuthSource
}

func CompileACPCatalog(input ACPCatalogInput) (ACPCompiledCatalog, error) {
	raw, err := compileACPRuntimeEntries(acpCatalogInput{
		GatewayTargets: input.GatewayTargets,
		ClaudeSmall:    input.ClaudeSmall,
		NativeACP:      input.NativeACP,
		NativeProfiles: input.NativeProfiles,
		NativeAuth:     input.NativeAuth,
	})
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	roles := append([]identity.AgentName(nil), input.AgentTypes...)
	if len(roles) == 0 {
		roles = []identity.AgentName{"planner", "builder", "reviewer"}
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	entries := make([]loop.RuntimeCatalogEntry, 0, len(roles)*len(raw.entries))
	for _, role := range roles {
		for index, source := range raw.entries {
			entry := cloneACPEntry(source)
			entry.AgentType = role
			entry.Default = index == 0
			entries = append(entries, entry)
		}
	}
	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	return ACPCompiledCatalog{
		RuntimeCatalog: catalog,
		gatewayTargets: cloneGatewayTargets(raw.gatewayTargets),
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
}

func mustEmptyRuntimeCatalog() loop.RuntimeCatalog {
	catalog, _ := loop.NewRuntimeCatalog(nil)
	return catalog
}
