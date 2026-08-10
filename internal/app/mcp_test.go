package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/session"
	mcpharness "github.com/looprig/mcp/pkg/harness"
)

// mcpTestSecret stands in for a header or env value across this file's
// tables; every failing-case assertion checks it never appears in a
// rendered error, matching mcpconfig_test.go's own "poisoned fixture"
// pattern.
const mcpTestSecret = "test-secret-do-not-log"

// mcpTestLoopID is a fresh, non-zero Loop identity used only to exercise
// LoopSelector.Permits, which matches a Named() selector on name alone and
// ignores loopID entirely -- any non-colliding value works here.
func mcpTestLoopID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

func TestMCPDefinitionsStdioHappyPath(t *testing.T) {
	spec := mcpServerSpec{name: "sh", kind: "stdio", command: "/bin/sh"}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 1", len(bindings))
	}

	binding := bindings[0]
	if binding.Name != "sh" {
		t.Errorf("binding.Name = %q, want sh", binding.Name)
	}
	if binding.Scope != mcpharness.ScopeSession {
		t.Errorf("binding.Scope = %v, want ScopeSession", binding.Scope)
	}
	if binding.Required {
		t.Errorf("binding.Required = true, want false")
	}
	if binding.Server.Transport == nil || binding.Server.Transport.Kind() != "stdio" {
		t.Errorf("binding.Server.Transport.Kind() = %v, want stdio", binding.Server.Transport)
	}
	if err := binding.Validate(); err != nil {
		t.Errorf("binding.Validate() error = %v", err)
	}

	loopID := mcpTestLoopID(t)
	if !binding.Visibility.Permits(loopID, "carbon") {
		t.Errorf("binding.Visibility.Permits(_, generic) = false, want true (roles empty -> generic)")
	}
	// Removed names and an unknown name are rejection fixtures; only Carbon
	// visibility is accepted.
	for _, role := range []string{"planner", "builder", "reviewer", "not-a-role"} {
		if binding.Visibility.Permits(loopID, role) {
			t.Errorf("binding.Visibility.Permits(_, %q) = true, want false", role)
		}
	}
}

func TestMCPDefinitionsStdioMissingCommandFailsClosed(t *testing.T) {
	spec := mcpServerSpec{name: "ghost", kind: "stdio", command: "definitely-not-a-command-xyzzy"}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err == nil {
		t.Fatal("mcpDefinitions() error = nil, want error")
	}
	if bindings != nil {
		t.Errorf("mcpDefinitions() bindings = %v, want nil on error", bindings)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("mcpDefinitions() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "ghost" {
		t.Errorf("MCPConfigError.Binding = %q, want ghost", configErr.Binding)
	}
}

func TestMCPDefinitionsVisibilityDefaultsToCarbonWhenEmpty(t *testing.T) {
	spec := mcpServerSpec{name: "sh", kind: "stdio", command: "/bin/sh"} // roles nil

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	binding := bindings[0]
	loopID := mcpTestLoopID(t)
	if !binding.Visibility.Permits(loopID, "carbon") {
		t.Errorf("empty roles: Permits(_, generic) = false, want true")
	}
	// Legacy role names remain explicit hidden-visibility fixtures.
	for _, role := range []string{"planner", "builder", "reviewer"} {
		if binding.Visibility.Permits(loopID, role) {
			t.Errorf("empty roles: Permits(_, %q) = true, want false", role)
		}
	}
}

func TestMCPDefinitionsVisibilityHonorsExplicitCarbonRole(t *testing.T) {
	spec := mcpServerSpec{
		name:    "sh",
		kind:    "stdio",
		command: "/bin/sh",
		roles:   []string{"carbon"},
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	binding := bindings[0]
	loopID := mcpTestLoopID(t)
	if !binding.Visibility.Permits(loopID, "carbon") {
		t.Errorf("explicit roles: Permits(_, generic) = false, want true")
	}
	// Legacy role names remain explicit hidden-visibility fixtures.
	for _, role := range []string{"planner", "builder", "reviewer"} {
		if binding.Visibility.Permits(loopID, role) {
			t.Errorf("explicit roles [generic]: Permits(_, %q) = true, want false", role)
		}
	}
}

func TestMCPDefinitionsHTTPHappyPath(t *testing.T) {
	spec := mcpServerSpec{
		name:    "context7",
		kind:    "http",
		url:     "https://mcp.example.test/mcp",
		headers: map[string]string{"Authorization": mcpTestSecret},
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 1", len(bindings))
	}
	binding := bindings[0]
	if binding.Server.Transport == nil || binding.Server.Transport.Kind() != "streamablehttp" {
		t.Errorf("binding.Server.Transport.Kind() = %v, want streamablehttp", binding.Server.Transport)
	}
	if err := binding.Validate(); err != nil {
		t.Errorf("binding.Validate() error = %v", err)
	}
}

func TestMCPDefinitionsHTTPBrokenHeaderNeverLeaksValue(t *testing.T) {
	spec := mcpServerSpec{
		name:    "context7",
		kind:    "http",
		url:     "https://mcp.example.test/mcp",
		headers: map[string]string{"Bad Header": mcpTestSecret}, // space is illegal in a header name
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err == nil {
		t.Fatal("mcpDefinitions() error = nil, want error (invalid header name)")
	}
	if bindings != nil {
		t.Errorf("mcpDefinitions() bindings = %v, want nil on error", bindings)
	}
	if strings.Contains(err.Error(), mcpTestSecret) {
		t.Errorf("mcpDefinitions() error leaked header secret: %v", err)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("mcpDefinitions() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "context7" {
		t.Errorf("MCPConfigError.Binding = %q, want context7", configErr.Binding)
	}
}

func TestMCPDefinitionsSSEHappyPath(t *testing.T) {
	spec := mcpServerSpec{
		name: "events",
		kind: "sse",
		url:  "https://mcp.example.test/sse",
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 1", len(bindings))
	}
	binding := bindings[0]
	if binding.Server.Transport == nil || binding.Server.Transport.Kind() != "sse" {
		t.Errorf("binding.Server.Transport.Kind() = %v, want sse", binding.Server.Transport)
	}
	if err := binding.Validate(); err != nil {
		t.Errorf("binding.Validate() error = %v", err)
	}
}

// TestMCPDefinitionsMultipleSpecsPreserveOrder proves mcpDefinitions returns
// bindings in the same order and count as the input specs across all three
// transport kinds in one call.
func TestMCPDefinitionsMultipleSpecsPreserveOrder(t *testing.T) {
	specs := []mcpServerSpec{
		{name: "alpha", kind: "stdio", command: "/bin/sh"},
		{name: "beta", kind: "http", url: "https://mcp.example.test/mcp"},
		{name: "gamma", kind: "sse", url: "https://mcp.example.test/sse"},
	}

	bindings, err := mcpDefinitions(specs)
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 3 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 3", len(bindings))
	}
	wantNames := []string{"alpha", "beta", "gamma"}
	for i, want := range wantNames {
		if bindings[i].Name != want {
			t.Errorf("bindings[%d].Name = %q, want %q", i, bindings[i].Name, want)
		}
	}
}

// TestMCPDefinitionsAbortsBatchOnFirstFailure proves a construction failure
// on one spec in a multi-spec batch fails the whole call closed: no partial
// slice, and the other (valid) specs' construction is not silently
// swallowed or reordered around the bad one.
func TestMCPDefinitionsAbortsBatchOnFirstFailure(t *testing.T) {
	specs := []mcpServerSpec{
		{name: "good-one", kind: "stdio", command: "/bin/sh"},
		{name: "bad", kind: "stdio", command: "definitely-not-a-command-xyzzy"},
		{name: "good-two", kind: "http", url: "https://mcp.example.test/mcp"},
	}

	bindings, err := mcpDefinitions(specs)
	if err == nil {
		t.Fatal("mcpDefinitions() error = nil, want error")
	}
	if bindings != nil {
		t.Errorf("mcpDefinitions() bindings = %v, want nil (fail closed, no partial slice)", bindings)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("mcpDefinitions() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "bad" {
		t.Errorf("MCPConfigError.Binding = %q, want bad", configErr.Binding)
	}
}

// TestMCPEnvVarsFromIsSortedByName proves the env-allowlist helper produces
// a deterministic order regardless of Go's randomized map iteration: the
// same map content always yields the same []stdio.Var slice, sorted by
// Name.
func TestMCPEnvVarsFromIsSortedByName(t *testing.T) {
	env := map[string]string{
		"ZETA":  "1",
		"ALPHA": "2",
		"MU":    mcpTestSecret,
	}

	for i := 0; i < 5; i++ {
		vars := mcpEnvVarsFrom(env)
		if len(vars) != 3 {
			t.Fatalf("mcpEnvVarsFrom() len = %d, want 3", len(vars))
		}
		wantNames := []string{"ALPHA", "MU", "ZETA"}
		for j, want := range wantNames {
			if vars[j].Name != want {
				t.Fatalf("run %d: vars[%d].Name = %q, want %q (want sorted order)", i, j, vars[j].Name, want)
			}
		}
		if vars[1].Value != mcpTestSecret {
			t.Errorf("vars[1].Value = %q, want secret preserved", vars[1].Value)
		}
	}
}

func TestMCPEnvVarsFromEmptyIsNil(t *testing.T) {
	if got := mcpEnvVarsFrom(nil); got != nil {
		t.Errorf("mcpEnvVarsFrom(nil) = %v, want nil", got)
	}
	if got := mcpEnvVarsFrom(map[string]string{}); got != nil {
		t.Errorf("mcpEnvVarsFrom({}) = %v, want nil", got)
	}
}

// TestMCPHeadersFromIsSortedByName mirrors TestMCPEnvVarsFromIsSortedByName
// for the header helper: same determinism concern, same proof.
func TestMCPHeadersFromIsSortedByName(t *testing.T) {
	headers := map[string]string{
		"X-Zeta":        "1",
		"Authorization": mcpTestSecret,
		"X-Alpha":       "2",
	}

	for i := 0; i < 5; i++ {
		got := mcpHeadersFrom(headers)
		if len(got) != 3 {
			t.Fatalf("mcpHeadersFrom() len = %d, want 3", len(got))
		}
		wantNames := []string{"Authorization", "X-Alpha", "X-Zeta"}
		for j, want := range wantNames {
			if got[j].Name() != want {
				t.Fatalf("run %d: got[%d].Name() = %q, want %q (want sorted order)", i, j, got[j].Name(), want)
			}
		}
		if got[0].Value() != mcpTestSecret {
			t.Errorf("got[0].Value() = %q, want secret preserved", got[0].Value())
		}
	}
}

func TestMCPHeadersFromEmptyIsNil(t *testing.T) {
	if got := mcpHeadersFrom(nil); got != nil {
		t.Errorf("mcpHeadersFrom(nil) = %v, want nil", got)
	}
	if got := mcpHeadersFrom(map[string]string{}); got != nil {
		t.Errorf("mcpHeadersFrom({}) = %v, want nil", got)
	}
}

func TestMCPDefinitionsEmptySpecsReturnsEmpty(t *testing.T) {
	bindings, err := mcpDefinitions(nil)
	if err != nil {
		t.Fatalf("mcpDefinitions(nil) error = %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("mcpDefinitions(nil) bindings = %v, want none", bindings)
	}
}

// fakeGateHostOpenCall is one recorded session.GateHost.OpenHostGate call.
type fakeGateHostOpenCall struct {
	loopID  uuid.UUID
	gate    gate.Gate
	payload gate.Payload
}

// fakeGateHostCloseCall is one recorded session.GateHost.CloseGate call.
type fakeGateHostCloseCall struct {
	id     gate.ID
	reason gate.CloseReason
}

// fakeGateHost is a minimal, in-memory session.GateHost test double. It
// records exactly what it was handed, so a test can assert the gate.Gate
// mcpGateOpener actually built rather than trusting the implementation's
// own claims about what it maps. Harness's own contract for OpenHostGate
// (pkg/rig/gate_host_test.go) is proven separately, in harness; this fake
// exists only to prove mcpGateOpener's mapping onto that contract, at the
// carbon layer.
type fakeGateHost struct {
	mu sync.Mutex

	openCalls []fakeGateHostOpenCall
	openID    gate.ID
	openErr   error

	awaitCalls []gate.ID
	answer     gate.Answer
	answerErr  error

	closeCalls []fakeGateHostCloseCall
	closeErr   error
}

func (f *fakeGateHost) OpenHostGate(_ context.Context, loopID uuid.UUID, g gate.Gate, payload gate.Payload) (gate.ID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls = append(f.openCalls, fakeGateHostOpenCall{loopID: loopID, gate: g, payload: payload})
	if f.openErr != nil {
		return gate.ID{}, f.openErr
	}
	return f.openID, nil
}

func (f *fakeGateHost) AwaitGateAnswer(_ context.Context, id gate.ID) (gate.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awaitCalls = append(f.awaitCalls, id)
	if f.answerErr != nil {
		return gate.Answer{}, f.answerErr
	}
	return f.answer, nil
}

func (f *fakeGateHost) CloseGate(_ context.Context, id gate.ID, reason gate.CloseReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls = append(f.closeCalls, fakeGateHostCloseCall{id: id, reason: reason})
	return f.closeErr
}

var _ session.GateHost = (*fakeGateHost)(nil)

// TestMCPGateOpenerUnboundRefuses is the unbound-opener half of this task's
// required trio: a fresh opener with no Bind call refuses every OpenGate
// with a typed error rather than panicking, blocking, or returning a zero
// value a caller could mistake for success.
func TestMCPGateOpenerUnboundRefuses(t *testing.T) {
	opener := &mcpGateOpener{}

	_, err := opener.OpenGate(context.Background(), mcpharness.GateRequest{
		Kind:    gate.KindForm,
		Binding: "unbound-binding",
	})
	if err == nil {
		t.Fatal("OpenGate() error = nil, want a typed refusal")
	}
	var unbound *mcpGateOpenerUnboundError
	if !errors.As(err, &unbound) {
		t.Fatalf("OpenGate() error = %T %v, want *mcpGateOpenerUnboundError", err, err)
	}
	if unbound.Binding != "unbound-binding" {
		t.Errorf("unbound.Binding = %q, want %q", unbound.Binding, "unbound-binding")
	}
}

// TestMCPGateOpenerHeadlessNeverBoundBehavesAsUnbound documents the
// connection between "unbound" and "headless" at this layer. assembly.go's
// mcpSessionAssembly.attach composes a headless session's Manager with a
// fresh &mcpGateOpener{} and simply never calls Bind on it -- there is no
// separate headless code path inside mcpGateOpener to exercise, because
// headlessness IS the permanent absence of a Bind call (design section
// 1.2.3: "Headless sessions install an always-refusing opener, matching
// the headless permission posture"). This test is therefore exactly the
// headless posture, exercised at the only layer that exists yet: a fresh
// opener, across more than one request kind, never bound, always refusing.
// mcp_integration_test.go's TestMCPSessionAssemblyAttachBindsGateHostOnlyWhenInteractive
// proves the same property at the wiring layer.
func TestMCPGateOpenerHeadlessNeverBoundBehavesAsUnbound(t *testing.T) {
	opener := &mcpGateOpener{}

	reqs := []mcpharness.GateRequest{
		{Kind: gate.KindForm, Binding: "server-a"},
		{Kind: gate.KindOpenURL, Binding: "server-b"},
	}
	for _, req := range reqs {
		_, err := opener.OpenGate(context.Background(), req)
		var unbound *mcpGateOpenerUnboundError
		if !errors.As(err, &unbound) {
			t.Fatalf("OpenGate(%+v) error = %T %v, want *mcpGateOpenerUnboundError", req, err, err)
		}
		if unbound.Binding != req.Binding {
			t.Errorf("unbound.Binding = %q, want %q", unbound.Binding, req.Binding)
		}
	}
}

// TestMCPGateOpenerBoundForwardsToHost is the faithful-mapping proof: once
// Bind installs a host, OpenGate must build the exact gate.Gate this
// package's doc comment on mcpGateOpener.OpenGate claims (Resolver,
// Blocks, Effect, Prompt, Restorable), forward loopID/payload unchanged,
// await the answer on the ID the host returned, and translate gate.Answer
// back into mcpharness.GateResponse.
func TestMCPGateOpenerBoundForwardsToHost(t *testing.T) {
	gateID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	loopID := mcpTestLoopID(t)
	schema := gate.PromptSchema{Fields: []gate.Field{
		{Name: "username", Label: "Username", Kind: gate.FieldText, Required: true},
	}}
	req := mcpharness.GateRequest{
		Kind:    gate.KindForm,
		Payload: gate.FormPayload{Title: "Sign in", Schema: schema},
		Prompt: gate.Prompt{
			Title:  "Sign in",
			Schema: schema,
			Controls: []gate.Control{
				{Action: gate.FormActionAccept, Label: "Submit"},
				{Action: gate.FormActionDecline, Label: "Decline"},
			},
		},
		Restorable: false,
		Binding:    "test-server",
		LoopID:     loopID,
	}

	host := &fakeGateHost{
		openID: gate.ID(gateID),
		answer: gate.Answer{
			GateID: gate.ID(gateID),
			Action: gate.FormActionAccept,
			Values: map[string]string{"username": mcpTestSecret},
			Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		},
	}

	opener := &mcpGateOpener{}
	opener.Bind(host)

	resp, err := opener.OpenGate(context.Background(), req)
	if err != nil {
		t.Fatalf("OpenGate() error = %v", err)
	}

	if len(host.openCalls) != 1 {
		t.Fatalf("OpenHostGate calls = %d, want 1", len(host.openCalls))
	}
	call := host.openCalls[0]
	if call.loopID != loopID {
		t.Errorf("OpenHostGate loopID = %v, want %v", call.loopID, loopID)
	}
	if call.gate.Kind != gate.KindForm {
		t.Errorf("gate.Kind = %q, want %q", call.gate.Kind, gate.KindForm)
	}
	if call.gate.Resolver != gate.ResolverSession {
		t.Errorf("gate.Resolver = %q, want %q (only legal value for GateHost)", call.gate.Resolver, gate.ResolverSession)
	}
	if call.gate.Blocks != gate.BlocksToolCall {
		t.Errorf("gate.Blocks = %q, want %q", call.gate.Blocks, gate.BlocksToolCall)
	}
	if call.gate.Effect != gate.EffectResume {
		t.Errorf("gate.Effect = %q, want %q", call.gate.Effect, gate.EffectResume)
	}
	if call.gate.Criticality != "" {
		t.Errorf("gate.Criticality = %q, want zero value", call.gate.Criticality)
	}
	if !reflect.DeepEqual(call.gate.ResponsePolicy, gate.ResponsePolicy{}) {
		t.Errorf("gate.ResponsePolicy = %+v, want zero value", call.gate.ResponsePolicy)
	}
	if !reflect.DeepEqual(call.gate.Subject, gate.Subject{}) {
		t.Errorf("gate.Subject = %+v, want zero value", call.gate.Subject)
	}
	if !reflect.DeepEqual(call.gate.Prompt, req.Prompt) {
		t.Errorf("gate.Prompt = %+v, want req.Prompt %+v", call.gate.Prompt, req.Prompt)
	}
	if call.gate.Restorable != req.Restorable {
		t.Errorf("gate.Restorable = %v, want %v", call.gate.Restorable, req.Restorable)
	}
	if !reflect.DeepEqual(call.payload, req.Payload) {
		t.Errorf("payload = %+v, want req.Payload %+v", call.payload, req.Payload)
	}

	if len(host.awaitCalls) != 1 || host.awaitCalls[0] != gate.ID(gateID) {
		t.Fatalf("AwaitGateAnswer calls = %v, want [%v]", host.awaitCalls, gateID)
	}

	if resp.Action != gate.FormActionAccept {
		t.Errorf("resp.Action = %q, want %q", resp.Action, gate.FormActionAccept)
	}
	if resp.Values["username"] != mcpTestSecret {
		t.Errorf("resp.Values[username] = %q, want the fake's answer preserved", resp.Values["username"])
	}
	if len(host.closeCalls) != 0 {
		t.Errorf("CloseGate calls = %d, want 0 (a successful answer needs no cleanup close)", len(host.closeCalls))
	}
}

// TestMCPGateOpenerAwaitFailureClosesTheGate proves the cleanup half of
// OpenGate's contract: a failure (including ctx cancellation, which
// AwaitGateAnswer surfaces the same way -- see session.GateHost's doc)
// withdraws the gate rather than leaving it open for a human to answer
// into a caller that already gave up.
func TestMCPGateOpenerAwaitFailureClosesTheGate(t *testing.T) {
	gateID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	host := &fakeGateHost{
		openID:    gate.ID(gateID),
		answerErr: context.Canceled,
	}

	opener := &mcpGateOpener{}
	opener.Bind(host)

	req := mcpharness.GateRequest{
		Kind:    gate.KindOpenURL,
		Payload: gate.OpenURLPayload{DisplayOrigin: "https://example.com", URL: "https://example.com/x"},
		Prompt:  gate.Prompt{Title: "Authorize access", Origin: "https://example.com"},
		Binding: "test-server",
	}
	_, err = opener.OpenGate(context.Background(), req)
	if err == nil {
		t.Fatal("OpenGate() error = nil, want the await failure surfaced")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("OpenGate() error = %v, want context.Canceled wrapped in it", err)
	}

	if len(host.closeCalls) != 1 {
		t.Fatalf("CloseGate calls = %d, want 1", len(host.closeCalls))
	}
	if host.closeCalls[0].id != gate.ID(gateID) {
		t.Errorf("CloseGate id = %v, want %v", host.closeCalls[0].id, gateID)
	}
	if host.closeCalls[0].reason != gate.CloseAbandoned {
		t.Errorf("CloseGate reason = %q, want %q", host.closeCalls[0].reason, gate.CloseAbandoned)
	}
}

// TestMCPNoticeRecorderReportStoresNotices proves the Reporter's basic
// contract: Report does not panic and does something observable -- here,
// storing the notice for later inspection (this task's design choice; see
// mcpNoticeRecorder's doc comment for why it is not a no-op or a
// late-binding forward).
func TestMCPNoticeRecorderReportStoresNotices(t *testing.T) {
	r := newMCPNoticeRecorder()
	loopID := mcpTestLoopID(t)

	r.Report(mcpharness.Notice{Kind: mcpharness.NoticeToolNameCollision, Binding: "a", LoopID: loopID, Message: "collision"})
	r.Report(mcpharness.Notice{Kind: mcpharness.NoticeAdopted, Binding: "b", Generation: 3, Message: "adopted"})

	got := r.Notices()
	if len(got) != 2 {
		t.Fatalf("Notices() len = %d, want 2", len(got))
	}
	if got[0].Kind != mcpharness.NoticeToolNameCollision || got[0].Binding != "a" || got[0].LoopID != loopID || got[0].Message != "collision" {
		t.Errorf("Notices()[0] = %+v, want the first reported notice preserved", got[0])
	}
	if got[1].Kind != mcpharness.NoticeAdopted || got[1].Binding != "b" || got[1].Generation != 3 || got[1].Message != "adopted" {
		t.Errorf("Notices()[1] = %+v, want the second reported notice preserved", got[1])
	}
	if r.Dropped() != 0 {
		t.Errorf("Dropped() = %d, want 0", r.Dropped())
	}
}

// TestMCPNoticeRecorderReportDoesNotPanicOnZeroNotice covers the boundary
// case: an unset mcpharness.Notice{} must still be stored, not rejected or
// panicked on -- Report has no validation to perform, only bounded
// storage.
func TestMCPNoticeRecorderReportDoesNotPanicOnZeroNotice(t *testing.T) {
	r := newMCPNoticeRecorder()
	r.Report(mcpharness.Notice{})
	if got := r.Notices(); len(got) != 1 {
		t.Fatalf("Notices() len = %d, want 1", len(got))
	}
}

// TestMCPNoticeRecorderBoundsBacklog proves the recorder cannot grow
// without limit: past maxMCPNoticeBacklog, the oldest retained notice is
// dropped (not the newest -- a Reporter that silently discarded fresh
// notices while keeping stale ones would mislead an operator reading it
// later) and Dropped() counts the eviction.
func TestMCPNoticeRecorderBoundsBacklog(t *testing.T) {
	r := newMCPNoticeRecorder()
	const over = 10
	for i := 0; i < maxMCPNoticeBacklog+over; i++ {
		r.Report(mcpharness.Notice{Kind: mcpharness.NoticeEventRejected, Message: fmt.Sprintf("n%d", i)})
	}

	got := r.Notices()
	if len(got) != maxMCPNoticeBacklog {
		t.Fatalf("Notices() len = %d, want %d", len(got), maxMCPNoticeBacklog)
	}
	if r.Dropped() != over {
		t.Errorf("Dropped() = %d, want %d", r.Dropped(), over)
	}
	if want := fmt.Sprintf("n%d", over); got[0].Message != want {
		t.Errorf("Notices()[0].Message = %q, want %q (oldest retained after eviction)", got[0].Message, want)
	}
	last := maxMCPNoticeBacklog + over - 1
	if want := fmt.Sprintf("n%d", last); got[len(got)-1].Message != want {
		t.Errorf("Notices()[last].Message = %q, want %q (newest report kept)", got[len(got)-1].Message, want)
	}
}

// TestMCPNoticeRecorderReportIsConcurrencySafe exercises the mutex under
// -race: mcpharness.Reporter.Report can be called "on the goroutine that
// discovered the fact" (mcp/pkg/harness/deps.go), which for a session with
// several live bindings means genuinely concurrent callers.
func TestMCPNoticeRecorderReportIsConcurrencySafe(t *testing.T) {
	r := newMCPNoticeRecorder()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			r.Report(mcpharness.Notice{Kind: mcpharness.NoticeEventRejected, Message: fmt.Sprintf("n%d", i)})
		}(i)
	}
	wg.Wait()

	if got := len(r.Notices()); got != n {
		t.Fatalf("Notices() len = %d, want %d", got, n)
	}
	if r.Dropped() != 0 {
		t.Errorf("Dropped() = %d, want 0", r.Dropped())
	}
}

// fakeMCPEventTarget is a minimal mcpharness.EventPublisher test double that
// records what it was handed and can be scripted to fail, so a test can
// prove mcpEventPublisher forwards both the event and the target's error
// unchanged rather than swallowing either.
type fakeMCPEventTarget struct {
	mu    sync.Mutex
	calls []event.Event
	err   error
}

func (f *fakeMCPEventTarget) PublishEvent(_ context.Context, ev event.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ev)
	return f.err
}

var _ mcpharness.EventPublisher = (*fakeMCPEventTarget)(nil)

// TestMCPEventPublisherUnboundDropsSilently is the "no session yet" half of
// this task's required trio: PublishEvent before Bind must not error and
// must not panic -- it drops, matching attach.go's own documented window
// where a status has nowhere to go yet (see mcpEventPublisher's doc for why
// that costs nothing durable).
func TestMCPEventPublisherUnboundDropsSilently(t *testing.T) {
	p := &mcpEventPublisher{}
	if err := p.PublishEvent(context.Background(), event.SessionActive{}); err != nil {
		t.Fatalf("PublishEvent() before Bind error = %v, want nil (dropped)", err)
	}
}

// TestMCPEventPublisherBoundForwardsToTarget is the faithful-forwarding
// proof: once Bind installs a target, PublishEvent must hand it the exact
// event unchanged.
func TestMCPEventPublisherBoundForwardsToTarget(t *testing.T) {
	target := &fakeMCPEventTarget{}
	p := &mcpEventPublisher{}
	p.Bind(target)

	ev := event.SessionActive{}
	if err := p.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("PublishEvent() error = %v", err)
	}

	target.mu.Lock()
	calls := append([]event.Event(nil), target.calls...)
	target.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("target PublishEvent calls = %d, want 1", len(calls))
	}
	if !reflect.DeepEqual(calls[0], ev) {
		t.Errorf("forwarded event = %+v, want %+v", calls[0], ev)
	}
}

// TestMCPEventPublisherBoundSurfacesTargetError proves PublishEvent does not
// swallow the bound target's own error -- a caller relying on this to learn
// its publish failed must actually see that failure.
func TestMCPEventPublisherBoundSurfacesTargetError(t *testing.T) {
	wantErr := errors.New("publish failed")
	target := &fakeMCPEventTarget{err: wantErr}
	p := &mcpEventPublisher{}
	p.Bind(target)

	if err := p.PublishEvent(context.Background(), event.SessionActive{}); !errors.Is(err, wantErr) {
		t.Errorf("PublishEvent() error = %v, want %v", err, wantErr)
	}
}

// TestMCPEventPublisherBindIsConcurrencySafe exercises the mutex under
// -race: a concurrent PublishEvent must never observe a half-written target.
func TestMCPEventPublisherBindIsConcurrencySafe(t *testing.T) {
	target := &fakeMCPEventTarget{}
	p := &mcpEventPublisher{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p.Bind(target)
	}()
	go func() {
		defer wg.Done()
		_ = p.PublishEvent(context.Background(), event.SessionActive{})
	}()
	wg.Wait()
}
