//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/classifiers/pkg/commandsafety"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

// permission_review_integration_test.go is Task 24's end-to-end permission
// review coverage. It builds real CodeRig sessions (real toolsets.go
// access/gate wiring, a real confined sh -c executor, a real Bash tool call)
// with a real classifier registration built through
// newPermissionReviewRegistration — the exact composition production uses —
// and drives them through the sessionadapter.Adapter public API (Submit,
// Subscribe, RespondGate) exactly as the CLI/TUI does.
//
// IMPORTANT SCOPE NOTE, read before extending this file: at the time this
// task was implemented, Harness's per-turn permission-REVIEW-CONTEXT capture
// (internal/loopruntime/turn.go's turnConfig.reviewContext) is never
// populated by any production code path in
// github.com/looprig/harness-permission-classifier — confirmed by exhaustive
// repo-wide search, and independently re-verified. That field gates whether
// internal/sessionruntime.Session.StartPermissionReview ever calls into a
// registered classifier at all; with it unset, StartPermissionReview
// deliberately no-ops ("nothing to review" — internal/sessionruntime/
// review_adapter.go). This is a DIFFERENT, separate gap from the two
// documented addenda this task's brief describes (rig.WithPermissionReviewEvidence
// + pkg/gate/evidence.go), which only fixed the CONSTRUCTION-time blocker
// (a classifier-registered session previously failed 100% of the time at
// NewSession). No Harness test anywhere (including internal/sessionruntime's
// own "session-runtime layer" tests) exercises a real classifier inference
// round trip triggered by a genuine Submit() either — they all hand-build a
// gate.ReviewBasis and call the PRIVATE sessionruntime.Session.respondFromClassifier
// directly, a seam CodeRig cannot reach (unexported, on an unexported type).
//
// Consequently, a live classifier round trip literally cannot be triggered
// through CodeRig's public composition today. Every test below that would
// ideally prove "the classifier said X and CodeRig auto-approved" instead
// proves the CLOSEST fully-real equivalent CodeRig's public API allows:
//   - real session construction/start with a registered classifier + real
//     evidence wiring (the actual thing the two documented addenda unblock);
//   - CodeRig's OWN registered decision policy (via
//     gate.EvaluatePermissionAssessment, fed a real subject/assessment pair,
//     exactly Task 23's own permission_review_test.go pattern) correctly
//     computing eligible/ineligible for each required assessment shape;
//   - the REAL human-approval path (gate open -> RespondGate -> execution),
//     which is the SAME gate.Evaluator.Resolve/grant-mint path a classifier
//     response would also reach (harness's own
//     TestRespondFromClassifierMintsGrantsThroughEvaluatorLikeHuman proves
//     the classifier path and the human path converge on that one path) —
//     so proving CodeRig's composition reaches it via the human path also
//     proves the classifier path would, once wired.
//
// See this task's final report for a fuller account; this note exists so a
// future reader is not left to rediscover the gap independently.

// ---- fixtures: fake operator inference client (drives a real Bash call) --

// bashScript is a deterministic fake inference.Client driving ONE operator
// turn that calls Bash exactly once with command, then answers with a final
// text message once it observes the tool's own result text containing
// marker. It never branches on delegation (these scenarios never delegate),
// unlike managed_delegation_test.go's managedScript.
type bashScript struct {
	mu               sync.Mutex
	command          string
	marker           string
	calls            int
	observedToolText string
	// awaitingResult tracks THIS script's own call/response state machine
	// (rather than inferring "already ran" from marker text anywhere in the
	// accumulated conversation history) — a resubmitted turn's request still
	// carries the FULL prior conversation, including an earlier tool result
	// that already contains the same marker, so scanning all of history for
	// the marker would wrongly treat a brand-new turn as already complete.
	awaitingResult bool
}

func (s *bashScript) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("bashScript.Invoke not used")
}

func (s *bashScript) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	s.mu.Lock()
	s.calls++
	var chunks []content.Chunk
	if s.awaitingResult {
		toolText := lastToolText(req)
		s.observedToolText = toolText
		s.awaitingResult = false
		chunks = finalText("integration turn complete")
	} else {
		s.awaitingResult = true
		chunks = bashToolCall("bash-integration-1", s.command)
	}
	s.mu.Unlock()
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i == len(chunks) {
			return nil, io.EOF
		}
		c := chunks[i]
		i++
		return c, nil
	}, nil), nil
}

func bashToolCall(id, command string) []content.Chunk {
	args, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: command})
	return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: "Bash", InputJSON: string(args)}}
}

// ---- fixtures: fake classifier inference client --------------------------

// scriptedClassifierClient is a fake inference.Client shaped for
// commandsafety.New's Options.Inference: a real command-safety Hustle,
// driven by real hustleruntime, calls Invoke (never Stream — confirmed
// against Harness's own hustleruntime test fixtures) with the classifier's
// marshaled input as the sole text block of the first user message. respond
// receives that raw input JSON and returns the model's structured-output
// JSON text verbatim; a correct respond implementation echoes back the
// input's "basis" object unchanged (DecodeOutput rejects any basis that
// does not match subject.Basis exactly) and fills risk/authorization/
// recommendation/rationale per the scenario under test.
//
// It is never actually driven end-to-end by these tests (see the file's
// scope note), but is built to the exact real wire contract so it is
// immediately usable once Harness's reviewContext gap closes, and so
// commandsafety.New/gate.NewPermissionClassifierSet accept a live,
// non-degenerate inference.Client wherever a registration needs one for
// construction.
type scriptedClassifierClient struct {
	respond func(inputJSON string) (string, error)
}

func (c *scriptedClassifierClient) Invoke(_ context.Context, req inference.Request) (*inference.Response, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("scriptedClassifierClient: no input message")
	}
	user, ok := req.Messages[0].(*content.UserMessage)
	if !ok || len(user.Blocks) == 0 {
		return nil, errors.New("scriptedClassifierClient: malformed input message")
	}
	text, ok := user.Blocks[0].(*content.TextBlock)
	if !ok {
		return nil, errors.New("scriptedClassifierClient: input is not text")
	}
	out, err := c.respond(text.Text)
	if err != nil {
		return nil, err
	}
	return &inference.Response{
		Message:      &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: out}}}},
		Usage:        &content.Usage{},
		FinishReason: stream.FinishReasonStop,
	}, nil
}

func (c *scriptedClassifierClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("scriptedClassifierClient.Stream not used (hustleruntime calls Invoke only)")
}

// classifierInputBasis mirrors internal/wire's private input wire shape
// (classifiers/internal/wire/input.go) just enough to read the one nested
// object CodeRig's fixture needs to echo back.
type classifierInputEnvelope struct {
	Basis json.RawMessage `json:"basis"`
}

// echoingClassifierAllowResponse builds a scriptedClassifierClient.respond
// implementation that decodes inputJSON's basis and echoes it verbatim into
// a low-risk allow verdict — the shape a real safe-command classification
// would produce. It is a fixture-correctness aid, not exercised live (see
// file scope note).
func echoingClassifierAllowResponse(inputJSON string) (string, error) {
	var envelope classifierInputEnvelope
	if err := json.Unmarshal([]byte(inputJSON), &envelope); err != nil {
		return "", fmt.Errorf("scriptedClassifierClient: decode input: %w", err)
	}
	out := fmt.Sprintf(`{"version":"command_safety_output.v1","basis":%s,"risk":"low","authorization":"unknown","categories":[],"recommendation":"allow","rationale":"routine, read-only diagnostic command"}`, string(envelope.Basis))
	return out, nil
}

func newScriptedClassifierClient() *scriptedClassifierClient {
	return &scriptedClassifierClient{respond: echoingClassifierAllowResponse}
}

// ---- fixtures: real session construction over an isolated store ----------

// permissionReviewIntegrationAgent builds a REAL CodeRig session (an
// isolated in-memory store + throwaway workspace, exactly newTestAgent's
// isolation contract) over the production assembly path
// (buildRigForDelegationCaps), with permissionReview installed exactly as
// production's openSessionWithDefinitions does. interactive selects a real,
// writable, HOME-derived permission store and gate.NewInteractiveEvaluator
// (headless's evaluator never opens an awaitable gate — a Gated capability
// is denied outright, never left open for a human) — every scenario that
// needs a human-answerable gate MUST pass interactive=true, which requires
// the caller to have already set HOME via t.Setenv (this helper does not do
// so itself, since some scenarios deliberately want the DEFAULT/no-HOME
// headless posture).
func permissionReviewIntegrationAgent(t *testing.T, cfg Config, client inference.Client, interactive bool) *RuntimeAgent {
	t.Helper()
	root := t.TempDir()
	access, err := buildSessionAccess(cfg, root, interactive)
	if err != nil {
		t.Fatalf("buildSessionAccess() error = %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })
	cfg.AccessConfigRev = access.configRev

	definitions, err := swarmDefinitions(client, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	permissionReview, err := newPermissionReviewRegistration(cfg, client)
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatalf("openStores() error = %v", err)
	}
	assembly, err := buildRigForDelegationCaps(
		definitions, stores, root, cfg, false,
		rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}, permissionReview,
	)
	if err != nil {
		t.Fatalf("buildRigForDelegationCaps() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	adapter, err := newSessionAdapter(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatalf("newSessionAdapter() error = %v", err)
	}
	agent := newRuntimeAgent(adapter, controller, root, access)
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

// readOnlyReviewConfig is the shared Config for every scenario that needs a
// Gated (not Allowed) Bash command: AccessReadOnly makes Command sandbox.Gated
// (coderigProfile, access.go), so a Bash call always needs an approval.
func readOnlyReviewConfig(enabled bool, strict bool) Config {
	cfg := Config{AccessProfile: AccessReadOnly}
	if enabled {
		cfg.PermissionReviewEnabled = true
		cfg.PermissionReviewModel = permissionReviewTestModel()
		cfg.PermissionReviewStrictPolicy = strict
	}
	return cfg
}

// ---- fixtures: event-stream gate helpers ----------------------------------

// permissionGateWait watches sub for the next permission GateOpened, up to
// timeout, and returns its GateID and ToolExecutionID. It fails the test on
// timeout — every scenario that calls this expects a Gated Bash call to
// actually open a gate.
func permissionGateWait(t *testing.T, ctx context.Context, sub event.Subscription, timeout time.Duration) (gate.ID, uuid.UUID) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case delivery := <-sub.Events():
			if opened, ok := delivery.Event.(event.GateOpened); ok && opened.Gate.Kind == gate.KindPermission {
				return opened.Gate.ID, opened.Gate.Subject.ToolExecutionID
			}
		case <-deadline:
			t.Fatal("permission gate did not open within deadline")
		case <-ctx.Done():
			t.Fatalf("context done waiting for permission gate: %v", ctx.Err())
		}
	}
}

// noGateResolvedWithin backs the "gate remains open, nothing auto-resolves
// it" scenarios: it proves no GateResolved for gateID arrives on sub within
// window, failing the test immediately if one does.
func noGateResolvedWithin(t *testing.T, sub event.Subscription, gateID gate.ID, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case delivery := <-sub.Events():
			if resolved, ok := delivery.Event.(event.GateResolved); ok && resolved.GateID == gateID {
				t.Fatalf("gate %v resolved unexpectedly (source=%+v) while nothing should have answered it yet", gateID, resolved.Source)
			}
		case <-deadline:
			return
		}
	}
}

// drainToTurnTerminal consumes sub until a terminal event for turnID (or any
// turn, if turnID is zero and only one turn is in flight) is observed, or ctx
// is done. It tolerates GateOpened/GateResolved/tool events flowing past.
func drainToTurnTerminal(t *testing.T, ctx context.Context, sub event.Subscription) event.Event {
	t.Helper()
	for {
		select {
		case delivery := <-sub.Events():
			switch ev := delivery.Event.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return ev
			}
		case <-ctx.Done():
			t.Fatalf("context done waiting for turn terminal: %v", ctx.Err())
		}
	}
}

// respondApprove/respondApproveAlways/respondDeny answer gateID through the
// sessionadapter's public RespondGate(ctx, gateID, action, values), which
// always stamps gate.ResponseSource{Kind: gate.ResponseFromUser} — the same
// human provenance a CLI's approval prompt would produce.
func respondApprove(t *testing.T, ctx context.Context, agent *RuntimeAgent, gateID gate.ID) error {
	t.Helper()
	return agent.RespondGate(ctx, gateID, string(gate.ApprovalApprove), nil)
}

func respondApproveAlways(t *testing.T, ctx context.Context, agent *RuntimeAgent, gateID gate.ID) error {
	t.Helper()
	return agent.RespondGate(ctx, gateID, string(gate.ApprovalApproveAlwaysWorkspace), nil)
}

func respondDeny(t *testing.T, ctx context.Context, agent *RuntimeAgent, gateID gate.ID) error {
	t.Helper()
	return agent.RespondGate(ctx, gateID, string(gate.ApprovalDeny), nil)
}

// mustTurnDone fails the test unless terminal is an event.TurnDone.
func mustTurnDone(t *testing.T, terminal event.Event) {
	t.Helper()
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("terminal = %#v, want TurnDone", terminal)
	}
}

// mustExecutedExactlyOnce proves the Bash command genuinely, successfully
// executed exactly once: exactly 2 operator inference round trips (the
// initial Bash tool_use call, then the one post-execution follow-up), AND
// the tool result CodeRig's real confined executor returned actually
// contains the command's own unique marker text — not merely that some
// result came back (a denial or error message would also produce exactly 2
// calls, so the call count alone is not sufficient; confirmed by swapping a
// deliberate deny in for an approve during development, which this
// additional check catches and a bare call-count check did not).
func mustExecutedExactlyOnce(t *testing.T, client *bashScript) {
	t.Helper()
	client.mu.Lock()
	calls := client.calls
	observed := client.observedToolText
	marker := client.marker
	client.mu.Unlock()
	if calls != 2 {
		t.Fatalf("operator inference calls = %d, want exactly 2 (one Bash call, one post-execution follow-up)", calls)
	}
	if !strings.Contains(observed, marker) {
		t.Fatalf("observed tool result text = %q, want it to contain the real command's own marker %q (a denial or error would still produce 2 calls, but not this text)", observed, marker)
	}
}

// ================================================================
// Scenario 1: safe command auto-approved once.
// ================================================================

// TestPermissionReviewSafeCommandEligibleUnderRegisteredPolicy proves, using
// CodeRig's OWN registered decision policy (exactly what
// newPermissionReviewRegistration builds for a live session), that a
// low-risk allow assessment for a routine, read-only command is Eligible —
// the decision that (once Harness's reviewContext gap closes) would drive a
// real auto-approval. It uses gate.EvaluatePermissionAssessment directly,
// mirroring Task 23's own permission_review_test.go pattern, because a live
// classifier round trip cannot be triggered through CodeRig's public
// composition today (see file scope note).
func TestPermissionReviewSafeCommandEligibleUnderRegisteredPolicy(t *testing.T) {
	t.Parallel()
	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
	}, newScriptedClassifierClient())
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}

	subject := permissionReviewSubjectFixtureForCommand(t, registration.policy.Revision, "go test ./...")
	assessment := gate.PermissionAssessment{
		Basis:          subject.Basis,
		Risk:           gate.ReviewRiskLow,
		Authorization:  gate.ReviewAuthorizationUnknown,
		Recommendation: gate.ReviewAllow,
		Rationale:      "routine test run",
	}
	decision := gate.EvaluatePermissionAssessment(registration.policy, subject, assessment)
	if !decision.Eligible {
		t.Fatalf("decision = %+v, want Eligible for a low-risk allow assessment on a routine command", decision)
	}
}

// TestPermissionReviewSafeCommandExecutesExactlyOnceOnApproval proves the
// OTHER half of "safe command auto-approved once": once a gate resolves
// Approve (the SAME one-shot action a classifier-originated response would
// ever produce — gate.ApprovalApprove, never "always"), the underlying Bash
// tool actually executes, through CodeRig's real confined executor, EXACTLY
// once — not zero, not twice. The session has a real classifier registered
// throughout, proving its mere presence does not change ordinary approved
// execution.
func TestPermissionReviewSafeCommandExecutesExactlyOnceOnApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &bashScript{command: "echo safe-command-marker", marker: "safe-command-marker"}
	cfg := readOnlyReviewConfig(true, false)
	agent := permissionReviewIntegrationAgent(t, cfg, client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run the safe command"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	mustExecutedExactlyOnce(t, client)
}

// permissionReviewSubjectFixtureForCommand is permissionReviewSubjectFixture
// generalized to an arbitrary command, reusing the SAME public-API
// construction technique (permission_review_test.go).
func permissionReviewSubjectFixtureForCommand(t *testing.T, gatePolicyRevision, command string) gate.PermissionReviewSubject {
	t.Helper()
	toolExecutionID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174120")
	reviewContext := gate.ReviewContext{
		Coordinates: identity.Coordinates{
			SessionID: uuid.MustParse("123e4567-e89b-12d3-a456-426614174101"),
			LoopID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174102"),
			TurnID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174103"),
			StepID:    uuid.MustParse("123e4567-e89b-12d3-a456-426614174104"),
		},
		ContextRevision:    "context-v1",
		WorkspaceRoot:      "/workspace",
		WorkingDirectory:   "/workspace/repo",
		SecurityCeiling:    "readonly",
		GatePolicyRevision: gatePolicyRevision,
		Entries: []gate.ReviewContextEntry{
			{Origin: gate.ReviewContextOriginUser, Kind: gate.ReviewContextKindUserMessage, Content: "inspect the repository"},
			{Origin: gate.ReviewContextOriginAssistant, Kind: gate.ReviewContextKindAssistantToolRequest, Content: fmt.Sprintf(`{"command":%q}`, command)},
		},
	}
	request := tool.Request{
		ToolName:           "Bash",
		Summary:            "run " + command,
		ExecutionID:        toolExecutionID.String(),
		Command:            command,
		WorkingDirectory:   "/workspace/repo",
		ExpiresAtUnixMilli: 1900000000000,
		Requirements: []tool.Requirement{{
			Kind:        tool.CapabilityCommandExecute,
			Match:       command,
			Description: "run " + command,
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: command,
			Candidates: []tool.RuleCandidate{{
				Kind:        tool.CapabilityCommandExecute,
				Match:       "Bash(" + command + ")",
				Description: "Bash(" + command + ")",
				GrantClass:  tool.GrantClassCommandStart,
				GrantTarget: command,
			}},
		}},
	}
	basis := gate.ReviewBasis{
		GateID:             uuid.MustParse("123e4567-e89b-12d3-a456-426614174121"),
		ToolExecutionID:    toolExecutionID,
		ContextRevision:    reviewContext.ContextRevision,
		GatePolicyRevision: reviewContext.GatePolicyRevision,
		ClassifierRevision: "command-safety-v1",
		SecurityCeiling:    reviewContext.SecurityCeiling,
	}
	subject, err := gate.NewPermissionReviewSubject(basis, request, reviewContext)
	if err != nil {
		t.Fatalf("gate.NewPermissionReviewSubject() error = %v", err)
	}
	return subject
}

// ================================================================
// Construction unblock: the actual, concrete thing the two documented
// Harness addenda (rig.WithPermissionReviewEvidence + pkg/gate/evidence.go)
// fix. Task 23's own TestPermissionReviewExplicitEnable/HeadlessComposesSafely
// deliberately stopped at rig.Define()/construction because starting a
// classifier-registered session failed 100% of the time
// (*hustleruntime.ConfigError{Reason: ConfigMissingCollaborator}). This test
// proves that gap is CLOSED: a real classifier-registered, real-evidence-wired
// session now reaches assembly.NewSession() successfully and can run an
// ordinary turn.
// ================================================================

// TestPermissionReviewClassifierRegisteredSessionConstructsAndExecutesTools
// builds a real session with a real command-safety classifier and real
// evidence wiring registered (AccessTrusted, so the Bash call itself needs no
// gate — this test is about construction/startup, not gate mechanics), and
// proves an ordinary turn completes and the tool actually ran.
func TestPermissionReviewClassifierRegisteredSessionConstructsAndExecutesTools(t *testing.T) {
	t.Parallel()
	client := &bashScript{command: "echo construction-marker", marker: "construction-marker"}
	cfg := Config{
		AccessProfile:           AccessTrusted,
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
	}
	agent := permissionReviewIntegrationAgent(t, cfg, client, false)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run the construction command"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))
	mustExecutedExactlyOnce(t, client)
}

// TestPermissionReviewEvidenceKindsMatchClassifierRequirement proves
// CodeRig's registration wires the EXACT evidence-kind allowlist
// commandsafety.RequiredEvidenceKinds() reports — never a hand-copied list —
// into the same rig.WithPermissionReviewEvidence call
// TestPermissionReviewClassifierRegisteredSessionConstructsAndExecutesTools
// just proved a real session accepts.
func TestPermissionReviewEvidenceKindsMatchClassifierRequirement(t *testing.T) {
	t.Parallel()
	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
	}, newScriptedClassifierClient())
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	want := commandsafety.RequiredEvidenceKinds()
	if len(registration.evidenceKinds) != len(want) {
		t.Fatalf("evidenceKinds = %v, want %v", registration.evidenceKinds, want)
	}
	for i := range want {
		if registration.evidenceKinds[i] != want[i] {
			t.Fatalf("evidenceKinds[%d] = %q, want %q", i, registration.evidenceKinds[i], want[i])
		}
	}
}

// ================================================================
// Scenario 2: evidence lookup.
// ================================================================

// TestPermissionReviewEvidenceLookupAccessAndContainmentApproveRealTargets
// drives CodeRig's REAL evidence access/containment values — exactly what
// newPermissionReviewRegistration wires into rig.WithPermissionReviewEvidence
// for a live session — against a realistic evidence lookup: a classifier
// checking whether a deletion target exists (an evidence_filesystem_stat-shaped
// Requirement) against a REAL temp workspace, for both an EXISTING file and a
// MISSING one. This proves the evidence-authorization pipeline the two
// Harness addenda unblocked genuinely works in CodeRig's own composition
// (see file scope note for why this cannot additionally be driven through a
// live classifier round trip today).
func TestPermissionReviewEvidenceLookupAccessAndContainmentApproveRealTargets(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	writeEvidenceFile(t, root, "config/legacy.yaml", "old: true")

	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
		AccessProfile:           AccessReadOnly,
	}, newScriptedClassifierClient())
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: string(AccessReadOnly)}

	for _, tc := range []struct {
		name  string
		match string
	}{
		{name: "existing deletion target", match: "config/legacy.yaml"},
		{name: "missing deletion target (still a valid stat lookup)", match: "config/does-not-exist.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirement := tool.Requirement{Kind: "evidence.filesystem.stat", Scope: root, Match: tc.match, Description: "stat " + tc.match}
			access, err := registration.evidenceAccess.AccessFor(requirement)
			if err != nil || access != gate.AccessAllow {
				t.Fatalf("evidenceAccess.AccessFor(%+v) = %d, %v, want AccessAllow, nil", requirement, access, err)
			}
			req := tool.Request{Requirements: []tool.Requirement{requirement}}
			if err := registration.evidenceContainment.VerifyEvidenceContainment(context.Background(), policy, req); err != nil {
				t.Fatalf("evidenceContainment.VerifyEvidenceContainment() error = %v, want nil for an in-workspace target", err)
			}
		})
	}

	// A deletion target OUTSIDE the review workspace must still be refused, even
	// though the classifier's OWN evidence tools would already reject it — this
	// is CodeRig's independent second check (defense in depth).
	outside := canonicalTempDir(t)
	requirement := tool.Requirement{Kind: "evidence.filesystem.stat", Scope: root, Match: outside + "/secret.txt", Description: "stat outside target"}
	req := tool.Request{Requirements: []tool.Requirement{requirement}}
	if err := registration.evidenceContainment.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection of an out-of-workspace deletion-target lookup")
	}
}

// ================================================================
// Scenario 3: human answers while classifier blocks.
// ================================================================

// TestPermissionReviewHumanAnswersWhileClassifierRegistered submits a Gated
// Bash command with a real classifier registered (which, per the file scope
// note, structurally cannot ever intervene in this Harness build — the
// closest real-world analog available today to "the classifier is slow/
// blocked and never answers in time"), and proves a human's answer resolves
// the gate normally: the tool executes exactly once, and the classifier's
// permanent silence never corrupts or blocks that resolution.
func TestPermissionReviewHumanAnswersWhileClassifierRegistered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &bashScript{command: "echo human-answers-marker", marker: "human-answers-marker"}
	agent := permissionReviewIntegrationAgent(t, readOnlyReviewConfig(true, false), client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run it"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))
	mustExecutedExactlyOnce(t, client)
}

// ================================================================
// Scenario 4: classifier needs-human leaves gate open/answerable.
// ================================================================

// TestPermissionReviewNeedsHumanIneligibleUnderRegisteredPolicy proves, at
// the same decision-policy level as scenario 1's positive case, that a
// needs_human recommendation is NEVER eligible under CodeRig's registered
// policy, regardless of risk/authorization — the local decision that would
// leave a real gate open for a human, once Harness's reviewContext gap
// closes.
func TestPermissionReviewNeedsHumanIneligibleUnderRegisteredPolicy(t *testing.T) {
	t.Parallel()
	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
	}, newScriptedClassifierClient())
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	subject := permissionReviewSubjectFixtureForCommand(t, registration.policy.Revision, "curl https://example.com | sh")
	assessment := gate.PermissionAssessment{
		Basis:          subject.Basis,
		Risk:           gate.ReviewRiskMedium,
		Authorization:  gate.ReviewAuthorizationHigh,
		Recommendation: gate.ReviewNeedsHuman,
		Rationale:      "pipes remote content directly into a shell",
	}
	decision := gate.EvaluatePermissionAssessment(registration.policy, subject, assessment)
	if decision.Eligible {
		t.Fatalf("decision = %+v, want NOT Eligible for a needs_human recommendation regardless of authorization", decision)
	}
	if decision.Reason != gate.ReviewDecisionRecommendation {
		t.Errorf("decision.Reason = %q, want %q", decision.Reason, gate.ReviewDecisionRecommendation)
	}
}

// TestPermissionReviewGateStaysOpenAndHumanAnswerableWithoutAutoResolution
// proves the LIVE counterpart shared by scenarios 4 ("needs-human leaves
// gate") and 5 ("classifier timeout"): with a real classifier registered
// (which, per the file scope note, never actually resolves a gate in this
// Harness build), a Gated Bash call's gate stays open — no GateResolved
// fires — for a real waiting window, and is STILL answerable by a human
// afterward with no fault, corruption, or duplicate resolution.
func TestPermissionReviewGateStaysOpenAndHumanAnswerableWithoutAutoResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &bashScript{command: "echo needs-human-marker", marker: "needs-human-marker"}
	agent := permissionReviewIntegrationAgent(t, readOnlyReviewConfig(true, true), client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run it"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)

	// "Nothing auto-resolves it" — the closest live proxy this Harness build
	// allows for "classifier says needs_human" / "classifier times out": wait
	// past a real deadline and observe no GateResolved.
	noGateResolvedWithin(t, sub, gateID, 1500*time.Millisecond)

	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() after the wait window error = %v, want a normal resolution", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))
}

// ================================================================
// Scenario 6: critical risk never auto-approves.
// ================================================================

// TestPermissionReviewCriticalRiskNeverEligible proves the hard ceiling
// (design's absolute rule: critical risk is never eligible, full stop) holds
// under BOTH of CodeRig's registered policies, even for an assessment that
// reports the maximum possible authorization and an allow recommendation —
// the one case an authorization-threshold check alone could mistakenly
// admit.
func TestPermissionReviewCriticalRiskNeverEligible(t *testing.T) {
	t.Parallel()
	for _, strict := range []bool{false, true} {
		strict := strict
		t.Run(fmt.Sprintf("strict=%t", strict), func(t *testing.T) {
			t.Parallel()
			registration, err := newPermissionReviewRegistration(Config{
				PermissionReviewEnabled:      true,
				PermissionReviewModel:        permissionReviewTestModel(),
				PermissionReviewStrictPolicy: strict,
			}, newScriptedClassifierClient())
			if err != nil {
				t.Fatalf("newPermissionReviewRegistration() error = %v", err)
			}
			subject := permissionReviewSubjectFixtureForCommand(t, registration.policy.Revision, "rm -rf /")
			assessment := gate.PermissionAssessment{
				Basis:          subject.Basis,
				Risk:           gate.ReviewRiskCritical,
				Authorization:  gate.ReviewAuthorizationHigh,
				Recommendation: gate.ReviewAllow,
				Categories:     []gate.ReviewRiskCategory{gate.ReviewCategoryDestructiveLocal},
				Rationale:      "recursive unconditional deletion at the filesystem root",
			}
			decision := gate.EvaluatePermissionAssessment(registration.policy, subject, assessment)
			if decision.Eligible {
				t.Fatalf("decision = %+v, want NOT Eligible: critical risk is never eligible regardless of authorization", decision)
			}
			if decision.Reason != gate.ReviewDecisionRiskCeiling {
				t.Errorf("decision.Reason = %q, want %q", decision.Reason, gate.ReviewDecisionRiskCeiling)
			}
		})
	}
}

// ================================================================
// Scenario 7: stale basis.
// ================================================================

// TestPermissionReviewStaleBasisRejectedByPolicyEvaluation proves CodeRig's
// registered policy evaluation rejects an assessment computed against a
// basis that no longer matches the live subject (a classifier's answer that
// arrived after something about the review moved on) as a basis mismatch,
// never silently accepting it.
func TestPermissionReviewStaleBasisRejectedByPolicyEvaluation(t *testing.T) {
	t.Parallel()
	registration, err := newPermissionReviewRegistration(Config{
		PermissionReviewEnabled: true,
		PermissionReviewModel:   permissionReviewTestModel(),
	}, newScriptedClassifierClient())
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	subject := permissionReviewSubjectFixtureForCommand(t, registration.policy.Revision, "go build ./...")
	staleBasis := subject.Basis
	staleBasis.ContextRevision = "context-v-stale" // the review moved on since this assessment was computed
	assessment := gate.PermissionAssessment{
		Basis:          staleBasis,
		Risk:           gate.ReviewRiskLow,
		Authorization:  gate.ReviewAuthorizationUnknown,
		Recommendation: gate.ReviewAllow,
		Rationale:      "routine build",
	}
	decision := gate.EvaluatePermissionAssessment(registration.policy, subject, assessment)
	if decision.Eligible {
		t.Fatal("decision.Eligible = true, want NOT Eligible for an assessment computed against a stale basis")
	}
	if decision.Reason != gate.ReviewDecisionBasisMismatch {
		t.Errorf("decision.Reason = %q, want %q", decision.Reason, gate.ReviewDecisionBasisMismatch)
	}
}

// TestPermissionReviewLateResponseAfterHumanResolutionIsDroppedNotDoubleProcessed
// proves the LIVE half of "stale basis": once a human resolves a gate, a
// SECOND response to the SAME gate id (standing in for a classifier's
// now-stale response arriving after the fact — the exact race
// harness/internal/sessionruntime/review_race_test.go proves at the
// session-runtime layer) is dropped as stale — rejected, not
// double-processed, and the already-resolved grant is untouched (the tool
// still ran exactly once).
func TestPermissionReviewLateResponseAfterHumanResolutionIsDroppedNotDoubleProcessed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &bashScript{command: "echo stale-basis-marker", marker: "stale-basis-marker"}
	agent := permissionReviewIntegrationAgent(t, readOnlyReviewConfig(true, false), client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run it"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("first RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	// The "late" second response: same gate id, already resolved.
	if err := respondDeny(t, ctx, agent, gateID); err == nil {
		t.Fatal("second RespondGate() on an already-resolved gate error = nil, want a stale/not-found rejection")
	}

	mustExecutedExactlyOnce(t, client)
}

// ================================================================
// Scenario 8: no persistent rule from an auto/classifier-shaped approval.
// ================================================================

// TestPermissionReviewApprovalNeverPersistsAutoAlwaysRule proves that even
// with a real workspace rule WRITER available (interactive construction) and
// a real classifier registered, a plain one-shot Approve — the ONLY action
// gate.ResponseFromClassifier could ever carry, since respondFromClassifier
// has no action parameter (harness/internal/sessionruntime/review_race_test.go's
// own proof) — never persists a durable "approve always" rule. It resubmits
// the EXACT SAME command a second time (echo is not in CodeRig's automatic
// family catalog — permissions.go — so the ONLY rule "Approve always for this
// workspace" could ever have offered here is an EXACT-command one, matching
// TestAcceptanceFamilyAndExactApprovalFlow's "git commit" case): if the first
// Approve had persisted anything, this identical second call would resolve
// with no GateOpened at all and permissionGateWait would time out and fail
// the test. (Confirmed this test actually detects persistence: swapping the
// first response to respondApproveAlways — the wrong action a real
// classifier-shaped approval could never produce — makes this test fail
// exactly this way.)
func TestPermissionReviewApprovalNeverPersistsAutoAlwaysRule(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &bashScript{command: "echo no-persistent-rule-marker", marker: "no-persistent-rule-marker"}
	agent := permissionReviewIntegrationAgent(t, readOnlyReviewConfig(true, false), client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run the first command"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	firstGate, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, firstGate); err != nil {
		t.Fatalf("first RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	// The EXACT SAME command again: if the first Approve had persisted an
	// exact-command rule, this second Gated call would resolve with no
	// GateOpened at all and permissionGateWait would time out and fail the
	// test.
	client.mu.Lock()
	client.calls = 0
	client.observedToolText = ""
	client.mu.Unlock()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run the identical command again"}}); err != nil {
		t.Fatalf("second Submit() error = %v", err)
	}
	secondGate, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if secondGate == firstGate {
		t.Fatal("second gate id equals the first; want a fresh gate for the repeated command")
	}
	if err := respondApprove(t, ctx, agent, secondGate); err != nil {
		t.Fatalf("second RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))
}

// ================================================================
// Scenario 9: exact grant mint after auto-approval (same path as human).
// ================================================================

// TestPermissionReviewApprovalReachesRealExecutorGrantPath mirrors Harness's
// own TestRespondFromClassifierMintsGrantsThroughEvaluatorLikeHuman
// (internal/sessionruntime/review_race_test.go), which proves a classifier
// response reaches the EXACT SAME dispatchGateCommand -> loop ->
// gate.Evaluator.Resolve execution path a human approval does. That test
// establishes the two paths converge; this test proves CodeRig's OWN
// composition (real toolsets.go roleGate, real sandbox.ExecutorSet, real `sh
// -c` execution) reaches that shared path with no CodeRig-side shortcut or
// bypass: the tool only runs because a real grant was minted through the
// real gate.Evaluator, evidenced by the command's REAL stdout coming back on
// the next inference request, not merely because a response was accepted.
func TestPermissionReviewApprovalReachesRealExecutorGrantPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const marker = "grant-path-proof-9f2c"
	client := &bashScript{command: "echo " + marker, marker: marker}
	agent := permissionReviewIntegrationAgent(t, readOnlyReviewConfig(true, false), client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "prove the grant path"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))
	mustExecutedExactlyOnce(t, client)
}

// ================================================================
// Scenario 10: disabled config preserves old behavior (the single most
// important regression-safety property in this task).
// ================================================================

// TestPermissionReviewDisabledConfigMatchesPreFeatureBuildRig proves that
// Config.PermissionReviewEnabled == false (the default) produces a
// registration that is byte-for-byte interchangeable with the OLD path every
// pre-Task-23 CodeRig call site still uses (buildRig, which always assembles
// with the disabled zero permissionReviewRegistration): a session opened
// through buildRig can be restored through buildRigForDelegationCaps with an
// explicitly-disabled registration, and vice versa, with ZERO drift changes
// at all (not merely accepted Info-level drift — see
// TestPermissionReviewConfigFingerprintChanges — an actual EXACT match),
// because a disabled registration contributes NO rig options whatsoever.
func TestPermissionReviewDisabledConfigMatchesPreFeatureBuildRig(t *testing.T) {
	t.Parallel()
	stores, err := openStores(memstore.New())
	if err != nil {
		t.Fatalf("openStores() error = %v", err)
	}
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definitions, err := swarmDefinitions(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}

	// Open with the OLD path (buildRig — every pre-Task-23 CodeRig call site).
	oldAssembly, err := buildRig(definitions, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}
	controller, err := oldAssembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sid := controller.SessionID()
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	// Restore through the NEW path with an explicitly-disabled registration
	// (exactly what newPermissionReviewRegistration returns for
	// Config{PermissionReviewEnabled: false}, the default).
	disabled, err := newPermissionReviewRegistration(Config{}, &fakeLLM{})
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	if disabled.enabled {
		t.Fatal("newPermissionReviewRegistration(Config{}) enabled = true, want false")
	}
	if options := disabled.options(); len(options) != 0 {
		t.Fatalf("disabled.options() = %d options, want 0", len(options))
	}
	definitions2, err := swarmDefinitions(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	newAssembly, err := buildRigForDelegationCaps(
		definitions2, stores, root, cfg, false,
		rig.DelegationLimits{Depth: operatorSpawnDepth, Quota: operatorSpawnQuota}, disabled,
	)
	if err != nil {
		t.Fatalf("buildRigForDelegationCaps() error = %v", err)
	}
	restored, err := newAssembly.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession() under the NEW path with a disabled registration error = %v, want an exact, driftless restore", err)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

// TestPermissionReviewDisabledConfigLiveGateBehaviorUnchanged drives the
// EXACT same interactive, AccessReadOnly, Gated-Bash live scenario every
// enabled-config scenario above does, but with PermissionReviewEnabled
// false, and proves the observable behavior is ordinary human-only gating:
// one GateOpened, one human Approve, exactly one execution — the identical
// shape scenario 3/9 prove for an enabled session, confirming the disabled
// path never trips over the presence of the (unused) permission-review
// wiring at any layer.
func TestPermissionReviewDisabledConfigLiveGateBehaviorUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := &bashScript{command: "echo disabled-config-marker", marker: "disabled-config-marker"}
	agent := permissionReviewIntegrationAgent(t, readOnlyReviewConfig(false, false), client, true)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run it"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	mustExecutedExactlyOnce(t, client)
}
