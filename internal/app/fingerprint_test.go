package app

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
)

func compactionFingerprintFor(t *testing.T, root string, client *fakeLLM, policy conversationContextPolicy, registration conversationHustleRegistration) event.ConfigFingerprint {
	t.Helper()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definitions, err := swarmDefinitionsWithContextPolicy(client, testModel(), cfg, policy, access)
	if err != nil {
		t.Fatalf("swarmDefinitionsWithContextPolicy() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	assembly, err := buildRigWithRegistration(
		definitions, stores, root, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota}, registration,
		permissionReviewRegistration{},
	)
	if err != nil {
		t.Fatalf("buildRigWithRegistration() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	return durableSessionFingerprint(t, stores, controller.SessionID())
}

func durableSessionFingerprint(t *testing.T, stores *swarmStores, sessionID uuid.UUID) event.ConfigFingerprint {
	t.Helper()
	replayer, err := stores.session.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer() error = %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{From: journal.Beginning()})
	if err != nil {
		t.Fatalf("replayer.Open() error = %v", err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, _, nextErr := cursor.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			t.Fatal("durable log has no SessionStarted")
		}
		if nextErr != nil {
			t.Fatalf("cursor.Next() error = %v", nextErr)
		}
		if started, ok := ev.(event.SessionStarted); ok {
			return started.Config
		}
	}
}

func compactionDefinitionForFingerprint(t *testing.T, promptRevision, parserRevision string) hustle.Definition {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName(conversationCompactionName),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithCurrentLoopModel(),
		hustle.WithTimeout(conversationCompactionTimeout),
		hustle.WithLimits(hustle.Limits{InputBytes: conversationCompactionInputBytes, OutputBytes: conversationCompactionOutputBytes}),
		hustle.WithSystemPrompt(conversationCompactionPrompt, promptRevision),
		hustle.WithPolicyRevision(parserRevision),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition
}

func TestCompactionCompositionFingerprintSensitivityAndSecretExclusion(t *testing.T) {
	t.Parallel()

	basePolicy, err := newConversationContextPolicy(testModel())
	if err != nil {
		t.Fatalf("newConversationContextPolicy() error = %v", err)
	}
	baseRegistration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatalf("newConversationHustleRegistration() error = %v", err)
	}
	root := t.TempDir()
	base := compactionFingerprintFor(t, root, &fakeLLM{credential: "secret-a"}, basePolicy, baseRegistration)

	tests := []struct {
		name         string
		client       *fakeLLM
		policy       conversationContextPolicy
		registration conversationHustleRegistration
		wantEqual    bool
	}{
		{name: "client credential excluded", client: &fakeLLM{credential: "secret-b"}, policy: basePolicy, registration: baseRegistration, wantEqual: true},
		{name: "compaction policy", client: &fakeLLM{}, policy: func() conversationContextPolicy { value := basePolicy; value.compaction.CompactAt--; return value }(), registration: baseRegistration},
		{name: "summary revision", client: &fakeLLM{}, policy: func() conversationContextPolicy {
			value := basePolicy
			value.summaryRevision = "coderig-summary-consumption-v2"
			return value
		}(), registration: baseRegistration},
		{name: "hustle prompt revision", client: &fakeLLM{}, policy: basePolicy, registration: conversationHustleRegistration{definition: compactionDefinitionForFingerprint(t, "coderig-compaction-prompt-v2", conversationCompactionParserRevision), limits: baseRegistration.limits}},
		{name: "hustle parser revision", client: &fakeLLM{}, policy: basePolicy, registration: conversationHustleRegistration{definition: compactionDefinitionForFingerprint(t, conversationCompactionPromptRevision, "harness-compaction-parser-v2"), limits: baseRegistration.limits}},
		{name: "hustle lane limits", client: &fakeLLM{}, policy: basePolicy, registration: func() conversationHustleRegistration {
			value := baseRegistration
			value.limits.AuditTimeout += time.Second
			return value
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := compactionFingerprintFor(t, root, tt.client, tt.policy, tt.registration)
			if equal := got.Equal(base); equal != tt.wantEqual {
				t.Errorf("ConfigFingerprint.Equal(base) = %v, want %v\ngot=%+v\nbase=%+v", equal, tt.wantEqual, got, base)
			}
		})
	}
}

// TestAgentFingerprintFields asserts the rig-level config-fingerprint fields the
// composition root injects via rig.WithFingerprintFields: AgentKind is the swarm+active-primer
// identity ("coderig:builder") and RuntimeSkills passes the human-set mode through verbatim. The
// workspace-root field is NOT set here — the rig's exclusive-workspace placement folds the
// canonical root into the fingerprint — so a restore still compares agent identity, skill
// mode, AND (via the placement) the repo root.
func TestAgentFingerprintFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want rig.ConfigFingerprintFields
	}{
		{
			name: "runtime skills off",
			cfg:  Config{RuntimeSkills: false},
			want: rig.ConfigFingerprintFields{AgentKind: "coderig:builder", RuntimeSkills: false},
		},
		{
			name: "runtime skills on",
			cfg:  Config{RuntimeSkills: true},
			want: rig.ConfigFingerprintFields{AgentKind: "coderig:builder", RuntimeSkills: true},
		},
		{
			name: "access profile and digest fold in",
			cfg:  Config{AccessProfile: AccessTrusted, AccessConfigRev: "coderig-access-v1:deadbeef"},
			want: rig.ConfigFingerprintFields{
				AgentKind:                 "coderig:builder",
				NativePermissionPolicyRev: "coderig-access-v1:deadbeef",
				AppFields:                 map[string]string{"access_profile": "trusted"},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agentFingerprintFields(tt.cfg)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("agentFingerprintFields = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestAccessConfigInvalidatesFingerprintFields proves the durable access
// configuration is drift-detecting at the rig-fingerprint boundary: a
// product-profile, reviewer-restriction, or egress-boundary change (all folded
// into AccessConfigRev), or the selected profile name, changes the rig-level
// fingerprint fields, so a restore with different authority is a mismatch rather
// than a silent authority change.
func TestAccessConfigInvalidatesFingerprintFields(t *testing.T) {
	t.Parallel()

	base := agentFingerprintFields(Config{AccessProfile: AccessReadOnly, AccessConfigRev: "rev-a"})

	if got := agentFingerprintFields(Config{AccessProfile: AccessReadOnly, AccessConfigRev: "rev-a"}); !reflect.DeepEqual(got, base) {
		t.Fatalf("identical access config produced different fields:\n got=%+v\nbase=%+v", got, base)
	}
	// A changed access digest (profile/reviewer/egress change) must invalidate.
	if got := agentFingerprintFields(Config{AccessProfile: AccessReadOnly, AccessConfigRev: "rev-b"}); reflect.DeepEqual(got, base) {
		t.Error("changed AccessConfigRev did not change the fingerprint fields")
	}
	// A changed selected profile name must invalidate.
	if got := agentFingerprintFields(Config{AccessProfile: AccessTrusted, AccessConfigRev: "rev-a"}); reflect.DeepEqual(got, base) {
		t.Error("changed AccessProfile did not change the fingerprint fields")
	}
}

// TestAgentKindFormat pins the AgentKind to "<swarm>:<active primer>" so a rename of
// the builder's attribution name is reflected in the fingerprint (and a prior/other session,
// with a different or empty AgentKind, cannot resume as CodeRig).
func TestAgentKindFormat(t *testing.T) {
	t.Parallel()
	want := "coderig:" + string(builder.Name)
	if agentKind != want {
		t.Errorf("agentKind = %q, want %q", agentKind, want)
	}
	if agentKind != "coderig:builder" {
		t.Errorf("agentKind = %q, want %q", agentKind, "coderig:builder")
	}
}
