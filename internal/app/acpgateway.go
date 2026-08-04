package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/looprig/acp/launch"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/codec"
	"github.com/looprig/inference/codec/anthropicapi"
	"github.com/looprig/inference/codec/openairesponses"
	"github.com/looprig/inference/gateway"
	model "github.com/looprig/inference/model"
)

// ACPGateway owns one child-specific, strict loopback gateway. The server is
// deliberately not shared between children: its route table includes the
// selected effort and therefore is part of that child's immutable runtime.
type ACPGateway struct {
	server  *gateway.Server
	binding launch.ProxyBinding
	plan    acpGatewayPlan
}

// NewACPGateway starts the fixed gateway for a gateway-backed child. Native
// authentication has no proxy binding and returns (nil, nil).
func NewACPGateway(ctx context.Context, catalog ACPCompiledCatalog, resolved loop.Resolved) (*ACPGateway, error) {
	if resolved.Credential == loop.CredentialNativeAuth {
		return nil, nil
	}
	plan, err := buildACPGatewayPlan(catalog, resolved)
	if err != nil {
		return nil, err
	}
	handler, err := gateway.New(gateway.Config{
		Resolver:     plan.resolver,
		Codecs:       plan.codecs,
		Authenticate: allowInnerGatewayAuth{},
	})
	if err != nil {
		return nil, fmt.Errorf("coderig: build ACP gateway: %w", err)
	}
	server, err := gateway.NewServer(gateway.ServerConfig{Handler: handler})
	if err != nil {
		return nil, fmt.Errorf("coderig: create ACP gateway server: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := server.Start(ctx); err != nil {
		_ = server.Close(context.Background())
		return nil, fmt.Errorf("coderig: start ACP gateway: %w", err)
	}
	baseURL, token, ready := server.Binding()
	if !ready || baseURL == "" || token == "" {
		_ = server.Close(context.Background())
		return nil, fmt.Errorf("coderig: ACP gateway did not become ready")
	}
	return &ACPGateway{server: server, binding: launch.ProxyBinding{BaseURL: baseURL, Token: token}, plan: plan}, nil
}

// Binding returns the launch-owned proxy data for this child. The returned
// value contains the loopback address and bearer token only; it is not part of
// model-facing runtime identity.
func (g *ACPGateway) Binding() launch.ProxyBinding {
	if g == nil {
		return launch.ProxyBinding{}
	}
	return g.binding
}

// Close is idempotent and delegates ownership to gateway.Server.
func (g *ACPGateway) Close(ctx context.Context) error {
	if g == nil || g.server == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return g.server.Close(ctx)
}

type acpGatewayPlan struct {
	resolver gateway.Resolver
	codecs   map[model.APIFormat]codec.ServerCodec
	routes   map[gateway.RouteKey]gateway.Target
}

func buildACPGatewayPlan(catalog ACPCompiledCatalog, resolved loop.Resolved) (acpGatewayPlan, error) {
	if resolved.Credential != loop.CredentialGatewayBacked {
		return acpGatewayPlan{}, fmt.Errorf("coderig: native-auth runtime has no gateway")
	}
	ingress, err := acpGatewayIngress(resolved.AgentHarness)
	if err != nil {
		return acpGatewayPlan{}, err
	}
	mainTarget, err := catalog.GatewayTarget(resolved)
	if err != nil {
		return acpGatewayPlan{}, err
	}
	mainAlias := resolved.TargetAlias
	if mainAlias == "" {
		mainAlias = resolved.ModelAlias
	}
	routes := map[gateway.RouteKey]gateway.Target{{Ingress: ingress, Model: string(mainAlias)}: mainTarget}
	if resolved.AgentHarness == "claude-code" && resolved.SmallModel != "" {
		smallResolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(
			resolved.AgentType, resolved.AgentHarness, resolved.SmallModel, model.EffortNone, false,
		)
		if err != nil {
			return acpGatewayPlan{}, fmt.Errorf("coderig: resolve ACP small model: %w", err)
		}
		smallTarget, err := catalog.GatewayTarget(smallResolved)
		if err != nil {
			return acpGatewayPlan{}, err
		}
		smallAlias := smallResolved.TargetAlias
		if smallAlias == "" {
			smallAlias = smallResolved.ModelAlias
		}
		if smallAlias != mainAlias {
			routes[gateway.RouteKey{Ingress: ingress, Model: string(smallAlias)}] = smallTarget
		}
	}
	return finishACPGatewayPlan(resolved.AgentHarness, ingress, routes)
}

func finishACPGatewayPlan(harness loop.AgentHarnessName, ingress model.APIFormat, routes map[gateway.RouteKey]gateway.Target) (acpGatewayPlan, error) {
	mux, err := gateway.NewMux(gateway.Mux{Routes: routes})
	if err != nil {
		return acpGatewayPlan{}, err
	}
	return acpGatewayPlan{
		resolver: gateway.Strict(mux),
		codecs:   map[model.APIFormat]codec.ServerCodec{ingress: acpGatewayCodec(harness)},
		routes:   routes,
	}, nil
}

func acpGatewayIngress(harness loop.AgentHarnessName) (model.APIFormat, error) {
	switch harness {
	case "claude-code":
		return model.APIFormatAnthropic, nil
	case "codex":
		return model.APIFormatOpenAIResponses, nil
	default:
		return "", fmt.Errorf("coderig: unsupported ACP harness")
	}
}

func acpGatewayCodec(harness loop.AgentHarnessName) codec.ServerCodec {
	if harness == "claude-code" {
		return anthropicapi.Codec{}
	}
	return openairesponses.Codec{}
}

// Server authentication is the outer trust boundary and generates a fresh
// token per child. gateway.New also requires an Authenticator for its handler;
// this private pass-through exists only inside that already-authenticated
// Server wrapper and never crosses the child process boundary.
type allowInnerGatewayAuth struct{}

func (allowInnerGatewayAuth) Authenticate(*http.Request) error { return nil }
