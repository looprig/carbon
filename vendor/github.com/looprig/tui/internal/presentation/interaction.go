package presentation

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/tui/components"
	inputpkg "github.com/looprig/tui/internal/input"
)

// otherChoice is the literal escape-hatch answer the 'o' accelerator sends in
// choice mode. It MUST equal tools.otherChoice (the AskUser validateAnswer
// contract): with choices present, an answer is valid only if it is a listed
// choice or this exact literal. It is duplicated here rather than imported because
// package tools is a higher layer (the tui must not depend on it).
const otherChoice = "other"

// interactionMode is the bottom surface's current mode. Compose is the default
// (editing the next message); the three prompt modes are entered when a prompt is
// pending and select how keys are routed (Task 8 implements that routing).
type interactionMode uint8

const (
	// modeCompose edits the next user message in the input box.
	modeCompose interactionMode = iota
	// modePermissionPrompt shows the active permission gate (approve/deny/scope).
	modePermissionPrompt
	// modeChoicePrompt shows an AskUser request with selectable choices.
	modeChoicePrompt
	// modeAnswerPrompt shows a free-text AskUser request (no choices).
	modeAnswerPrompt
	// modeFormPrompt shows a structured form gate (gate.KindForm): a field editor
	// the user fills in and submits.
	modeFormPrompt
	// modeOpenURLPrompt shows an open-url gate (gate.KindOpenURL): the origin being
	// authorized, and the completion decision. It has no editor — there is nothing
	// to fill in, only to confirm.
	modeOpenURLPrompt
)

// interactionModel owns the bottom interaction surface: the compose editor, the
// slash-completion panel, the FIFO queue of pending prompts (keyed by ToolExecutionID), and
// the saved compose draft restored when the queue drains. It is a value type
// driven Elm-style: Update/ApplyEvent return a new interactionModel. It holds NO
// agent reference — it only PRODUCES a typed uiAction for Screen to act on.
type interactionModel struct {
	mode          interactionMode
	pending       []prompt // FIFO; pending[0] is the active prompt
	input         components.InputBox
	slash         *components.SlashComplete // slash-command panel; nil = hidden
	files         *components.FileComplete  // @path completion panel; nil = hidden
	slashCommands []components.SlashCmd
	composeDraft  string // editor text saved when a prompt preempts compose
}

// newInteractionModel returns an idle model in compose mode with a focused input.
func newInteractionModel() interactionModel {
	return interactionModel{
		mode:          modeCompose,
		input:         components.NewInputBox(),
		slashCommands: append([]components.SlashCmd(nil), components.SlashCommands...),
	}
}

// cloneHead returns a copy of pending whose head element is a distinct value, so
// mutating index 0 of the result never writes through a slice the caller still
// holds. interactionModel is driven by value, but a slice header copy shares its
// backing array; the selection handlers mutate the head in place, so they must
// clone it first to stay sound under that value-copy contract. copy duplicates the
// slice of prompt value structs into a fresh backing array, so every element —
// head and tail — is an independent copy. pending must be non-empty.
func cloneHead(pending []prompt) []prompt {
	out := make([]prompt, len(pending))
	copy(out, pending)
	return out
}

// ActivePrompt returns the head (active) pending prompt, or nil when none pend.
// The pointer is into the model's own slice; callers must not retain it past the
// next ApplyEvent/pop.
func (m *interactionModel) ActivePrompt() *prompt {
	if len(m.pending) == 0 {
		return nil
	}
	return &m.pending[0]
}

// PendingCount is the number of queued prompts (active + waiting).
func (m interactionModel) PendingCount() int { return len(m.pending) }

// pendingGateLoops returns the distinct producing-loop ids of every currently pending
// prompt (permission or AskUser), so the modern loop bar can mark a gated loop with its
// "!" affordance WITHOUT stealing focus (design §Prompts: prompt-open must not interrupt
// the user's reading; the bar marker is how a non-focused loop signals it needs attention,
// and focus stays the user's to change). It is a READ over the existing pending FIFO — no
// new state — and returns nil when nothing pends so the common (no-gate) path allocates
// nothing. A loop with several pending prompts appears once (the marker is boolean per loop).
func (m interactionModel) pendingGateLoops() map[uuid.UUID]bool {
	if len(m.pending) == 0 {
		return nil
	}
	loops := make(map[uuid.UUID]bool, len(m.pending))
	for i := range m.pending {
		loops[m.pending[i].LoopID] = true
	}
	return loops
}

// ApplyEvent folds one turn-stream event into the interaction surface. A
// PermissionRequested/UserInputRequested enqueues a prompt (append-once by
// ToolExecutionID), stamped with its producing loop's id (ev.EventHeader().LoopID), and
// reveals the head; the terminal events clear only the FINISHING loop's pending
// prompts (ClearPromptsForLoop) and, if that drains the queue, restore compose —
// a sibling loop's pending gate is left intact (design §7). All other events are
// no-ops here (the transcript owns them). It returns the updated model by value.
func (m interactionModel) ApplyEvent(ev event.Event) interactionModel {
	switch ev := ev.(type) {
	case event.PermissionRequested:
		m.enqueueForLoop(promptFromPermission(ev.ToolExecutionID, ev.Request), ev.EventHeader().LoopID)
	case event.UserInputRequested:
		m.enqueueForLoop(promptFromUserInput(ev.ToolExecutionID, ev.Question, ev.Choices), ev.EventHeader().LoopID)
	case event.GateOpened:
		// ONLY host-raised gates are folded from GateOpened. A permission or AskUser
		// gate also opens one, but those already enqueue from their own per-kind
		// request event above — folding them here too would double-queue the same
		// prompt. A form or open-url gate has no such event: it is raised by an
		// integration host, and this envelope is the only thing the TUI ever sees of
		// it.
		switch ev.Gate.Kind {
		case gate.KindForm:
			m.enqueueForLoop(promptFromForm(ev.Gate), ev.EventHeader().LoopID)
		case gate.KindOpenURL:
			m.enqueueForLoop(promptFromOpenURL(ev.Gate), ev.EventHeader().LoopID)
		}
	case event.GateResolved:
		// A gate answered elsewhere (a policy timeout, another client) must not
		// leave a stale editor on screen asking for an answer already given.
		m = m.clearGate(ev.GateID)
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		m = m.ClearPromptsForLoop(ev.EventHeader().LoopID)
	}
	return m
}

// enqueueForLoop stamps p with its producing loop's id and enqueues it. The
// LoopID scopes terminal-event clearing per loop (ClearPromptsForLoop, design
// §7); it is set here at the single enqueue site rather than threaded through the
// prompt constructors, so prompt construction stays purely about the gate's
// view-model and the producer-identity concern lives at the event boundary.
func (m *interactionModel) enqueueForLoop(p prompt, loopID uuid.UUID) {
	p.LoopID = loopID
	m.enqueue(p)
}

// enqueue appends p unless the SAME gate is already pending (append-once: a re-delivered gate
// event must not double-queue). A gate's identity is (LoopID, ToolExecutionID), not the call id
// alone: distinct loops (the primary and its subagents) may surface gates whose ToolExecutionID
// coincides, and those are DIFFERENT gates that must both queue — deduping on the call id alone
// would silently drop one loop's gate and undercount the pending queue. A genuine duplicate (the
// same loop's same gate, e.g. replayed from the backlog and again live) matches on both fields
// and is ignored. The first prompt to land saves the current compose draft and switches the mode
// to the head's mode; subsequent appends leave the active head and mode untouched.
func (m *interactionModel) enqueue(p prompt) {
	for i := range m.pending {
		if samePrompt(m.pending[i], p) {
			return // already pending — ignore the duplicate (same loop's same gate, re-delivered)
		}
	}
	if len(m.pending) == 0 {
		m.composeDraft = m.input.Value()
	}
	m.pending = append(m.pending, p)
	m.syncModeToHead()
}

// samePrompt reports whether a and b are the same pending request.
//
// A form prompt is identified by its GATE id, which GateOpened names outright.
// The (LoopID, ToolExecutionID) pair the other kinds use is not an identity for
// it: an integration's form gate need not belong to a tool call at all, so its
// Subject.ToolExecutionID is routinely zero, and two such gates on one loop would
// collide on that pair and silently drop the second. The gate id has no such
// ambiguity, so it is preferred whenever either prompt carries one.
//
// The prompt KIND is deliberately not part of the identity. A gate is identified
// by WHICH gate it is, not by how it is rendered, and the existing contract is
// that (LoopID, ToolExecutionID) names one gate across kinds — so a permission and
// an AskUser prompt sharing that pair are one re-delivered gate, not two.
func samePrompt(a, b prompt) bool {
	if a.LoopID != b.LoopID {
		return false
	}
	if a.GateID != (gate.ID{}) || b.GateID != (gate.ID{}) {
		return a.GateID == b.GateID
	}
	return a.ToolExecutionID == b.ToolExecutionID
}

// clearGate drops the pending prompt whose gate id is id and reveals what remains.
//
// It matches on the gate id ONLY, and a zero id matches nothing. That guard is
// load-bearing rather than defensive: permission and AskUser prompts are enqueued
// from their per-kind events and carry no gate id, so without it a single
// GateResolved would match every one of them at once and wipe the queue.
func (m interactionModel) clearGate(id gate.ID) interactionModel {
	if id == (gate.ID{}) || len(m.pending) == 0 {
		return m
	}
	kept := make([]prompt, 0, len(m.pending))
	for _, p := range m.pending {
		if p.GateID != id {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(m.pending) {
		return m // nothing matched — no compose clobber
	}
	m.pending = kept
	if len(m.pending) == 0 {
		m.restoreCompose()
	} else {
		m.syncModeToHead()
	}
	return m
}

// pop removes the active (head) prompt and reveals the next one. When the queue
// drains it returns to compose mode and restores the saved draft. It is the
// resolution path the modal router (Task 8) calls after approve/deny/answer.
func (m interactionModel) pop() interactionModel {
	if len(m.pending) == 0 {
		return m
	}
	m.pending = m.pending[1:]
	m.syncModeToHead()
	return m
}

// ClearPrompts drops every pending prompt and restores compose mode plus the
// saved draft. It is the terminal-event path: when the turn ends, any unresolved
// prompts are abandoned and the user is returned to their composer exactly as they
// left it. Value receiver returning a new model, matching pop's Elm-style contract.
//
// When no prompt was active it is a no-op. Terminal events fire on every turn end,
// and restoring the (empty) saved draft would clobber text the user typed into the
// composer while the turn was streaming. Only an actually-preempted compose is restored.
func (m interactionModel) ClearPrompts() interactionModel {
	if len(m.pending) == 0 {
		return m
	}
	m.pending = nil
	m.restoreCompose()
	return m
}

// ClearPromptsForLoop drops only the pending prompts produced by loopID and
// reveals what remains. It is the per-turn terminal-event path (design §7): when
// one loop's turn ends (TurnDone/TurnFailed/TurnInterrupted), only THAT loop's
// unresolved gates are abandoned — a sibling loop's pending prompt survives,
// where the blanket ClearPrompts would have wrongly dropped it. Value receiver
// returning a new model, matching ClearPrompts/pop's Elm-style contract.
//
// It is a no-op (returns m unchanged) when nothing pends or when no pending
// prompt belongs to loopID — terminal events fire on every turn end, and an
// unrelated loop's end must not restore the (possibly empty) saved draft over
// text the user typed into the composer while the turn streamed.
//
// When some prompts match, it builds a FRESH slice of the survivors (LoopID !=
// loopID) — never an in-place filter, since the value-copy model shares pending's
// backing array with the caller (see cloneHead). If the queue then drains it
// restores compose; otherwise it re-syncs the mode to the new head, which may
// have changed when the prior active head was one of the cleared prompts.
func (m interactionModel) ClearPromptsForLoop(loopID uuid.UUID) interactionModel {
	if len(m.pending) == 0 {
		return m
	}
	kept := make([]prompt, 0, len(m.pending))
	for _, p := range m.pending {
		if p.LoopID != loopID {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(m.pending) {
		return m // nothing belonged to loopID — no compose clobber
	}
	m.pending = kept
	if len(m.pending) == 0 {
		m.restoreCompose()
	} else {
		m.syncModeToHead()
	}
	return m
}

// syncModeToHead sets the mode from the active prompt: permission → permission
// mode; user-input → choice or answer depending on freeText. With no pending
// prompt it returns to compose and restores the saved draft.
func (m *interactionModel) syncModeToHead() {
	p := m.ActivePrompt()
	if p == nil {
		m.restoreCompose()
		return
	}
	switch {
	case p.Kind == promptPermission:
		m.mode = modePermissionPrompt
	case p.Kind == promptForm:
		m.mode = modeFormPrompt
	case p.Kind == promptOpenURL:
		m.mode = modeOpenURLPrompt
	case p.freeText:
		m.mode = modeAnswerPrompt
		// The input box IS the answer field, so it must start empty: the compose
		// draft was already saved to composeDraft at enqueue and is restored by
		// restoreCompose when the queue drains. A back-to-back free-text prompt
		// likewise opens on an empty field.
		m.input.SetValue("")
	default:
		m.mode = modeChoicePrompt
	}
}

// restoreCompose returns to compose mode and refills the editor with the saved
// draft, clearing any stale completion panel.
func (m *interactionModel) restoreCompose() {
	m.mode = modeCompose
	m.input.SetValue(m.composeDraft)
	m.slash, m.files = nil, nil
}

// noop is the consumed-key result: nothing for Screen to act on, re-render only.
var noop = uiAction{Kind: uiNoop}

// isEnter reports whether msg is a submit Enter — main Enter or keypad Enter,
// both of which stringify to "enter" (KeyEnter and KeyKpEnter share that
// keystroke). shift+enter stringifies to "shift+enter", so it is naturally
// excluded here and forwards to the textarea's newline binding. Routing Enter
// through this one helper keeps the decision identical across every mode
// (compose + the three prompt modes) so a real key like keypad Enter cannot
// submit in one mode yet be typed literally in another.
func isEnter(msg tea.KeyPressMsg) bool { return msg.String() == "enter" }

// Update advances the model on a key press and returns the new model, a typed
// uiAction, and the editor's cursor-blink Cmd. It dispatches on the current mode:
// compose edits/submits the next message; the three prompt modes route the key to a
// per-mode handler that produces the typed action (approve/deny/answer/interrupt).
// When a prompt-mode handler resolves the head (approve/deny/answer) the head is
// popped optimistically in the returned model — fire-and-route, no ack — revealing
// the next prompt or returning to compose. esc precedence is encoded per mode: deny
// in permission mode, interrupt (no pop) in choice/answer mode, existing behavior in
// compose. The third return is the textarea's blink Cmd from the editing modes
// (compose + free-text answer); the prompt control modes return a nil Cmd. Screen
// batches it so the cursor keeps blinking wherever the composer is the active field.
func (m interactionModel) Update(msg tea.KeyPressMsg) (interactionModel, uiAction, tea.Cmd) {
	switch m.mode {
	case modePermissionPrompt:
		model, action := m.permissionKey(msg)
		return model, action, nil
	case modeChoicePrompt:
		model, action := m.choiceKey(msg)
		return model, action, nil
	case modeAnswerPrompt:
		return m.answerKey(msg)
	case modeFormPrompt:
		model, action := m.formKey(msg)
		return model, action, nil
	case modeOpenURLPrompt:
		model, action := m.openURLKey(msg)
		return model, action, nil
	default:
		return m.composeKey(msg)
	}
}

// openURLKey routes a key in modeOpenURLPrompt (head is a promptOpenURL).
//
// Two keys, and each is guarded by the gate's own Controls: enter reports
// completion (gate.FormActionAccept) and esc declines. The session refuses any
// action a gate never advertised, so a key the gate does not offer must be a
// no-op here rather than a request that comes back rejected — an opener that
// wants no explicit completion (RequiresCompletion false, so no accept control)
// simply cannot have one pressed. Every other key re-renders.
//
// An unsupported gate (its envelope did not validate) can only be declined:
// accepting would be the user vouching for an origin the TUI could not vouch for.
//
// There is deliberately no "open" key. The TUI has no URL to open — the host owns
// the browser and already opened it.
func (m interactionModel) openURLKey(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		if !head.offersAction(gate.FormActionDecline) {
			return m, noop
		}
		return m.pop(), uiAction{
			Kind: uiGateRespond, LoopID: head.LoopID, GateID: head.GateID,
			GateAction: gate.FormActionDecline,
		}
	}
	if head.unsupported {
		return m, noop
	}
	if isEnter(msg) {
		if !head.offersAction(gate.FormActionAccept) {
			return m, noop
		}
		return m.pop(), uiAction{
			Kind: uiGateRespond, LoopID: head.LoopID, GateID: head.GateID,
			GateAction: gate.FormActionAccept,
		}
	}
	return m, noop
}

// formKey routes a key in modeFormPrompt (head is a promptForm).
//
// esc declines and enter submits, but each only when the gate's Controls actually
// offer that action — the session refuses an action a gate never advertised, so an
// unoffered key is a no-op here rather than a doomed request. enter additionally
// requires every required field to be answered; an incomplete submit sets the
// validation notice instead of sending a response the session would reject.
//
// The remaining keys edit the focused field: ↑/↓ move between fields, ←/→ cycle a
// select, space toggles a confirm, and printable runes plus backspace type into a
// text field. An unsupported form has no editor, so only esc does anything.
func (m interactionModel) formKey(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		if !head.offersAction(gate.FormActionDecline) {
			return m, noop
		}
		return m.pop(), uiAction{
			Kind: uiGateRespond, LoopID: head.LoopID, GateID: head.GateID,
			GateAction: gate.FormActionDecline,
		}
	}
	if head.unsupported {
		return m, noop
	}
	if isEnter(msg) {
		return m.submitForm(head)
	}
	return m.editFormField(msg)
}

// submitForm accepts the form when the gate offers accept and every required field
// is answered; otherwise it flags the incomplete form and stays open.
func (m interactionModel) submitForm(head prompt) (interactionModel, uiAction) {
	if !head.offersAction(gate.FormActionAccept) {
		return m, noop
	}
	if !head.formComplete() {
		m.pending = cloneHead(m.pending)
		m.pending[0].invalid = true
		return m, noop
	}
	return m.pop(), uiAction{
		Kind: uiGateRespond, LoopID: head.LoopID, GateID: head.GateID,
		GateAction: gate.FormActionAccept, Values: head.formValues(),
	}
}

// editFormField applies an editing key to the focused field. Every path clones the
// head first: the value-copy model shares pending's backing array with the caller
// (see cloneHead), so mutating in place would write through the caller's slice.
// Any edit clears a stale validation notice.
func (m interactionModel) editFormField(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	m.pending = cloneHead(m.pending)
	head := &m.pending[0]
	head.invalid = false
	switch msg.Code {
	case tea.KeyUp:
		head.moveFocus(-1)
		return m, noop
	case tea.KeyDown, tea.KeyTab:
		head.moveFocus(1)
		return m, noop
	case tea.KeyLeft:
		head.cycleChoice(-1)
		return m, noop
	case tea.KeyRight:
		head.cycleChoice(1)
		return m, noop
	case tea.KeyBackspace:
		head.backspaceText()
		return m, noop
	}
	head.typeRune(msg)
	return m, noop
}

// permissionKey routes a key in modePermissionPrompt (head is the ONE combined
// tool-preparation approval prompt). It offers exactly the three gate.ApprovalControls
// actions: y approves (gate.ApprovalApprove), a approves-always-for-this-workspace
// (gate.ApprovalApproveAlwaysWorkspace, persisting the displayed candidates), and n or
// esc deny fail-secure (gate.ApprovalDeny). There is no session scope, user-global
// scope, or per-capability sub-prompt. An approve/deny resolves the head, so it pops
// optimistically; any other key re-renders.
func (m interactionModel) permissionKey(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		return m.pop(), uiAction{Kind: uiDeny, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID}
	}
	switch msg.Code {
	case 'y':
		return m.pop(), uiAction{Kind: uiApprove, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Approval: gate.ApprovalApprove}
	case 'a':
		return m.pop(), uiAction{Kind: uiApprove, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Approval: gate.ApprovalApproveAlwaysWorkspace}
	case 'n':
		return m.pop(), uiAction{Kind: uiDeny, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID}
	}
	return m, noop
}

// choiceKey routes a key in modeChoicePrompt (head is a promptUserInput with
// choices). up/down move the selection (no-op action, no pop); enter answers the
// selected choice; 1–9 are accelerators for choices at that index; o answers the
// literal "other" escape hatch; esc interrupts WITHOUT popping (the terminal event
// clears). There is no free-text capture in choice mode — any other key re-renders.
func (m interactionModel) choiceKey(msg tea.KeyPressMsg) (interactionModel, uiAction) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		return m, uiAction{Kind: uiInterrupt}
	}
	if isEnter(msg) {
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: head.Choices[head.selected]}
	}
	switch msg.Code {
	case tea.KeyUp:
		return m.selectBy(-1)
	case tea.KeyDown:
		return m.selectBy(1)
	case 'o':
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: otherChoice}
	}
	if i := int(msg.Code - '1'); msg.Code >= '1' && msg.Code <= '9' && i < len(head.Choices) {
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: head.Choices[i]}
	}
	return m, noop
}

// selectBy moves the head choice cursor by delta and returns a no-op (selection
// is local state, nothing for Screen to act on). Under the value-copy model the
// returned model's pending slice shares its backing array with the caller, so the
// head is cloned before mutating to avoid writing through the caller's slice.
func (m interactionModel) selectBy(delta int) (interactionModel, uiAction) {
	m.pending = cloneHead(m.pending)
	m.pending[0].moveSelection(delta)
	return m, noop
}

// answerKey routes a key in modeAnswerPrompt (head is a free-text promptUserInput;
// the input box IS the answer field). enter submits the typed answer when
// non-empty (pop + uiAnswer; the queue-drain restore puts the compose draft back);
// an empty enter re-prompts (no-op); esc interrupts WITHOUT popping; every other
// key (including shift+enter's newline via the textarea binding) forwards to the
// editor and returns its blink Cmd so the answer field's cursor keeps blinking.
func (m interactionModel) answerKey(msg tea.KeyPressMsg) (interactionModel, uiAction, tea.Cmd) {
	head := *m.ActivePrompt()
	if msg.Code == tea.KeyEsc {
		return m, uiAction{Kind: uiInterrupt}, nil
	}
	if isEnter(msg) {
		v := m.input.Value()
		if strings.TrimSpace(v) == "" {
			return m, noop, nil
		}
		return m.pop(), uiAction{Kind: uiAnswer, LoopID: head.LoopID, ToolExecutionID: head.ToolExecutionID, Text: v}, nil
	}
	cmd := m.input.Update(msg)
	return m, noop, cmd
}

// ForwardToEditor routes a NON-keypress message to the editor. It is the sibling of
// Update, whose argument is a tea.KeyPressMsg and which therefore cannot carry any of the
// messages the editor still needs:
//
//   - tea.PasteMsg — Bubble Tea v2 delivers bracketed-paste text as its own message
//     rather than folding it into a keypress the way v1 did.
//   - the textarea's reply to its OWN ctrl+v binding, which reads the system clipboard in
//     a command and answers with a package-INTERNAL message. Being unexported, it can
//     never be matched by type here; it can only be forwarded blind, which is why Screen
//     routes its unhandled messages through this door rather than enumerating cases.
//   - the cursor-blink tick, which keeps the composer caret alive.
//
// Only the two text-entry modes accept one. modeCompose refreshes the completion panels
// so a pasted "/cmd" or "@path" behaves exactly as a typed one does — but ONLY when the
// value actually changed, because a blink tick arrives on a timer and refreshCompletion
// touches the filesystem. modeAnswerPrompt writes straight to the editor without touching
// the panels, mirroring answerKey: the input box IS the answer field there, and slash/@
// completion is a compose-only affordance. Every other mode (permission, choice, form,
// open-URL) drives discrete controls with no free-text editor, so the message is inert
// rather than silently filling a composer hidden behind the card.
func (m interactionModel) ForwardToEditor(msg tea.Msg) (interactionModel, tea.Cmd) {
	switch m.mode {
	case modeCompose:
		before := m.input.Value()
		cmd := m.input.Update(msg)
		if m.input.Value() != before {
			m.refreshCompletion()
		}
		return m, cmd
	case modeAnswerPrompt:
		return m, m.input.Update(msg)
	default:
		return m, nil
	}
}

// composeKey routes a key in modeCompose. When the slash panel is visible it owns
// tab/up/down/enter (mirroring screen.go's handleKey/handleEnter): tab fills the
// input with the highlighted command, up/down navigate the panel, and enter
// dispatches the HIGHLIGHTED command. With no panel, a bare Enter submits/runs via
// composeEnter; any other key edits the editor and rebuilds the panel, returning the
// editor's blink Cmd so the composer cursor keeps blinking. The panel-navigation and
// submit/run keys are pure state changes — they return a nil Cmd.
func (m interactionModel) composeKey(msg tea.KeyPressMsg) (interactionModel, uiAction, tea.Cmd) {
	if msg.String() == "esc" && (m.slash != nil || m.files != nil) {
		m.slash, m.files = nil, nil
		return m, noop, nil
	}
	if m.slash != nil {
		if isEnter(msg) {
			name := m.slash.Selected().Name
			m.input.Reset()
			m.slash = nil
			return m, slashAction(name), nil
		}
		switch msg.String() {
		case "tab":
			m.input.SetValue(m.slash.Selected().Name)
			m.slash = nil
			return m, noop, nil
		case "up":
			m.slash.Up()
			return m, noop, nil
		case "down":
			m.slash.Down()
			return m, noop, nil
		}
	}
	// The @path panel owns tab/enter (complete the highlighted entry) and up/down
	// (navigate). Tab and Enter both COMPLETE here (they never submit while the panel is
	// open); completing a directory keeps the panel open one level in (completeAtPath).
	if m.files != nil {
		switch {
		case isEnter(msg), msg.String() == "tab":
			m.completeAtPath(m.files.Selected())
			return m, noop, nil
		case msg.String() == "up":
			m.files.Up()
			return m, noop, nil
		case msg.String() == "down":
			m.files.Down()
			return m, noop, nil
		}
	}
	if isEnter(msg) {
		model, action := m.composeEnter()
		return model, action, nil
	}
	cmd := m.forwardToInput(msg)
	return m, noop, cmd
}

// completeAtPath replaces the @path partial being typed at the end of the editor with
// the selected entry: "<text before>@<path>", appending "/" for a directory. After
// completing a directory the panel re-lists that directory's contents (drill in); after
// a file it hides. SetValue keeps the editor cursor at the end, ready to keep typing.
func (m *interactionModel) completeAtPath(sel components.FileItem) {
	v := m.input.Value()
	before := strings.TrimSuffix(v, inputpkg.LastField(v)) // text before the trailing @token
	path := sel.Path
	if sel.IsDir {
		path += "/"
	}
	m.input.SetValue(before + "@" + path)
	if sel.IsDir {
		m.files = components.NewFileComplete(inputpkg.ListFiles(path))
	} else {
		m.files = nil
	}
}

// composeEnter resolves a bare Enter in compose mode WITH NO slash panel by
// re-parsing the typed text. An empty/whitespace draft is a no-op (input kept). A
// leading-slash known command resets the input and returns uiRunSlash; an unknown
// slash falls through to a plain submit. A plain submit resets the input and
// returns uiSubmit carrying the composed text. The panel-visible slash path
// (dispatching the highlighted m.slash.Selected entry and Tab/Up/Down navigation)
// lives in composeKey, mirroring screen.go's handleEnter/handleKey slash dispatch.
func (m interactionModel) composeEnter() (interactionModel, uiAction) {
	v := m.input.Value()
	if strings.TrimSpace(v) == "" {
		return m, noop
	}
	if strings.HasPrefix(v, "/") {
		name := firstToken(v)
		if m.isSlashCommand(name) {
			m.input.Reset()
			m.slash = nil
			return m, slashAction(name)
		}
		// Unknown command: fall through to a plain-text submit.
	}
	m.input.Reset()
	m.slash, m.files = nil, nil
	return m, uiAction{Kind: uiSubmit, Text: v}
}

// forwardToInput sends the message — a keypress, or a paste (see ForwardToEditor) — to
// the editor and rebuilds the completion panels from the new value. It returns the
// editor's Cmd (cursor blink) so the caller can keep the composer cursor alive. It
// mirrors screen.go's forwardToInput so compose behavior is identical.
//
// It rebuilds UNCONDITIONALLY, even for a key that left the value untouched: that is the
// long-standing per-keystroke behavior (an arrow key hides a stale panel), and a key is a
// deliberate user action, so the cost is bounded by typing speed. The value-gated variant
// exists in ForwardToEditor for messages that arrive on a TIMER.
func (m *interactionModel) forwardToInput(msg tea.Msg) tea.Cmd {
	cmd := m.input.Update(msg)
	m.refreshCompletion()
	return cmd
}

// refreshCompletion rebuilds the completion panels from the editor's CURRENT value: a
// leading-slash word (no whitespace) opens the command panel from that prefix (nil if
// nothing matches); otherwise an @path being typed at the end opens the file panel. They
// are mutually exclusive, and any other text hides both.
//
// NOTE the file panel calls inputpkg.ListFiles, which touches the FILESYSTEM. Every caller
// must therefore be driven by a real user edit — never by a timer — or the composer would
// stat a directory on every tick. See ForwardToEditor's value gate.
func (m *interactionModel) refreshCompletion() {
	v := m.input.Value()
	m.slash, m.files = nil, nil
	switch {
	case strings.HasPrefix(v, "/") && !strings.ContainsAny(v, " \t\n"):
		m.slash = components.NewSlashCompleteWithCommands(firstToken(v), m.slashCommands)
	default:
		if partial, ok := inputpkg.ActiveAtToken(v); ok {
			m.files = components.NewFileComplete(inputpkg.ListFiles(partial))
		}
	}
}

// helpText builds the /help listing from the canonical command table. Screen
// commits the result as a system entry when /help runs.
func helpText() string {
	var b strings.Builder
	b.WriteString("commands:")
	for _, c := range components.SlashCommands {
		b.WriteString("\n  " + c.Name + " — " + c.Desc)
	}
	return b.String()
}

// slashAction maps a recognized slash-command name to the typed uiAction Screen acts
// on. Every command shares the generic uiRunSlash carrier (Screen.runSlash switches on
// the name), keeping all status-gated slash execution in one place.
func slashAction(name string) uiAction {
	return uiAction{Kind: uiRunSlash, Slash: name}
}

// isSlashCommand reports whether name is a visible command or the exact hidden
// /help compatibility command. Hidden commands are deliberately kept out of the
// component command table so they never appear in completion or helpText.
func (m interactionModel) isSlashCommand(name string) bool {
	if name == "/help" {
		return true
	}
	for _, c := range m.slashCommands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// isSlashCommand preserves the package-level compatibility check for the static catalog.
func isSlashCommand(name string) bool { return newInteractionModel().isSlashCommand(name) }

// firstToken returns the first whitespace-delimited token of s, or "" if none.
func firstToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
