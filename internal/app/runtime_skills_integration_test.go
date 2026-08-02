//go:build integration

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/inference"
)

// runtime_skills_integration_test.go is the P2b Phase 3c END-TO-END acceptance: with
// RuntimeSkills ON and a real on-disk <root>/.skills/<name>/SKILL.md, the builder
// delegates to a builder child, the child calls Skill{name:"<workspace-skill>"}, the
// workspace load surfaces a HUMAN-GATED SkillLoadRequest (ScopeOnce) attributed to the
// delegate loop, and after Approve the snapshot body is returned as the tool result.
//
// workspaceSkillBody is the marker phrase planted in the on-disk workspace SKILL.md so a
// future rewrite can prove the APPROVED snapshot body (not an error) is what Skill returned.
const workspaceSkillBody = "WORKSPACE-SKILL-MARKER: project-local checklist"

// writeWorkspaceSkill writes a valid <root>/.skills/<name>/SKILL.md (frontmatter + body) and
// returns name.
func writeWorkspaceSkill(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", dir, err)
	}
	doc := "---\nname: " + name + "\ndescription: A project-local workspace skill.\n---\n\n# Local\n\n" + workspaceSkillBody + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile(SKILL.md) error = %v", err)
	}
	return name
}

func TestRuntimeSkillsWorkspaceLoadGatedEndToEnd(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	t.Chdir(root)
	name := writeWorkspaceSkill(t, root, "project-checklist")
	phase := "initial"
	step := 0
	var skillResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if strings.Contains(req.System, `<role name="`+string(builder.Name)+`">`) && phase == "initial" && step == 0 {
			step++
			return toolCall("skill-delegate", `{"agent":"builder","message":"prepare for restore","wait":true}`), nil
		}
		if phase == "initial" {
			return finalText("runtime skill child prepared"), nil
		}
		if step == 0 {
			step++
			return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: "skill-load", Name: "Skill", InputJSON: fmt.Sprintf(`{"name":%q}`, name)}}, nil
		}
		skillResult = lastToolText(req)
		return finalText("delegate consumed skill"), nil
	}
	f1, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := f1.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{}, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	sid := a1.SessionID()
	_, initialEvents := runManagedTurnObserved(t, a1, "create runtime-skill delegate")
	var childID uuid.UUID
	for _, ev := range initialEvents {
		if started, ok := ev.(event.LoopStarted); ok && !started.Cause.Coordinates.LoopID.IsZero() {
			childID = started.LoopID
		}
	}
	if childID.IsZero() {
		t.Fatal("runtime-skill delegate missing before restore")
	}
	if err := a1.sess.SetActiveLoop(context.Background(), childID); err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := a1.sess.(interface{ WaitIdle(context.Context) error }).WaitIdle(waitCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := a1.sess.CheckpointWorkspace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}

	phase, step = "restored", 0
	f2, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f2.Close() })
	a, err := f2.openWithClient(context.Background(), client, newModelFactory(), SessionSelector{Resume: sid}, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if a.ActiveLoopID() != childID {
		t.Fatalf("restored runtime-skill active loop = %v, want %v", a.ActiveLoopID(), childID)
	}
	sub, err := a.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commandID, err := a.Submit(ctx, []content.Block{&content.TextBlock{Text: "delegate skill load"}})
	if err != nil {
		t.Fatal(err)
	}
	var turnID uuid.UUID
	approved := false
	var observed []event.Event
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("runtime-skill flow timed out after %s: %v", eventTypes(observed), ctx.Err())
		case delivery, ok := <-sub.Events():
			if !ok {
				t.Fatalf("runtime-skill subscription closed after %s", eventTypes(observed))
			}
			observed = append(observed, delivery.Event)
			switch ev := delivery.Event.(type) {
			case event.GateOpened:
				if ev.EventHeader().LoopID != childID || childID.IsZero() {
					t.Fatalf("workspace-skill gate loop = %v, want delegate %v", ev.EventHeader().LoopID, childID)
				}
				// The untrusted workspace skill load (context.load) is Gated by CodeRig's
				// product access source, so it surfaces one combined approval on the delegate
				// loop; approving it once admits the exact skill identity.
				if err := a.Approve(ctx, childID, ev.Gate.Subject.ToolExecutionID, gate.ApprovalApprove); err != nil {
					t.Fatal(err)
				}
				approved = true
			case event.TurnStarted:
				if ev.Cause.CommandID == commandID {
					turnID = ev.TurnID
				}
			case event.TurnDone:
				if ev.TurnID == turnID && !turnID.IsZero() {
					if !approved {
						t.Fatal("turn completed without a workspace-skill approval")
					}
					if !strings.Contains(skillResult, workspaceSkillBody) {
						t.Fatalf("skill result = %q", skillResult)
					}
					return
				}
			case event.TurnFailed:
				t.Fatalf("runtime-skill turn failed on loop %v: %v", ev.EventHeader().LoopID, ev.Err)
			}
		}
	}
}
