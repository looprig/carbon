package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/acp/launch"
	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	acpdriver "github.com/looprig/foreignloops/driver/acp"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
)

var errACPChildUnavailable = errors.New("coderig: ACP child unavailable")

// boundedACPChildError is the model-facing error boundary for ACP startup and
// restore. ACP launch/RPC/stdio errors can contain executable paths, login
// locations, URLs, provider messages, or stderr; none of those details belong
// in a Subagent result or durable Harness error. Keep cancellation recognizable
// for controller shutdown, but collapse every other cause to one fixed result.
func boundedACPChildError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errACPChildUnavailable
}

// ACPChildrenConfig is the composition-root input for delegated ACP loops.
// Executable paths are preflighted before a profile is registered. Env is
// reduced to EnvAllowlist before it reaches the child process.
type ACPChildrenConfig struct {
	Catalog       ACPCompiledCatalog
	Executables   map[loop.AgentHarnessName]string
	WorkspaceRoot string
	Env           []string
	// EnvAllowlist is the compatibility fallback for callers that predate
	// credential-specific allowlists. Production supplies both mode-specific
	// lists below.
	EnvAllowlist        []string
	NativeEnvAllowlist  []string
	GatewayEnvAllowlist []string
	// executablePreflight is an internal test seam. Production leaves it nil,
	// which performs a bounded ACP initialize/session probe before advertising
	// a profile; focused composition tests can replace the process probe with a
	// deterministic result.
	executablePreflight func(context.Context, ACPNativeAuthProbe) bool
}

// ACPComposition is the immutable CodeRig-to-Harness bridge for ACP children.
// The registry is retained for inspection and the function pair is the narrow
// legacy rig option; dispatch still selects by bound RuntimeProfile.
type ACPComposition struct {
	Catalog  ACPCompiledCatalog
	Registry *foreign.BuilderRegistry
	Live     foreign.Builder
	Restored foreign.RestoredBuilder
}

// NewACPComposition preflights configured executable paths, registers only
// cataloged ACP profiles, and returns a registry-backed builder pair. Missing
// executables simply omit that harness; native primers remain usable.
func NewACPComposition(config ACPChildrenConfig) (*ACPComposition, error) {
	if (config.Catalog.HasProfile("acp/claude-code") || config.Catalog.HasProfile("acp/codex")) && !cleanAbsolutePath(config.WorkspaceRoot) {
		return nil, fmt.Errorf("coderig: ACP workspace root must be a clean absolute path")
	}
	registry := new(foreign.BuilderRegistry)
	available := make(map[loop.RuntimeProfileName]struct{})
	for _, profile := range []loop.RuntimeProfileName{"acp/claude-code", "acp/codex"} {
		if !config.Catalog.HasProfile(profile) {
			continue
		}
		harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
		probe := ACPNativeAuthProbe{
			Harness:       harness,
			Executable:    config.Executables[harness],
			WorkspaceRoot: config.WorkspaceRoot,
			Env:           filterACPEnv(config.Env, acpNativeAuthEnvAllowlist),
		}
		if !preflightACPExecutable(probe.Executable) {
			continue
		}
		preflight := config.executablePreflight
		if preflight == nil {
			preflight = preflightProductionACPExecutable
		}
		if !preflight(context.Background(), probe) {
			continue
		}
		available[profile] = struct{}{}
	}
	filtered, err := config.Catalog.filterProfiles(available)
	if err != nil {
		return nil, err
	}
	config.Catalog = filtered
	factory := &acpChildFactory{config: config}
	for _, profile := range []loop.RuntimeProfileName{"acp/claude-code", "acp/codex"} {
		if !config.Catalog.HasProfile(profile) {
			continue
		}
		if err := registry.Register(profile, factory.live, factory.restored); err != nil {
			return nil, err
		}
	}
	return &ACPComposition{
		Catalog:  config.Catalog,
		Registry: registry,
		Live:     dispatchACPBuilder(registry),
		Restored: dispatchACPRestoredBuilder(registry),
	}, nil
}

func preflightACPExecutable(path string) bool {
	if !cleanAbsolutePath(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0111 != 0
}

type acpChildFactory struct {
	config ACPChildrenConfig
}

func (f *acpChildFactory) live(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
) (loop.Backend, string, error) {
	_, acpConfig, ownedGateway, err := f.configFor(loopCtx, cfg, "")
	if err != nil {
		return nil, "", boundedACPChildError(err)
	}
	backend, sid, err := acpdriver.BuildWith(acpConfig)(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac)
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return nil, "", boundedACPChildError(err)
	}
	if backend == nil {
		_ = ownedGateway.Close(context.Background())
		return nil, "", boundedACPChildError(errors.New("coderig: ACP builder returned no backend"))
	}
	return wrapACPGatewayBackend(backend, ownedGateway), sid, nil
}

func (f *acpChildFactory) restored(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed foreign.RestoredForeign,
) (loop.Backend, error) {
	_, acpConfig, ownedGateway, err := f.configFor(loopCtx, cfg, seed.AgentSessionID)
	if err != nil {
		return nil, boundedACPChildError(err)
	}
	backend, err := acpdriver.BuildRestoredWith(acpConfig)(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed)
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return nil, boundedACPChildError(err)
	}
	if backend == nil {
		_ = ownedGateway.Close(context.Background())
		return nil, boundedACPChildError(errors.New("coderig: ACP restored builder returned no backend"))
	}
	return wrapACPGatewayBackend(backend, ownedGateway), nil
}

func (f *acpChildFactory) configFor(ctx context.Context, cfg loop.BoundDefinition, _ string) (loop.Resolved, acpdriver.Config, *ACPGateway, error) {
	resolved, harness, err := resolveACPBoundRuntime(f.config.Catalog, cfg)
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	posture, err := acpPostureFor(string(cfg.Name()))
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	ownedGateway, err := NewACPGateway(ctx, f.config.Catalog, resolved)
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	binding := launch.ProxyBinding{}
	if ownedGateway != nil {
		binding = ownedGateway.Binding()
	}
	modelAlias := string(resolved.ModelAlias)
	smallModelAlias := string(resolved.SmallModel)
	if resolved.Credential == loop.CredentialNativeAuth {
		if resolved.Target.Name == "" {
			return loop.Resolved{}, acpdriver.Config{}, nil, fmt.Errorf("coderig: native ACP model unavailable")
		}
		modelAlias = resolved.Target.Name
		smallModelAlias = resolved.NativeSmallModel
		if harness == "claude-code" && smallModelAlias == "" {
			smallModelAlias = resolved.Target.Name
		}
	}
	return resolved, acpdriver.Config{
		Harness:         acpdriver.Harness(harness),
		Executable:      f.config.Executables[harness],
		Env:             f.config.envForCredential(resolved.Credential),
		Credential:      resolved.Credential,
		Binding:         binding,
		ModelAlias:      modelAlias,
		SmallModelAlias: smallModelAlias,
		Posture:         posture,
		WorkspaceRoot:   f.config.WorkspaceRoot,
	}, ownedGateway, nil
}

func (c ACPChildrenConfig) envForCredential(credential loop.CredentialMode) []string {
	allowlist := c.GatewayEnvAllowlist
	if credential == loop.CredentialNativeAuth {
		allowlist = c.NativeEnvAllowlist
	}
	if len(allowlist) == 0 {
		allowlist = c.EnvAllowlist
	}
	if credential == loop.CredentialGatewayBacked {
		// Even legacy callers that supply only EnvAllowlist must not be able
		// to pass harness login locations to a gateway-backed child.
		allowlist = intersectEnvAllowlists(allowlist, acpGatewayEnvAllowlist)
	}
	return filterACPEnv(c.Env, allowlist)
}

func intersectEnvAllowlists(left, right []string) []string {
	allowed := make(map[string]struct{}, len(right))
	for _, name := range right {
		allowed[name] = struct{}{}
	}
	result := make([]string, 0, len(left))
	for _, name := range left {
		if _, ok := allowed[name]; ok {
			result = append(result, name)
		}
	}
	return result
}

func resolveACPBoundRuntime(catalog ACPCompiledCatalog, cfg loop.BoundDefinition) (loop.Resolved, loop.AgentHarnessName, error) {
	identity := cfg.RuntimeIdentity()
	profile := cfg.RuntimeProfile()
	if profile == "" || identity.ModelAlias == "" || !catalog.HasProfile(profile) {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
	resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(cfg.Name(), harness, identity.ModelAlias, identity.Effort, true)
	if err != nil || resolved.Profile != profile {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	return resolved, harness, nil
}

func acpPostureFor(role string) (driver.Posture, error) {
	switch role {
	case "planner", "reviewer":
		return driver.PostureReadOnly, nil
	case "builder":
		return driver.PostureWorkspaceWrite, nil
	default:
		return "", fmt.Errorf("coderig: unsupported ACP role posture")
	}
}

func filterACPEnv(env, allowlist []string) []string {
	if len(allowlist) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allowed[name] = struct{}{}
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := allowed[name]; ok {
			result = append(result, entry)
		}
	}
	return result
}

func dispatchACPBuilder(registry *foreign.BuilderRegistry) foreign.Builder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, pub foreign.EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error), fac *event.Factory) (loop.Backend, string, error) {
		builder, _, err := registry.Builder(cfg.RuntimeProfile())
		if err != nil {
			return nil, "", err
		}
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac)
	}
}

func dispatchACPRestoredBuilder(registry *foreign.BuilderRegistry) foreign.RestoredBuilder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, pub foreign.EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error), fac *event.Factory, seed foreign.RestoredForeign) (loop.Backend, error) {
		_, builder, err := registry.Builder(cfg.RuntimeProfile())
		if err != nil {
			return nil, err
		}
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed)
	}
}

type acpGatewayBackend struct {
	loop.Backend
	done <-chan struct{}
}

func wrapACPGatewayBackend(backend loop.Backend, ownedGateway *ACPGateway) loop.Backend {
	if ownedGateway == nil {
		return backend
	}
	done := make(chan struct{})
	go func() {
		<-backend.DoneChan()
		_ = ownedGateway.Close(context.Background())
		close(done)
	}()
	return &acpGatewayBackend{Backend: backend, done: done}
}

func (b *acpGatewayBackend) DoneChan() <-chan struct{} { return b.done }

var _ foreign.Builder = (*acpChildFactory)(nil).live
var _ foreign.RestoredBuilder = (*acpChildFactory)(nil).restored
var _ loop.Backend = (*acpGatewayBackend)(nil)

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}
