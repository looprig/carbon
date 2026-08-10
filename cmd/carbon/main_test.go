package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/tui"
	"github.com/looprig/tui/runtime"
)

// TestOpenThunkSelectsResumeOnlyOnce proves the process opener restores only its first
// session and all later serialized /clear reopens select a fresh session.
// The function-shaped seam keeps SessionStoreFactory as the production owner while making
// the NewSession/RestoreSession selector decision observable without opening a real store.
func TestOpenThunkSelectsResumeOnlyOnce(t *testing.T) {
	resume := mustUUID(t)
	const opens = 3
	var selectors []carbon.SessionSelector
	openSession := func(_ context.Context, sel carbon.SessionSelector, _ carbon.Config) (tui.Agent, error) {
		selectors = append(selectors, sel)
		return nil, nil
	}
	open := openThunk(openSession, resume, carbon.Config{})

	for range opens {
		if _, err := open(context.Background()); err != nil {
			t.Errorf("open: %v", err)
		}
	}

	if len(selectors) != opens {
		t.Fatalf("opens = %d, want %d", len(selectors), opens)
	}
	var restores int
	for _, sel := range selectors {
		if sel.Resume == resume {
			restores++
		} else if !sel.Resume.IsZero() {
			t.Errorf("unexpected resume selector %v", sel.Resume)
		}
	}
	if restores != 1 {
		t.Errorf("restore selections = %d, want exactly 1; all /clear opens must be new", restores)
	}
}

// TestOpenThunkSelectsNewForLaunchAndClear covers the no-resume launch explicitly: both the
// initial open and the later /clear reopen carry a zero selector, which SessionStoreFactory
// maps to Rig.NewSession.
func TestOpenThunkSelectsNewForLaunchAndClear(t *testing.T) {
	var selectors []carbon.SessionSelector
	openSession := func(_ context.Context, sel carbon.SessionSelector, _ carbon.Config) (tui.Agent, error) {
		selectors = append(selectors, sel)
		return nil, nil
	}
	open := openThunk(openSession, uuid.UUID{}, carbon.Config{})
	for i := 0; i < 2; i++ {
		if _, err := open(context.Background()); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	for i, sel := range selectors {
		if !sel.Resume.IsZero() {
			t.Errorf("selector %d resume = %v, want zero for NewSession", i, sel.Resume)
		}
	}
}

// TestRunCLIClosesSessionBeforeStore pins the process ownership order: the CLI runtime
// closes the live session before it returns, then the composition root closes the shared
// SessionStoreFactory. This remains true for a non-zero runtime exit code.
func TestRunCLIClosesSessionBeforeStore(t *testing.T) {
	var order []string
	record := func(step string) {
		order = append(order, step)
	}
	agent := &orderingAgent{close: func() { record("session") }}
	open := func(context.Context) (tui.Agent, error) { return agent, nil }
	runner := func(ctx context.Context, open tui.OpenAgent, _ runtime.Banner) int {
		live, err := open(ctx)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := live.Close(ctx); err != nil {
			t.Fatalf("close live session: %v", err)
		}
		return exitFailed
	}
	closeStore := func() error { record("store"); return nil }

	if got := runCLIWithStore(context.Background(), open, runtime.Banner{Name: bannerName}, runner, closeStore); got != exitFailed {
		t.Fatalf("exit = %d, want %d", got, exitFailed)
	}
	if got, want := strings.Join(order, ","), "session,store"; got != want {
		t.Fatalf("shutdown order = %q, want %q", got, want)
	}
}

// TestRunCLIStoreCloseErrorFails maps shared-store teardown failure to a process failure even
// when the CLI runtime itself completed successfully.
func TestRunCLIStoreCloseErrorFails(t *testing.T) {
	runner := func(context.Context, tui.OpenAgent, runtime.Banner) int { return exitOK }
	closeStore := func() error { return errors.New("close store") }
	if got := runCLIWithStore(context.Background(), nil, runtime.Banner{}, runner, closeStore); got != exitFailed {
		t.Fatalf("exit = %d, want %d when SessionStoreFactory.Close fails", got, exitFailed)
	}
}

// orderingAgent is the smallest complete migrated CLI contract used by the process-order
// test. The explicit Active/loop-targeted image methods keep this command package
// compiled against the same multi-loop surface as the production session adapter.
type orderingAgent struct{ close func() }

func (*orderingAgent) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (*orderingAgent) SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (*orderingAgent) CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error) {
	return uuid.UUID{}, nil
}
func (*orderingAgent) ActiveLoopID() uuid.UUID                                 { return uuid.UUID{} }
func (*orderingAgent) SessionID() uuid.UUID                                    { return uuid.UUID{0x42} }
func (*orderingAgent) Interrupt(context.Context) (bool, error)                 { return false, nil }
func (a *orderingAgent) Close(context.Context) error                           { a.close(); return nil }
func (*orderingAgent) AcceptsImages(uuid.UUID) bool                            { return false }
func (*orderingAgent) Subscribe(event.EventFilter) (event.Subscription, error) { return nil, nil }
func (*orderingAgent) ReplayBacklog(context.Context) ([]event.Event, error)    { return nil, nil }
func (*orderingAgent) Approve(context.Context, uuid.UUID, uuid.UUID, gate.ApprovalAction) error {
	return nil
}
func (*orderingAgent) Deny(context.Context, uuid.UUID, uuid.UUID) error                  { return nil }
func (*orderingAgent) ProvideAnswer(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }
func (*orderingAgent) RespondGate(context.Context, gate.ID, string, map[string]json.RawMessage) error {
	return nil
}

var _ tui.Agent = (*orderingAgent)(nil)

// TestRunPreservesPublicIdentity pins the process-facing name independently from the rig's
// internal Carbon identity.
func TestRunPreservesPublicIdentity(t *testing.T) {
	if bannerName != "Carbon" {
		t.Errorf("bannerName = %q, want %q", bannerName, "Carbon")
	}
}

// TestRunHasNoServeAdapter guards the process boundary: the command and Rig
// packages do not directly import the generic harness HTTP layer. A future HTTP entry point
// should pass the real rig to serve.Handler instead of adding a Carbon Runner adapter here.
func TestRunHasNoServeAdapter(t *testing.T) {
	moduleRoot := filepath.Clean("../..")
	for _, relRoot := range []string{"cmd/carbon", "."} {
		root := filepath.Join(moduleRoot, relRoot)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if importPath == "github.com/looprig/harness/pkg/serve" {
					t.Errorf("%s imports harness serve; no Carbon serve adapter belongs in this migration", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}
}

// TestParseFlags covers the Carbon CLI flag parser: --list, --resume <uuid>,
// --data-dir, and the boundary validation (an invalid/empty resume id fails at the
// boundary, not deep in the wiring; --list and --resume are mutually exclusive). Carbon has no
// positional agent name (it is one fixed Carbon session), so an unexpected positional arg is rejected.
func TestParseFlags(t *testing.T) {
	t.Parallel()

	validID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name        string
		args        []string
		wantList    bool
		wantResume  uuid.UUID
		wantDataDir string
		wantProfile carbon.AccessProfile
		wantAck     bool
		wantErr     bool
	}{
		{name: "no flags → new session", args: nil, wantProfile: carbon.AccessReadOnly},
		{name: "list flag", args: []string{"-list"}, wantList: true, wantProfile: carbon.AccessReadOnly},
		{name: "list flag double dash", args: []string{"--list"}, wantList: true, wantProfile: carbon.AccessReadOnly},
		{name: "resume a session", args: []string{"-resume", validID.String()}, wantResume: validID, wantProfile: carbon.AccessReadOnly},
		{name: "resume double dash", args: []string{"--resume", validID.String()}, wantResume: validID, wantProfile: carbon.AccessReadOnly},
		{name: "removed runtime-skills flag rejected", args: []string{"--runtime-skills"}, wantErr: true},
		{name: "removed greeting flag rejected", args: []string{"--greeting"}, wantErr: true},
		{name: "removed security-mode flag rejected", args: []string{"--security-mode", "write"}, wantErr: true},
		{name: "data-dir default empty", args: nil, wantDataDir: "", wantProfile: carbon.AccessReadOnly},
		{name: "data-dir flag", args: []string{"-data-dir", "/tmp/carbon-store"}, wantDataDir: "/tmp/carbon-store", wantProfile: carbon.AccessReadOnly},
		{name: "data-dir double dash", args: []string{"--data-dir", "/tmp/carbon-store"}, wantDataDir: "/tmp/carbon-store", wantProfile: carbon.AccessReadOnly},
		{name: "data-dir whitespace trimmed to empty", args: []string{"-data-dir", "   "}, wantDataDir: "", wantProfile: carbon.AccessReadOnly},
		{name: "data-dir with resume", args: []string{"-data-dir", "/tmp/s", "-resume", validID.String()}, wantResume: validID, wantDataDir: "/tmp/s", wantProfile: carbon.AccessReadOnly},
		{name: "access-profile default is readonly", args: nil, wantProfile: carbon.AccessReadOnly},
		{name: "access-profile trusted", args: []string{"--access-profile", "trusted"}, wantProfile: carbon.AccessTrusted},
		{name: "access-profile readonly explicit", args: []string{"-access-profile", "readonly"}, wantProfile: carbon.AccessReadOnly},
		{name: "access-profile case-insensitive", args: []string{"--access-profile", "TRUSTED"}, wantProfile: carbon.AccessTrusted},
		{name: "access-profile unknown rejected", args: []string{"--access-profile", "write"}, wantErr: true},
		{name: "unconfined requires acknowledgement", args: []string{"--access-profile", "unconfined"}, wantErr: true},
		{name: "unconfined with acknowledgement", args: []string{"--access-profile", "unconfined", "--acknowledge-unconfined"}, wantProfile: carbon.AccessUnconfined, wantAck: true},
		{name: "acknowledgement without unconfined is harmless", args: []string{"--acknowledge-unconfined"}, wantProfile: carbon.AccessReadOnly, wantAck: true},
		{name: "invalid resume id rejected", args: []string{"-resume", "not-a-uuid"}, wantErr: true},
		{name: "empty resume id rejected", args: []string{"-resume", ""}, wantErr: true},
		{name: "list and resume are mutually exclusive", args: []string{"-list", "-resume", validID.String()}, wantErr: true},
		{name: "unknown flag rejected", args: []string{"-nope"}, wantErr: true},
		{name: "unexpected positional rejected", args: []string{"extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFlags(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.list != tt.wantList {
				t.Errorf("list = %v, want %v", got.list, tt.wantList)
			}
			if got.resume != tt.wantResume {
				t.Errorf("resume = %v, want %v", got.resume, tt.wantResume)
			}
			if got.dataDir != tt.wantDataDir {
				t.Errorf("dataDir = %q, want %q", got.dataDir, tt.wantDataDir)
			}
			if got.accessProfile != tt.wantProfile {
				t.Errorf("accessProfile = %q, want %q", got.accessProfile, tt.wantProfile)
			}
			if got.acknowledgeUnconfined != tt.wantAck {
				t.Errorf("acknowledgeUnconfined = %v, want %v", got.acknowledgeUnconfined, tt.wantAck)
			}
		})
	}
}

func TestParseCredentialFlagsAndRejectMixedCommands(t *testing.T) {
	flags, err := parseFlags([]string{"--credentials-list"})
	if err != nil {
		t.Fatalf("parse credential list: %v", err)
	}
	if !flags.credentialList || flags.credentialLogin != "" || flags.credentialLogout != "" {
		t.Fatalf("credential list flags = %+v", flags)
	}
	flags, err = parseFlags([]string{"--login", "OpenAI"})
	if err != nil {
		t.Fatalf("parse login: %v", err)
	}
	if flags.credentialLogin != "OpenAI" {
		t.Fatalf("credential login = %q, want OpenAI", flags.credentialLogin)
	}
	flags, err = parseFlags([]string{"credentials", "login", "anthropic"})
	if err != nil || flags.credentialLogin != "anthropic" {
		t.Fatalf("subcommand login = %+v, err=%v", flags, err)
	}
	flags, err = parseFlags([]string{"login", "openai"})
	if err != nil || flags.credentialLogin != "openai" {
		t.Fatalf("direct login = %+v, err=%v", flags, err)
	}
	flags, err = parseFlags([]string{"credential", "logout", "credential://openai/personal"})
	if err != nil || flags.credentialLogout != "credential://openai/personal" {
		t.Fatalf("subcommand logout = %+v, err=%v", flags, err)
	}
	flags, err = parseFlags([]string{"logout", "credential://openai/personal"})
	if err != nil || flags.credentialLogout != "credential://openai/personal" {
		t.Fatalf("direct logout = %+v, err=%v", flags, err)
	}
	if _, err := parseFlags([]string{"--list", "--credentials-list"}); err == nil {
		t.Fatal("mixed session/credential command error = nil")
	}
	if _, err := parseFlags([]string{"--login", "openai", "--logout", "credential://openai/personal"}); err == nil {
		t.Fatal("mixed credential command error = nil")
	}
}

func TestPrintCredentialsRedactsSensitiveFieldsByConstruction(t *testing.T) {
	var out bytes.Buffer
	err := printCredentials(&out, []carbon.CredentialSummary{{
		Reference: "credential://openai/personal",
		Provider:  "openai",
		Transport: "responses",
		Scheme:    "api_key",
		Usage:     "metered_api",
		Status:    "configured",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "credential://openai/personal") || !strings.Contains(got, "status=configured") {
		t.Fatalf("credential output = %q, missing safe fields", got)
	}
	for _, forbidden := range []string{"sk-", "token", "secret", "@", "/credentials/"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("credential output = %q, contains forbidden %q", got, forbidden)
		}
	}
}

func TestPrintCredentialLogoutDoesNotClaimRemoteRevocation(t *testing.T) {
	var out bytes.Buffer
	if err := printCredentialLogout(&out, carbon.CredentialLogoutOutcome{
		Reference:           "credential://openai/personal",
		LocalCatalogDeleted: true,
		LocalStateDeleted:   true,
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "local_catalog=deleted") || !strings.Contains(got, "local_state=deleted") || !strings.Contains(got, "remote_revocation=not-attempted") {
		t.Fatalf("logout output = %q, missing separate local/remote outcomes", got)
	}
	if strings.Contains(got, "revoked") {
		t.Fatalf("logout output falsely claimed remote revocation: %q", got)
	}
}

// TestFlagParseErrorIsTyped proves FlagParseError carries its reason and unwraps its cause,
// so the boundary failure is errors.As-recoverable rather than a bare string.
func TestFlagParseErrorIsTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       *FlagParseError
		wantMsg   string
		wantCause bool
	}{
		{name: "reason only", err: &FlagParseError{Reason: "boom"}, wantMsg: "carbon: boom"},
		{
			name:      "reason with cause",
			err:       &FlagParseError{Reason: "bad id", Cause: errStub{}},
			wantMsg:   "carbon: bad id: stub",
			wantCause: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", got, tt.wantMsg)
			}
			if (tt.err.Unwrap() != nil) != tt.wantCause {
				t.Errorf("Unwrap() non-nil = %v, want %v", tt.err.Unwrap() != nil, tt.wantCause)
			}
		})
	}
}

// errStub is a minimal error for the cause-chaining assertion.
type errStub struct{}

func (errStub) Error() string { return "stub" }

// TestPrintSessions proves the --list formatter renders each catalog row (id, status,
// last-active, title) in the order given, shows "(untitled)" for a title-less session, and
// prints a friendly note for an empty store. Ordering is the catalog's responsibility; the CLI
// prints in the order it receives.
func TestPrintSessions(t *testing.T) {
	t.Parallel()

	newer := mustUUID(t)
	older := mustUUID(t)
	untitled := mustUUID(t)
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		metas     []sessionstore.SessionMeta
		wantNote  string   // exact single-line note (empty store)
		wantOrder []string // substrings that must appear, in this order
		wantParts []string // substrings that must be present
	}{
		{
			name:     "empty store prints a friendly note",
			metas:    nil,
			wantNote: "no sessions yet\n",
		},
		{
			name: "rows render newest-first in the order given",
			metas: []sessionstore.SessionMeta{
				{SessionID: newer, Title: "newer work", Status: sessionstore.StatusActive, LastActiveAt: now},
				{SessionID: older, Title: "older work", Status: sessionstore.StatusStopped, LastActiveAt: now.Add(-time.Hour)},
			},
			wantOrder: []string{newer.String(), older.String()},
			wantParts: []string{"newer work", "older work", "active", "stopped", now.Format(time.RFC3339)},
		},
		{
			name: "untitled session shows a placeholder",
			metas: []sessionstore.SessionMeta{
				{SessionID: untitled, Status: sessionstore.StatusActive, LastActiveAt: now},
			},
			wantParts: []string{untitled.String(), "(untitled)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := printSessions(&buf, tt.metas); err != nil {
				t.Fatalf("printSessions: %v", err)
			}
			out := buf.String()
			if tt.wantNote != "" && out != tt.wantNote {
				t.Fatalf("printSessions(empty) = %q, want %q", out, tt.wantNote)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(out, part) {
					t.Errorf("output missing %q:\n%s", part, out)
				}
			}
			prev := -1
			for _, want := range tt.wantOrder {
				i := strings.Index(out, want)
				if i < 0 {
					t.Fatalf("output missing ordered token %q:\n%s", want, out)
				}
				if i < prev {
					t.Errorf("token %q out of order:\n%s", want, out)
				}
				prev = i
			}
		})
	}
}

// mustUUID mints a random UUID for a test row or fails the test.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}
