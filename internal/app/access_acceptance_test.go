package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools/permission"
)

// access_acceptance_test.go raises acceptance coverage to the full named product
// list for Task 5.4. It complements the assembly tests (access_assembly_test.go)
// and the exact-value/unit tests (access_test.go, egress_test.go,
// permissions_test.go) by driving behavior THROUGH the assembled session access
// wiring: the session gate opened under each profile, the interactive workspace
// store's family/exact approval flow, the headless permission-file load, the
// egress fail-closed surfaced at assembly, new/restore authority parity, and the
// agent-level executor-set shutdown. OS-level enforcement proofs (real-HOME and
// root denial, /dev/null usability, deep proxy target/broad behavior) belong to
// the sandbox and tests modules; see the file-level notes at each scenario.

// mustLoopProvenance returns a background context carrying a non-zero loop
// provenance so the assembled accessGate can resolve the calling loop's executor.
func mustLoopProvenance(t *testing.T) context.Context {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return loop.WithProvenance(context.Background(), loop.Provenance{LoopID: id})
}

// grantFreeRequest builds a single-requirement prepared request that needs no
// executor grant (no GrantClass/GrantTarget), so Authorize observes only the
// profile's structural access decision (Allow/Deny/Gated) without minting a
// token. Kind/Scope route to the session's Generic sandbox profile.
func grantFreeRequest(kind, scope, match string) tool.Request {
	return tool.Request{
		ToolName: "Probe",
		Summary:  "probe " + kind,
		Requirements: []tool.Requirement{{
			Kind:        kind,
			Scope:       scope,
			Match:       match,
			Description: "probe " + kind + " " + match,
		}},
	}
}

// gateOutcome classifies the observable result of a headless Authorize.
type gateOutcome int

const (
	outcomeAllowed          gateOutcome = iota // Approved with no error (structural Allow)
	outcomeDenied                              // unapproved with no error (structural/stored Deny)
	outcomeApprovalRequired                    // typed approval-required denial (headless Gated, unmet)
)

// observeGate authorizes req through the headless gate and classifies the result.
func observeGate(t *testing.T, ctx context.Context, g loop.AccessGate, req tool.Request) gateOutcome {
	t.Helper()
	resolution, err := g.Authorize(ctx, req)
	if err != nil {
		var evalErr *gate.EvaluationError
		if errors.As(err, &evalErr) && evalErr.Kind == gate.EvaluationApprovalRequired {
			return outcomeApprovalRequired
		}
		t.Fatalf("Authorize(%s) unexpected error: %v", req.Requirements[0].Kind, err)
	}
	if resolution.Approved {
		return outcomeAllowed
	}
	return outcomeDenied
}

// TestAcceptanceProfileGateBehavior opens a session's access wiring under EACH of
// the three product profiles and observes Generic's assembled gate decision
// for host read, host write, workspace write, and network — the effective
// authority each profile grants end-to-end (not just the profile struct). It also
// proves host access is DENIED under ReadOnly (the OS-enforcement counterpart —
// real-HOME/root denial and /dev/null usability — is proven by the sandbox/tests
// modules on macOS Seatbelt and Linux namespaces/Landlock). IsolatedHome vs
// RealHome policy is pinned at profile construction by TestCoderigProfileExactValues.
func TestAcceptanceProfileGateBehavior(t *testing.T) {
	root := canonicalTempDir(t)

	hostRead := grantFreeRequest("filesystem.read", "host:*", "host:*")
	hostWrite := grantFreeRequest("filesystem.write", "host:*", "host:*")
	workspaceWrite := grantFreeRequest("filesystem.write", root, root)
	network := grantFreeRequest("network", "", "*")

	cases := []struct {
		profile        AccessProfile
		hostRead       gateOutcome
		hostWrite      gateOutcome
		workspaceWrite gateOutcome
		network        gateOutcome
	}{
		// ReadOnly: no host access, no network, workspace is read-only.
		{AccessReadOnly, outcomeDenied, outcomeDenied, outcomeDenied, outcomeDenied},
		// Trusted: host read allowed, host write gated, workspace writable, network on.
		{AccessTrusted, outcomeAllowed, outcomeApprovalRequired, outcomeAllowed, outcomeAllowed},
		// Unconfined: everything allowed.
		{AccessUnconfined, outcomeAllowed, outcomeAllowed, outcomeAllowed, outcomeAllowed},
	}
	for _, tc := range cases {
		t.Run(string(tc.profile), func(t *testing.T) {
			access, err := buildHeadlessAccess(Config{
				AccessProfile:  tc.profile,
				ModelConfigRev: "fixture-model-config-revision",
				PrimerAlias:    "fixture-primer",
				PrimerEfforts:  []model.Effort{model.EffortMax},
			}, root)
			if err != nil {
				t.Fatalf("buildHeadlessAccess(%q): %v", tc.profile, err)
			}
			t.Cleanup(func() { _ = access.Close() })
			ctx := mustLoopProvenance(t)

			if got := observeGate(t, ctx, access.gate, hostRead); got != tc.hostRead {
				t.Errorf("Generic host read = %d, want %d", got, tc.hostRead)
			}
			if got := observeGate(t, ctx, access.gate, hostWrite); got != tc.hostWrite {
				t.Errorf("Generic host write = %d, want %d", got, tc.hostWrite)
			}
			if got := observeGate(t, ctx, access.gate, workspaceWrite); got != tc.workspaceWrite {
				t.Errorf("Generic workspace write = %d, want %d", got, tc.workspaceWrite)
			}
			if got := observeGate(t, ctx, access.gate, network); got != tc.network {
				t.Errorf("Generic network = %d, want %d", got, tc.network)
			}

		})
	}
}

// commandRequest mirrors the tools/bash command requirement (commandRequirement)
// so the test exercises the SAME shapes the assembled Bash tool prepares, but
// wired to CodeRig's product family catalog. When the command is a catalog member
// the reusable candidate is the family prefix; otherwise it is the exact command.
func commandRequest(t *testing.T, root, command string) tool.Request {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	requirement := tool.Requirement{
		Kind:        permission.CapabilityCommandExecute,
		Match:       command,
		Description: "execute `" + command + "`",
		GrantClass:  tool.GrantClassCommandStart,
		GrantTarget: command,
	}
	if candidate := permission.ProposeCommandCandidate(command, productFamilyEligibility()); candidate != "" {
		requirement.Candidates = []tool.RuleCandidate{{
			Kind:        permission.CapabilityCommandExecute,
			Match:       candidate,
			Description: "allow `" + candidate + "`",
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: command,
		}}
	}
	return tool.Request{
		ToolName:           "Bash",
		Summary:            "run command",
		ExecutionID:        id.String(),
		Command:            command,
		WorkingDirectory:   root,
		ExpiresAtUnixMilli: time.Now().Add(5 * time.Minute).UnixMilli(), // within the executor's grant TTL
		Requirements:       []tool.Requirement{requirement},
	}
}

// TestAcceptanceFamilyAndExactApprovalFlow drives the INTERACTIVE assembled
// Generic gate + workspace permission store end-to-end: a `git log` invocation is
// approved-always, persisting CodeRig's product family rule (git log/status/diff/
// show/push), so a DIFFERENT `git log` invocation is reused with no second prompt,
// while a non-catalog `git commit` gets only an exact candidate and still prompts.
// This asserts the wired family catalog is the product one and that a family
// approval flows through the session's assembled gate.
func TestAcceptanceFamilyAndExactApprovalFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // interactive assembly derives the workspace store path from HOME
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	root := canonicalTempDir(t)

	// ReadOnly makes command execution Gated, so the first approval is required.
	access, err := buildSessionAccess(Config{AccessProfile: AccessReadOnly}, root, true)
	if err != nil {
		t.Fatalf("buildSessionAccess(interactive): %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })

	var prompts int
	var lastCandidates []tool.RuleCandidate
	approve := func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		prompts++
		lastCandidates = prompt.Candidates
		return gate.ApprovalApproveAlwaysWorkspace, nil
	}
	ctx := loop.WithApprovalRequester(mustLoopProvenance(t), approve)

	// 1. First `git log` invocation prompts once and offers the git-log FAMILY
	//    candidate (product catalog), which "Approve always" persists.
	res, err := access.gate.Authorize(ctx, commandRequest(t, root, "git log --oneline"))
	if err != nil {
		t.Fatalf("Authorize(git log --oneline): %v", err)
	}
	if !res.Approved {
		t.Fatal("first git log was not approved")
	}
	if prompts != 1 {
		t.Fatalf("prompts after first approval = %d, want 1", prompts)
	}
	wantFamily := permission.ProposeCommandCandidate("git log --oneline", productFamilyEligibility())
	if len(lastCandidates) != 1 || lastCandidates[0].Match != wantFamily {
		t.Fatalf("offered candidate = %+v, want the product family candidate %q", lastCandidates, wantFamily)
	}
	if wantFamily == "git log --oneline" {
		t.Fatalf("git log produced an exact candidate %q, want a git-log family", wantFamily)
	}

	// 2. A DIFFERENT git-log invocation reuses the persisted family rule: approved
	//    with NO second prompt.
	res, err = access.gate.Authorize(ctx, commandRequest(t, root, "git log -n 5 --stat"))
	if err != nil {
		t.Fatalf("Authorize(git log -n 5): %v", err)
	}
	if !res.Approved {
		t.Fatal("sibling git log was not auto-approved from the family rule")
	}
	if prompts != 1 {
		t.Errorf("family reuse prompted again: prompts = %d, want 1", prompts)
	}

	// 3. A non-catalog `git commit` is NOT covered by the git-log family: it
	//    prompts again and offers only the EXACT command as a candidate (no family).
	res, err = access.gate.Authorize(ctx, commandRequest(t, root, "git commit -m wip"))
	if err != nil {
		t.Fatalf("Authorize(git commit): %v", err)
	}
	if !res.Approved {
		t.Fatal("git commit was not approved")
	}
	if prompts != 2 {
		t.Errorf("non-catalog command did not prompt: prompts = %d, want 2", prompts)
	}
	if len(lastCandidates) != 1 || lastCandidates[0].Match != "git commit -m wip" {
		t.Errorf("git commit candidate = %+v, want the exact command (not a family)", lastCandidates)
	}
}

func TestAcceptanceGenericPermissionApprovalReachesStoreAndExecutorGrantPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	root := canonicalTempDir(t)
	command := "echo task6-grant-path"

	access, err := buildSessionAccess(Config{
		AccessProfile:  AccessReadOnly,
		ModelConfigRev: "fixture-model-revision-must-not-change-access",
		PrimerAlias:    "fixture-model-alias-must-not-change-access",
		PrimerEfforts:  []model.Effort{model.EffortMax},
	}, root, true)
	if err != nil {
		t.Fatalf("buildSessionAccess(interactive): %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })

	provenanceContext := mustLoopProvenance(t)
	provenance, ok := loop.ProvenanceFrom(provenanceContext)
	if !ok {
		t.Fatal("loop provenance missing from test context")
	}
	prompts := 0
	ctx := loop.WithApprovalRequester(provenanceContext, func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		prompts++
		return gate.ApprovalApproveAlwaysWorkspace, nil
	})
	request := commandRequest(t, root, command)
	resolution, err := access.gate.Authorize(ctx, request)
	if err != nil {
		t.Fatalf("Authorize(first command): %v", err)
	}
	if !resolution.Approved || len(resolution.Grants) == 0 || prompts != 1 {
		t.Fatalf("first resolution approved=%v grants=%d prompts=%d, want true/nonzero/1", resolution.Approved, len(resolution.Grants), prompts)
	}
	executor, err := access.set.For(provenance.LoopID.String())
	if err != nil {
		t.Fatalf("set.For: %v", err)
	}
	stdout, exitCode, err := executor.RunCommandWithGrants(context.Background(), request.ExecutionID, root, command, resolution.Grants)
	if err != nil || exitCode != 0 || !strings.Contains(string(stdout), "task6-grant-path") {
		t.Fatalf("granted execution = stdout %q exit %d error %v", stdout, exitCode, err)
	}

	permissionPath, err := defaultPermissionsPath(filepath.Join(home, ".looprig", "coderig"), root)
	if err != nil {
		t.Fatalf("defaultPermissionsPath: %v", err)
	}
	if _, err := os.Stat(permissionPath); err != nil {
		t.Fatalf("workspace approval not persisted at hashed path: %v", err)
	}

	reopened, err := buildSessionAccess(Config{AccessProfile: AccessReadOnly}, root, true)
	if err != nil {
		t.Fatalf("reopen session access: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopenedContext := loop.WithApprovalRequester(mustLoopProvenance(t), func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		t.Fatal("persisted workspace approval unexpectedly prompted")
		return "", errors.New("unexpected prompt")
	})
	resolution, err = reopened.gate.Authorize(reopenedContext, commandRequest(t, root, command))
	if err != nil || !resolution.Approved || len(resolution.Grants) == 0 {
		t.Fatalf("reopened resolution approved=%v grants=%d error=%v", resolution.Approved, len(resolution.Grants), err)
	}
}

// TestAcceptanceProcessEnabledBashSharesExecutorAcrossGateAndBuild extends
// TestAcceptanceGenericPermissionApprovalReachesStoreAndExecutorGrantPath's
// grant-redemption proof through the REAL, session-supervised Bash
// definition Task 27 wires into the Generic roster: a grant minted by
// access.gate.Authorize for one Loop ID validates when redeemed
// through bashDefinition's OWN Build, invoked for that SAME Loop ID. Since a
// minted grant only verifies against the EXACT *sandbox.Executor it was
// authorized against, this end-to-end success proves bashDefinition's
// retained synchronous set.For(bindings.LoopID.String()) lookup resolves
// the IDENTICAL executor accessGate resolved for the identical Loop ID (both
// call set.For on the SAME access.set with the SAME LoopID string;
// sandbox.ExecutorSet.For's own memoization, proven by the sandbox module,
// is what makes a single lookup answer for both call sites) -- not a
// different or stale one.
func TestAcceptanceProcessEnabledBashSharesExecutorAcrossGateAndBuild(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	root := canonicalTempDir(t)
	command := "echo task27-grant-path"

	access, err := buildSessionAccess(Config{AccessProfile: AccessReadOnly}, root, true)
	if err != nil {
		t.Fatalf("buildSessionAccess(interactive): %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })

	provenanceContext := mustLoopProvenance(t)
	provenance, ok := loop.ProvenanceFrom(provenanceContext)
	if !ok {
		t.Fatal("loop provenance missing from test context")
	}
	ctx := loop.WithApprovalRequester(provenanceContext, func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		return gate.ApprovalApproveAlwaysWorkspace, nil
	})
	request := commandRequest(t, root, command)
	resolution, err := access.gate.Authorize(ctx, request)
	if err != nil {
		t.Fatalf("Authorize(command): %v", err)
	}
	if !resolution.Approved || len(resolution.Grants) == 0 {
		t.Fatalf("resolution approved=%v grants=%d, want true/nonzero", resolution.Approved, len(resolution.Grants))
	}
	executionID, err := uuid.Parse(request.ExecutionID)
	if err != nil {
		t.Fatalf("uuid.Parse(request.ExecutionID): %v", err)
	}

	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() (session): %v", err)
	}
	definition := bashDefinition(access.set, newProcessRunnerResolver(access.set))
	built, err := definition.Build(context.Background(), tool.Bindings{
		SessionID: sessionID,
		LoopID:    provenance.LoopID,
		Workspace: &tool.WorkspaceBinding{Root: root, Coordinator: noopCoordinator{}, Observations: tool.NewWorkspaceObservations()},
		Process:   &tool.ProcessBinding{Registry: &fakeSessionResourceRegistry{dir: t.TempDir()}},
	})
	if err != nil {
		t.Fatalf("bashDefinition Build() error = %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("bashDefinition Build() returned %d tools, want 1", len(built))
	}
	preparer, ok := built[0].(tool.CallPreparer)
	if !ok {
		t.Fatalf("built Bash tool %T does not implement tool.CallPreparer", built[0])
	}

	argsJSON := `{"command": "` + command + `"}`
	req, artifact, err := preparer.PrepareCall(context.Background(), executionID, argsJSON)
	if err != nil {
		t.Fatalf("Bash PrepareCall() error = %v", err)
	}
	runCtx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{
		ExecutionID: executionID,
		Request:     req,
		Artifact:    artifact,
		Grants:      resolution.Grants,
	})
	result, err := built[0].InvokableRun(runCtx, argsJSON)
	if err != nil {
		t.Fatalf("Bash InvokableRun() returned a Go error %v; Bash never returns one", err)
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "task27-grant-path") {
		t.Fatalf("granted execution through bashDefinition's Build = %q, want it to contain the command's own output (grant did not validate against the resolved executor)", text)
	}
}

func TestAcceptanceModelAndInteractivePermissionFilesRemainSeparated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	root := canonicalTempDir(t)

	looprigRoot := filepath.Join(home, ".looprig", "coderig")
	modelPath, err := defaultModelConfigPath(looprigRoot)
	if err != nil {
		t.Fatalf("defaultModelConfigPath: %v", err)
	}
	permissionPath, err := defaultPermissionsPath(looprigRoot, root)
	if err != nil {
		t.Fatalf("defaultPermissionsPath: %v", err)
	}
	wantModelPath := filepath.Join(home, ".looprig", "coderig", "models.json")
	if modelPath != wantModelPath {
		t.Fatalf("model path = %q, want %q", modelPath, wantModelPath)
	}
	if modelPath == permissionPath {
		t.Fatalf("model and permission paths are identical: %q", modelPath)
	}
	digest := sha256.Sum256([]byte(root))
	wantPermissionPath := filepath.Join(home, ".looprig", "coderig", "workspaces", hex.EncodeToString(digest[:]), permissionFileName)
	if permissionPath != wantPermissionPath {
		t.Fatalf("permission path = %q, want %q", permissionPath, wantPermissionPath)
	}

	if err := os.MkdirAll(filepath.Dir(modelPath), 0o700); err != nil {
		t.Fatalf("create model directory: %v", err)
	}
	modelBytes := []byte(validLMStudioModelConfig)
	if err := os.WriteFile(modelPath, modelBytes, 0o600); err != nil {
		t.Fatalf("write models.json: %v", err)
	}
	modelBefore, err := os.Stat(modelPath)
	if err != nil {
		t.Fatalf("stat models.json: %v", err)
	}
	if _, err := os.Stat(permissionPath); !os.IsNotExist(err) {
		t.Fatalf("permission file exists before model load: %v", err)
	}

	_, err = loadProductionModelsFrom(modelPath, func(model.Model, auth.APIKey) (inference.Client, error) {
		return &fakeLLM{}, nil
	})
	if err != nil {
		t.Fatalf("loadProductionModelsFrom: %v", err)
	}
	if _, err := os.Stat(permissionPath); !os.IsNotExist(err) {
		t.Fatalf("model load created or touched permission file: %v", err)
	}

	access, err := buildSessionAccess(Config{AccessProfile: AccessReadOnly}, root, true)
	if err != nil {
		t.Fatalf("buildSessionAccess(interactive): %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })
	ctx := loop.WithApprovalRequester(mustLoopProvenance(t), func(context.Context, gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		return gate.ApprovalApproveAlwaysWorkspace, nil
	})
	resolution, err := access.gate.Authorize(ctx, commandRequest(t, root, "git status --short"))
	if err != nil {
		t.Fatalf("Authorize(git status): %v", err)
	}
	if !resolution.Approved {
		t.Fatal("interactive approval did not approve the Generic command")
	}
	if _, err := os.Stat(permissionPath); err != nil {
		t.Fatalf("interactive approval did not persist hashed workspace permissions: %v", err)
	}

	modelAfterBytes, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatalf("read models.json after approval: %v", err)
	}
	modelAfter, err := os.Stat(modelPath)
	if err != nil {
		t.Fatalf("stat models.json after approval: %v", err)
	}
	if !bytes.Equal(modelAfterBytes, modelBytes) || !modelAfter.ModTime().Equal(modelBefore.ModTime()) || modelAfter.Mode() != modelBefore.Mode() {
		t.Fatal("interactive approval created or modified models.json")
	}
}

// TestAcceptanceHeadlessPermissionFileLoads proves headless assembly loads an
// explicit read-only permission file, fails startup on a missing or malformed
// file, and never searches HOME. A valid file is produced through the product
// interactive store (round-trip), then loaded through the product headless config.
func TestAcceptanceHeadlessPermissionFileLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, permissionFileName)

	// Author a valid file by persisting a product family rule through the
	// interactive store, so the read-only load exercises a real, well-formed file.
	writeStore, _, err := permission.NewWorkspaceStore(permission.Config{Path: path, FamilyEligible: productFamilyEligibility()})
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	family := permission.ProposeCommandCandidate("git log --oneline", productFamilyEligibility())
	if err := writeStore.WriteRules(context.Background(), []tool.RuleCandidate{{
		Kind:        permission.CapabilityCommandExecute,
		Match:       family,
		Description: "allow `git log`",
		GrantClass:  tool.GrantClassCommandStart,
		GrantTarget: "git log --oneline",
	}}); err != nil {
		t.Fatalf("WriteRules: %v", err)
	}

	// Load through the product headless config: the explicit path is honored and
	// no HOME is searched.
	cfg, err := headlessPermissionConfig(path)
	if err != nil {
		t.Fatalf("headlessPermissionConfig(%q): %v", path, err)
	}
	if cfg.Path != path {
		t.Fatalf("headless config path = %q, want the explicit %q (no HOME search)", cfg.Path, path)
	}
	store, _, err := permission.NewReadOnlyStore(cfg)
	if err != nil {
		t.Fatalf("NewReadOnlyStore(valid file): %v", err)
	}
	matched, err := store.MatchesAllow(context.Background(), tool.Requirement{
		Kind:  permission.CapabilityCommandExecute,
		Match: "git log -n 2",
	})
	if err != nil {
		t.Fatalf("MatchesAllow: %v", err)
	}
	if !matched {
		t.Error("loaded read-only file did not admit the persisted git-log family rule")
	}

	// Drive the explicit file through assembled session access as well. HOME
	// points elsewhere, proving headless assembly does not discover or write an
	// interactive workspace store.
	elsewhere := t.TempDir()
	t.Setenv("HOME", elsewhere)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	root := canonicalTempDir(t)
	access, err := buildHeadlessAccess(Config{AccessProfile: AccessReadOnly}, root, path)
	if err != nil {
		t.Fatalf("buildHeadlessAccess(explicit permission file): %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })
	if got := observeGate(t, mustLoopProvenance(t), access.gate, commandRequest(t, root, "git log -n 2")); got != outcomeAllowed {
		t.Fatalf("assembled headless Generic outcome = %d, want persisted allow", got)
	}
	interactivePath, err := defaultPermissionsPath(filepath.Join(elsewhere, ".looprig"), root)
	if err != nil {
		t.Fatalf("defaultPermissionsPath: %v", err)
	}
	if _, err := os.Stat(interactivePath); !os.IsNotExist(err) {
		t.Fatalf("headless access searched or wrote interactive permissions: %v", err)
	}

	// A malformed file fails startup (fail closed) rather than loading loosely.
	badPath := filepath.Join(t.TempDir(), permissionFileName)
	if err := os.WriteFile(badPath, []byte("this is not json"), 0o600); err != nil {
		t.Fatalf("write malformed file: %v", err)
	}
	badCfg, err := headlessPermissionConfig(badPath)
	if err != nil {
		t.Fatalf("headlessPermissionConfig(bad): %v", err)
	}
	if _, _, err := permission.NewReadOnlyStore(badCfg); err == nil {
		t.Error("NewReadOnlyStore(malformed) = nil error; want fail-closed startup failure")
	}

	// A missing configured file fails startup too.
	missingCfg, err := headlessPermissionConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("headlessPermissionConfig(missing): %v", err)
	}
	if _, _, err := permission.NewReadOnlyStore(missingCfg); err == nil {
		t.Error("NewReadOnlyStore(missing) = nil error; want fail-closed startup failure")
	}
}

// TestAcceptanceEgressFailureSurfacedAtAssembly proves an organization-proxy
// misconfiguration fails CLOSED at CodeRig's session-access assembly (no silent
// direct fallback, no leaked session): a specific NO_PROXY exception alongside an
// upstream proxy, and a malformed upstream URL, both abort buildHeadlessAccess.
// The deep proxy connection/auth failure at run time is the sandbox proxy's
// responsibility; this asserts CodeRig surfaces the fail-closed contract.
func TestAcceptanceEgressFailureSurfacedAtAssembly(t *testing.T) {
	root := canonicalTempDir(t)

	t.Run("upstream_with_specific_no_proxy", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "http://proxy.corp.example:3128")
		t.Setenv("NO_PROXY", "internal.example,localhost")
		access, err := buildHeadlessAccess(Config{AccessProfile: AccessTrusted}, root)
		if err == nil {
			_ = access.Close()
			t.Fatal("assembly with upstream + specific NO_PROXY succeeded; want fail-closed")
		}
		if access != nil {
			t.Fatalf("failed assembly returned a non-nil session access: %+v", access)
		}
	})

	t.Run("malformed_upstream", func(t *testing.T) {
		t.Setenv("HTTPS_PROXY", "://bob:secret@:notaport")
		t.Setenv("NO_PROXY", "")
		access, err := buildHeadlessAccess(Config{AccessProfile: AccessTrusted}, root)
		if err == nil {
			_ = access.Close()
			t.Fatal("assembly with malformed upstream succeeded; want fail-closed")
		}
	})
}

// TestAcceptanceNewRestoreAuthorityParity proves the single assembly path produces
// the SAME effective authority for two independent opens of the same profile over
// the same workspace: identical access-config digest and identical Generic policy
// revisions. New and restore share this path (openRuntimeAgent), so a restore under
// the same configuration reconstructs identical authority; the drift rejection is
// covered by TestRestoreRejectsAccessProfileDrift.
func TestAcceptanceNewRestoreAuthorityParity(t *testing.T) {
	root := canonicalTempDir(t)

	first, err := buildHeadlessAccess(Config{AccessProfile: AccessTrusted}, root)
	if err != nil {
		t.Fatalf("buildHeadlessAccess(first): %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := buildHeadlessAccess(Config{AccessProfile: AccessTrusted}, root)
	if err != nil {
		t.Fatalf("buildHeadlessAccess(second): %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if first.configRev != second.configRev {
		t.Errorf("access-config digest differs across identical opens:\n%s\n%s", first.configRev, second.configRev)
	}
	if first.policyRev != second.policyRev {
		t.Errorf("Generic policy revision differs: %q vs %q", first.policyRev, second.policyRev)
	}
	if first.profileName != second.profileName {
		t.Errorf("profile name differs: %q vs %q", first.profileName, second.profileName)
	}
}

// TestAcceptanceFixedProfileInPresentationMetadata proves the session-fixed access
// profile surfaces through the runtime agent's presentation METADATA (the TUI's
// SessionPresenter), distinct per selected profile, while the session identity is
// preserved (a stable, non-zero session ID). The user-facing startup banner stays
// the fixed product name independent of the profile — pinned by the command's
// TestRunPreservesPublicIdentity; the profile is metadata, never the banner.
func TestAcceptanceFixedProfileInPresentationMetadata(t *testing.T) {
	for _, profile := range []AccessProfile{AccessReadOnly, AccessTrusted} {
		t.Run(string(profile), func(t *testing.T) {
			stores, err := openTestStores(t)
			if err != nil {
				t.Fatalf("openStores: %v", err)
			}
			agent, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(testModel()), Config{AccessProfile: profile}, stores, t.TempDir())
			if err != nil {
				t.Fatalf("newSessionOverStores(%q): %v", profile, err)
			}
			t.Cleanup(func() { _ = agent.Close(context.Background()) })

			presentation := agent.SessionPresentation()
			if presentation.ProfileName != string(profile) {
				t.Errorf("SessionPresentation().ProfileName = %q, want %q", presentation.ProfileName, profile)
			}
			if agent.SessionID().IsZero() {
				t.Error("session ID is zero; the session identity must be preserved")
			}
		})
	}
}

// TestAcceptanceAgentShutdownClosesExecutors proves the runtime agent closes the
// session's executor sets at shutdown: a materialized executor is released and the
// set is closed exactly once, so a post-shutdown resolution fails closed and a
// direct access Close is a no-op. This is the agent-level counterpart to the
// access-level idempotence in TestSessionAccessCloseIsIdempotent.
func TestAcceptanceAgentShutdownClosesExecutors(t *testing.T) {
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openStores: %v", err)
	}
	agent, err := newSessionOverStores(context.Background(), &fakeLLM{}, newModelFactoryFor(testModel()), Config{}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores: %v", err)
	}
	access := agent.access
	if _, err := access.set.For("live-loop"); err != nil {
		t.Fatalf("set.For before shutdown: %v", err)
	}

	if err := agent.Close(context.Background()); err != nil {
		t.Fatalf("agent.Close: %v", err)
	}

	// Shutdown closed the executor sets: resolving now fails closed.
	if _, err := access.set.For("live-loop"); !errors.Is(err, sandbox.ErrExecutorSetClosed) {
		t.Errorf("set.For after shutdown = %v, want ErrExecutorSetClosed", err)
	}
	// The sets were closed exactly once; a direct Close is now a no-op.
	if err := access.Close(); err != nil {
		t.Errorf("access.Close after shutdown (must be a no-op): %v", err)
	}
}
