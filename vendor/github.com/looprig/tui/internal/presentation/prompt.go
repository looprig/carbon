package presentation

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tui/styles"
)

// promptKind tags a prompt as a permission gate or a user-input request. Each is
// rendered and routed differently, so the kind is carried explicitly rather than
// inferred from which fields are populated.
type promptKind uint8

const (
	// promptPermission is a tool-call approval gate: the user approves (at a
	// scope) or denies.
	promptPermission promptKind = iota
	// promptUserInput is an AskUser request: the user picks a choice or types a
	// free-text answer.
	promptUserInput
	// promptForm is a structured human-input gate (gate.KindForm): a bounded set
	// of typed fields the user fills in and submits. Unlike the other two it is
	// built from a GateOpened envelope rather than a per-kind loop event, because a
	// form gate is raised by an integration host and has no loop-side request event.
	promptForm
	// promptOpenURL is an open-url gate (gate.KindOpenURL): the host has sent the
	// user out-of-band to authorize something at an origin, and this prompt names
	// that origin and takes the user's completion decision. Like promptForm it is
	// folded from a GateOpened envelope.
	//
	// It carries NO url, and that absence is the design. The action target is the
	// ephemeral credential — it carries the OAuth `state`, the PKCE challenge,
	// sometimes a token — and it lives only on the gate's private payload, which
	// never leaves the session. The host opened it (it holds the BrowserOpener);
	// the TUI's job is to say WHO is being authorized and to take the answer. With
	// no url field here, no url can reach the scrollback or a log.
	promptOpenURL
)

// formField is one editable row of a form prompt: the schema's description of the
// field plus the live value the user is editing.
//
// The three value fields are kind-exclusive rather than one string, because the
// answer's JSON TYPE is part of the contract: gate.ParseFormAnswers wants a string
// for text and select but a bool for confirm, and collapsing them to a string here
// would push a "was this really a bool?" guess onto the encode step.
type formField struct {
	Name     string
	Label    string
	Kind     gate.FieldKind
	Required bool
	Options  []gate.Option
	Text     string // FieldText: the typed value
	Choice   int    // FieldSelect: index into Options
	Confirm  bool   // FieldConfirm: the toggle state
}

// display renders the field's current value for the editor row.
func (f formField) display() string {
	switch f.Kind {
	case gate.FieldConfirm:
		if f.Confirm {
			return "yes"
		}
		return "no"
	case gate.FieldSelect:
		if f.Choice < 0 || f.Choice >= len(f.Options) {
			return ""
		}
		if label := f.Options[f.Choice].Label; label != "" {
			return label
		}
		return f.Options[f.Choice].Value
	default:
		return f.Text
	}
}

// encode returns the field's answer as the JSON value gate.ParseFormAnswers
// expects, and whether the field has an answer to submit at all. An unanswered
// optional field returns false so it is omitted from the response rather than
// submitted as an empty string — the schema distinguishes "not answered" from
// "answered empty", and only the former is legal for an optional field.
func (f formField) encode() (json.RawMessage, bool) {
	switch f.Kind {
	case gate.FieldConfirm:
		// A confirm always has an answer: false is a real answer, not a blank.
		raw, err := json.Marshal(f.Confirm)
		if err != nil {
			return nil, false
		}
		return raw, true
	case gate.FieldSelect:
		if f.Choice < 0 || f.Choice >= len(f.Options) {
			return nil, false
		}
		raw, err := json.Marshal(f.Options[f.Choice].Value)
		if err != nil {
			return nil, false
		}
		return raw, true
	default:
		if f.Text == "" {
			return nil, false
		}
		raw, err := json.Marshal(f.Text)
		if err != nil {
			return nil, false
		}
		return raw, true
	}
}

// satisfied reports whether a required field has an answer. It mirrors
// gate.ParseFormAnswers' required rule exactly: a text or select field must carry
// a non-empty value, while a confirm is always answered (false is an answer). It
// is what stops the user submitting a response the session would reject.
func (f formField) satisfied() bool {
	if !f.Required {
		return true
	}
	_, ok := f.encode()
	return ok || f.Kind == gate.FieldConfirm
}

// prompt is the interaction layer's view-model for one pending request, keyed by
// the gate's ToolExecutionID. It carries everything the renderer needs and the selection
// state the modal key router (Task 8) mutates — but holds NO agent reference: the
// interactionModel only PRODUCES a uiAction; Screen drives the agent. A permission
// prompt uses ToolName/Summary/Requirements; a user-input prompt uses
// Question/Choices/selected/freeText.
type prompt struct {
	ToolExecutionID uuid.UUID
	// LoopID is the producing loop's id, stamped from the enqueuing event's
	// Header (zero when that event carries no LoopID). It scopes terminal-event clearing
	// per loop (design §7): a TurnDone/TurnFailed/TurnInterrupted clears only the
	// prompts whose LoopID matches the finishing loop, so one loop ending never
	// abandons a sibling loop's pending gate.
	LoopID   uuid.UUID
	Kind     promptKind
	ToolName string // promptPermission: the tool name, for the "Approve <tool>?" header
	// Summary is the promptPermission one-line request summary (tool.Request.Summary),
	// shown under the header. It is a display-ready string from the wire.
	Summary string
	// Requirements is the promptPermission list of EVERY unmet capability the one
	// combined prompt must show, each with its exact persisted rule candidates. Every
	// string here is a display-ready description from the typed gate payload
	// (tool.Requirement.Description / tool.RuleCandidate.Description) — the TUI never
	// reconstructs a rule or parses tool arguments.
	Requirements []requirementLine
	Question     string   // promptUserInput: the AskUser question
	Choices      []string // promptUserInput: selectable choices (nil → free-text)
	selected     int      // promptUserInput: cursor over Choices
	freeText     bool     // promptUserInput: true when there are no Choices

	// promptForm fields. GateID is carried directly because a form gate is folded
	// from GateOpened, which names the gate outright — unlike a permission or
	// AskUser prompt, whose gate id the adapter must look up by ToolExecutionID.
	GateID   gate.ID
	Title    string      // promptForm/promptOpenURL: the heading
	Body     string      // promptForm/promptOpenURL: the explanatory body
	Fields   []formField // promptForm: the editable rows
	Controls []string    // promptForm/promptOpenURL: the actions the gate actually offers
	focus    int         // promptForm: cursor over Fields
	// Origin is the promptOpenURL gate's VALIDATED bare origin (gate.Prompt.Origin,
	// e.g. "https://github.com"). It is the whole security content of the prompt:
	// it is what the user's trust decision is made on, and it is trustworthy here
	// precisely because gate.ValidateGate holds the envelope to the same bare-origin
	// rule as the durable payload — scheme and host, never a path or query. It is
	// therefore rendered AS an origin rather than as prose an integration supplied.
	Origin string
	// unsupported marks a prompt the TUI cannot honestly render: a form whose schema
	// it cannot edit (see promptFromForm), or an open-url gate whose envelope does
	// not validate (see promptFromOpenURL). It is rendered as a notice offering only
	// decline, never as a partial editor that would submit an answer the user did
	// not give, nor as an origin the user could not trust.
	unsupported bool
	// invalid is set when a submit was attempted with a required field unanswered.
	// It drives the validation notice; it is cleared by any subsequent edit.
	invalid bool
}

// offersAction reports whether the gate's Prompt.Controls declare action.
//
// It is the fail-closed half of the response contract: the session's RespondGate
// refuses any action a gate never offered (validateGateAction), so a key bound to
// an action outside Controls must be a no-op here rather than a request the
// session will reject. An integration that offers no decline cannot be declined
// by a keypress it never advertised.
func (p *prompt) offersAction(action string) bool {
	for _, c := range p.Controls {
		if c == action {
			return true
		}
	}
	return false
}

// promptFromForm builds a form prompt view-model from a GateOpened envelope.
//
// It renders from the PUBLIC envelope only: Gate.Prompt is the presentation
// projection an opener derives from its private FormPayload, and the payload
// itself never leaves the session. The schema here therefore drives the editor,
// while the session still validates the submitted answer against the payload's
// authoritative copy — so a divergent projection costs a rejected response, never
// an unvalidated one.
//
// A schema the TUI cannot faithfully edit yields an unsupported prompt rather
// than a partial one. gate.FieldMultiSelect is the case that matters: a form
// answer is one value per field and multi-select cannot be represented, so
// gate.ValidateFormSchema rejects it in a payload — but Gate.Prompt is not run
// through that validation, so a projection can still carry one. Rendering it as a
// single-select would silently submit one selection where the user believed they
// had chosen several. An unknown field kind fails closed the same way.
func promptFromForm(g gate.Gate) prompt {
	p := prompt{
		ToolExecutionID: g.Subject.ToolExecutionID,
		GateID:          g.ID,
		Kind:            promptForm,
		Title:           g.Prompt.Title,
		Body:            g.Prompt.Body,
	}
	for _, c := range g.Prompt.Controls {
		p.Controls = append(p.Controls, c.Action)
	}
	if len(g.Prompt.Schema.Fields) == 0 {
		p.unsupported = true
		return p
	}
	for _, f := range g.Prompt.Schema.Fields {
		field, ok := formFieldFromSchema(f)
		if !ok {
			p.unsupported = true
			p.Fields = nil
			return p
		}
		p.Fields = append(p.Fields, field)
	}
	return p
}

// promptFromOpenURL builds an open-url prompt view-model from a GateOpened
// envelope.
//
// Like promptFromForm it reads the PUBLIC envelope only — but here that is not
// merely sufficient, it is the point. The private OpenURLPayload holds the action
// URL, and the TUI is never given it: the host opened the browser, and the only
// question left for a human is whether the origin now asking for authorization is
// one they trust. So this copies Prompt.Origin and nothing that could carry a
// target.
//
// The envelope is re-validated here rather than trusted. gate.ValidateGate is the
// same check the session applies at open time, and it is what makes Origin an
// origin: without it a malformed or absent origin would render as a trust
// decision the user cannot actually make, which is precisely what displaying a
// bare origin exists to prevent. A gate that fails it is marked unsupported and
// offers only decline — fail closed, never a prompt that vouches for something it
// could not check.
func promptFromOpenURL(g gate.Gate) prompt {
	p := prompt{
		ToolExecutionID: g.Subject.ToolExecutionID,
		GateID:          g.ID,
		Kind:            promptOpenURL,
		Title:           g.Prompt.Title,
		Body:            g.Prompt.Body,
		Origin:          g.Prompt.Origin,
	}
	for _, c := range g.Prompt.Controls {
		p.Controls = append(p.Controls, c.Action)
	}
	if err := gate.ValidateGate(g); err != nil {
		p.unsupported = true
		p.Origin = ""
	}
	return p
}

// formFieldFromSchema converts one schema field into an editable row, applying the
// schema's default. It fails closed on any kind the editor cannot represent.
func formFieldFromSchema(f gate.Field) (formField, bool) {
	field := formField{
		Name:     f.Name,
		Label:    f.Label,
		Kind:     f.Kind,
		Required: f.Required,
		Options:  f.Options,
	}
	switch f.Kind {
	case gate.FieldText:
		_ = json.Unmarshal(f.Default, &field.Text) // absent/!string default → empty field
	case gate.FieldConfirm:
		_ = json.Unmarshal(f.Default, &field.Confirm) // absent/!bool default → false
	case gate.FieldSelect:
		if len(f.Options) == 0 {
			return formField{}, false // a select with nothing to select is unanswerable
		}
		var def string
		if json.Unmarshal(f.Default, &def) == nil {
			for i, o := range f.Options {
				if o.Value == def {
					field.Choice = i
					break
				}
			}
		}
	default:
		// gate.FieldMultiSelect and anything unknown. See promptFromForm.
		return formField{}, false
	}
	return field, true
}

// formValues encodes the answered fields as the response Values map. It is only
// meaningful for an accept.
func (p *prompt) formValues() map[string]json.RawMessage {
	values := make(map[string]json.RawMessage, len(p.Fields))
	for _, f := range p.Fields {
		if raw, ok := f.encode(); ok {
			values[f.Name] = raw
		}
	}
	return values
}

// formComplete reports whether every required field is answered.
func (p *prompt) formComplete() bool {
	for _, f := range p.Fields {
		if !f.satisfied() {
			return false
		}
	}
	return true
}

// moveFocus shifts the field cursor by delta, clamped to the field list.
func (p *prompt) moveFocus(delta int) {
	n := len(p.Fields)
	if n == 0 {
		p.focus = 0
		return
	}
	next := p.focus + delta
	if next < 0 {
		next = 0
	}
	if next > n-1 {
		next = n - 1
	}
	p.focus = next
}

// typeRune applies a printable keypress to the focused field: a rune extends a
// text field, and space toggles a confirm.
//
// Space is safe to overload because it can only ever reach a FOCUSED field, and a
// confirm field has no text to type a space into. A text field keeps the space.
//
// A rune is accepted only when it is printable and carries no modifier, so a
// control chord (ctrl+c, alt+f) can never be typed into an answer as a literal.
// Values are bounded at maxFormFieldRunes: the field is a human-facing prompt, not
// a data channel, and the session refuses an over-long answer anyway
// (gate.ParseFormAnswers) — clamping here refuses it before the user loses work.
func (p *prompt) typeRune(msg tea.KeyPressMsg) {
	if p.focus < 0 || p.focus >= len(p.Fields) {
		return
	}
	f := &p.Fields[p.focus]
	if msg.Mod != 0 {
		return
	}
	if f.Kind == gate.FieldConfirm {
		if msg.Code == tea.KeySpace {
			f.Confirm = !f.Confirm
		}
		return
	}
	if f.Kind != gate.FieldText {
		return
	}
	r := msg.Code
	if !unicode.IsPrint(r) {
		return
	}
	if utf8.RuneCountInString(f.Text) >= maxFormFieldRunes {
		return
	}
	f.Text += string(r)
}

// backspaceText removes the last rune of the focused text field.
func (p *prompt) backspaceText() {
	if p.focus < 0 || p.focus >= len(p.Fields) {
		return
	}
	f := &p.Fields[p.focus]
	if f.Kind != gate.FieldText || f.Text == "" {
		return
	}
	runes := []rune(f.Text)
	f.Text = string(runes[:len(runes)-1])
}

// maxFormFieldRunes bounds one typed field value. It is well under the session's
// own 4096-BYTE cap (gate.ParseFormAnswers) so a multi-byte answer at this rune
// count still fits, and it is far more than a prompt field should ever need.
const maxFormFieldRunes = 512

// cycleChoice advances the focused select field's option by delta, wrapping. It is
// a no-op on any other field kind.
func (p *prompt) cycleChoice(delta int) {
	if p.focus < 0 || p.focus >= len(p.Fields) {
		return
	}
	f := &p.Fields[p.focus]
	if f.Kind != gate.FieldSelect || len(f.Options) == 0 {
		return
	}
	n := len(f.Options)
	f.Choice = ((f.Choice+delta)%n + n) % n
}

// requirementLine is one unmet capability the combined permission prompt shows:
// the requirement's display description plus the display descriptions of the exact
// reusable rule candidates a workspace approval would persist. Both are read
// verbatim off the typed gate payload — never reconstructed.
type requirementLine struct {
	Description string
	Candidates  []string
}

// promptFromPermission builds the ONE combined permission prompt view-model from a
// typed prepared tool.Request (the request narrowed to its unmet requirements, each
// carrying its RuleCandidates). Every unmet requirement and every exact persisted
// candidate is projected into a single prompt — the TUI renders the typed payload and
// never reconstructs a rule or parses tool arguments. freeText is false: a permission
// gate is never free-text.
func promptFromPermission(callID uuid.UUID, req tool.Request) prompt {
	p := prompt{
		ToolExecutionID: callID,
		Kind:            promptPermission,
		ToolName:        req.ToolName,
		Summary:         req.Summary,
	}
	for _, requirement := range req.Requirements {
		line := requirementLine{Description: requirement.Description}
		for _, candidate := range requirement.Candidates {
			line.Candidates = append(line.Candidates, candidate.Description)
		}
		p.Requirements = append(p.Requirements, line)
	}
	return p
}

// permissionRequestSummary is the one-line display description remembered for a
// gated tool call's committed card. It prefers the request Summary, falling back to
// the tool name, so the committed card can read "Approved <summary>". It reads only
// display-ready fields off the typed request.
func permissionRequestSummary(req tool.Request) string {
	if req.Summary != "" {
		return req.Summary
	}
	return req.ToolName
}

// promptFromUserInput builds a user-input prompt view-model. freeText is true
// exactly when there are no choices (an empty or nil slice), in which case the
// user types an answer rather than picking one.
func promptFromUserInput(callID uuid.UUID, question string, choices []string) prompt {
	return prompt{
		ToolExecutionID: callID,
		Kind:            promptUserInput,
		Question:        question,
		Choices:         choices,
		freeText:        len(choices) == 0,
	}
}

// moveSelection shifts the choice cursor by delta and clamps it to the valid
// range [0, len(Choices)-1]. An empty choice list pins the cursor at zero. It is
// the up/down handler for choice mode; the value-copy router calls it on the head
// of the RETURNED model's freshly-cloned slice (see interactionModel.choiceKey).
func (p *prompt) moveSelection(delta int) {
	n := len(p.Choices)
	if n == 0 {
		p.selected = 0
		return
	}
	next := p.selected + delta
	if next < 0 {
		next = 0
	}
	if next > n-1 {
		next = n - 1
	}
	p.selected = next
}

// approvalHint pairs one of the three exact approval actions (gate.ApprovalAction)
// with the key that produces it. The keys match the modal router
// (interaction.go permissionKey): y approves, a approves-always-for-this-workspace,
// n denies. The order is the ApprovalControls order — there is no session scope,
// user-global scope, or second capability prompt.
type approvalHint struct {
	key    string
	action gate.ApprovalAction
}

// approvalHints is the ordered permission legend, one hint per gate.ApprovalControls
// control. It is derived from the shared control set so a control can never drift from
// its key: the labels ARE the gate.ApprovalAction values the session validates against.
var approvalHints = []approvalHint{
	{"y", gate.ApprovalApprove},
	{"a", gate.ApprovalApproveAlwaysWorkspace},
	{"n", gate.ApprovalDeny},
}

// requirementBullet / candidateBullet prefix the requirement and candidate rows so the
// combined prompt reads as a list: each unmet capability, and beneath it each exact
// rule an "always for this workspace" approval would persist.
const (
	requirementBullet = "• "
	candidateBullet   = "    ↳ "
)

// renderPermissionBox renders the ONE combined tool-preparation approval prompt as a
// blue-panel card: a bold "Approve <ToolName>?" title, the request summary, then EVERY
// unmet requirement's description with its exact persisted rule candidates beneath it,
// and a footer offering exactly the three actions gate.ApprovalControls declares
// (Approve / Approve always for this workspace / Deny), keyed y/a/n. When pending > 1 a
// faint "(+N more pending)" note trails the card. It renders the typed payload only —
// no agent, no mutation, no rule reconstruction.
func renderPermissionBox(p prompt, width, pending int) string {
	textW := cardTextWidth(width)
	sections := make([]string, 0, 4)
	sections = append(sections, styles.CardTitleStyle.Render("Approve "+p.ToolName+"?"))
	if p.Summary != "" {
		sections = append(sections, strings.Join(wrapToWidth(p.Summary, textW), "\n"))
	}
	if body := permissionRequirementsBody(p, textW); body != "" {
		sections = append(sections, body)
	}
	footer := make([]string, 0, len(approvalHints))
	for _, h := range approvalHints {
		footer = append(footer, styleKeyHint("["+h.key+"] "+string(h.action)))
	}
	sections = append(sections, strings.Join(footer, "  "))
	return cardFrame(cardSections(sections...), width, pending)
}

// permissionRequirementsBody renders every unmet requirement and, indented beneath
// each, its exact persisted rule candidates — all display-ready strings straight from
// the typed payload. An empty requirement list yields "" (a pure tool with nothing to
// grant), and cardSections drops the empty body.
func permissionRequirementsBody(p prompt, textW int) string {
	if len(p.Requirements) == 0 {
		return ""
	}
	rows := make([]string, 0, len(p.Requirements))
	for _, requirement := range p.Requirements {
		rows = append(rows, strings.Join(wrapToWidth(requirementBullet+requirement.Description, textW), "\n"))
		for _, candidate := range requirement.Candidates {
			rows = append(rows, strings.Join(wrapToWidth(candidateBullet+candidate, textW), "\n"))
		}
	}
	return strings.Join(rows, "\n")
}

// renderAskUserBox renders an AskUser prompt as a card. With choices it shows the
// numbered list (a window scrolling with selected so a high row stays visible), the
// ▸ cursor with the selected row highlighted, an [o] other escape hatch and a key
// legend; with no choices it renders the free-text variant (the question above the
// reused answer field, no list/[o]). height bounds the choice window; width sizes the
// card. Pure: view-model only.
func renderAskUserBox(p prompt, width, height, pending int) string {
	if p.freeText {
		return renderFreeTextBox(p, width, pending)
	}
	return renderChoiceBox(p, width, height, pending)
}

// renderFreeTextBox renders the free-text answer card: a bold "answer" title, the
// (width-wrapped) question as its body, and a faint submit hint footer. The actual
// editor is the reused composer placed by the surface below this card in
// modeAnswerPrompt — the card is the framing.
func renderFreeTextBox(p prompt, width, pending int) string {
	textW := cardTextWidth(width)
	title := styles.CardTitleStyle.Render("answer")
	body := strings.Join(wrapToWidth(p.Question, textW), "\n")
	footer := styles.CardHintStyle.Render(freeTextLegend)
	return cardFrame(cardSections(title, body, footer), width, pending)
}

// formLegend is the muted key legend shown at the foot of a form card. ↑/↓ moves
// between fields, ←/→ cycles a select, space toggles a confirm, enter submits and
// esc declines.
const formLegend = "↑/↓ field · ←/→ select · space toggle · enter submit · esc decline"

// formUnsupportedLegend is the legend of an unrenderable form: declining is the
// only honest action left.
const formUnsupportedLegend = "esc decline"

// formUnsupportedNotice is the body shown for a form whose schema the TUI cannot
// faithfully edit (see promptFromForm).
const formUnsupportedNotice = "This request uses a field type this terminal cannot show. Decline it and answer where the integration can render it."

// formRequiredMark trails the label of a required field.
const formRequiredMark = " *"

// renderFormBox renders a form gate as a card: the form's title, its body, one row
// per field with the focused row highlighted, and a footer legend listing only the
// actions the gate actually offers. An unsupported schema renders the notice
// instead of an editor. Pure: view-model only.
func renderFormBox(p prompt, width, pending int) string {
	textW := cardTextWidth(width)
	title := styles.CardTitleStyle.Render(formTitle(p))
	if p.unsupported {
		body := strings.Join(wrapToWidth(formUnsupportedNotice, textW), "\n")
		footer := styles.CardHintStyle.Render(formUnsupportedLegend)
		return cardFrame(cardSections(title, body, footer), width, pending)
	}

	sections := make([]string, 0, 4)
	if p.Body != "" {
		sections = append(sections, strings.Join(wrapToWidth(p.Body, textW), "\n"))
	}
	rows := make([]string, 0, len(p.Fields))
	for i, f := range p.Fields {
		rows = append(rows, formRow(f, i == p.focus, textW))
	}
	sections = append(sections, strings.Join(rows, "\n"))
	if p.invalid {
		sections = append(sections, styles.CardHintStyle.Render("fill in the required fields marked *"))
	}
	sections = append(sections, styles.CardHintStyle.Render(formLegend))
	return cardFrame(cardSections(append([]string{title}, sections...)...), width, pending)
}

// formTitle is the card heading: the form's own title, or a neutral fallback when
// the projection carried none.
func formTitle(p prompt) string {
	if p.Title != "" {
		return p.Title
	}
	return "Request"
}

// formRow renders one field as "<label>: <value>", the focused row carrying the ▸
// cursor and a filled highlight bar. A required field's label is marked with *.
func formRow(f formField, focused bool, width int) string {
	label := f.Label
	if label == "" {
		label = f.Name
	}
	if f.Required {
		label += formRequiredMark
	}
	row := label + ": " + f.display()
	if focused {
		return styles.CardSelectedStyle.Width(width).Render("▸ " + truncate(row, width-choicePrefixWidth))
	}
	return "  " + truncate(row, width-choicePrefixWidth)
}

// openURLTitleFallback is the card heading when the projection carried no title.
const openURLTitleFallback = "Authorize"

// openURLOriginLabel prefixes the origin row. The row is the security content of
// the card, so the origin is labelled for what it is rather than left to read as
// prose.
const openURLOriginLabel = "origin: "

// openURLTrustNotice is the standing caution under the origin. It is the TUI's
// own words, not the integration's: the body prose comes from whoever opened the
// gate, so the line telling the user what they are actually deciding must not.
const openURLTrustNotice = "Continue only if you trust this origin with the access it asked for."

// openURLUnsupportedNotice is the body of an open-url gate whose envelope did not
// validate (see promptFromOpenURL). It names no origin, because the reason it is
// here is that the origin could not be trusted to be one.
const openURLUnsupportedNotice = "This request did not name a valid origin, so it cannot be shown safely. Decline it."

// openURLNoActionsLegend is the legend of a gate that offers no action at all.
// The session refuses any action a gate did not advertise, so there is genuinely
// nothing to press — say so rather than print keys that would be swallowed.
const openURLNoActionsLegend = "no actions offered · waiting"

// openURLCompleteHint / openURLDeclineHint are the legend fragments, rendered only
// when the gate's Controls actually offer the action. The completion key is what
// RequiresCompletion looks like from the envelope: an opener that wants an
// explicit "I finished" offers accept, and an opener that does not, does not.
const (
	openURLCompleteHint = "[enter] I've completed it"
	openURLDeclineHint  = "[esc] decline"
)

// renderOpenURLBox renders an open-url gate as a card: the heading, the opener's
// body prose, the validated origin on its own labelled row, the standing trust
// caution, and a footer offering ONLY the actions the gate advertises.
//
// There is no URL on this card because there is no URL in the view-model to put
// on it — the action target never leaves the session (see promptOpenURL). The
// user is shown who is being authorized and asked to confirm completion; opening
// the browser was the host's job. Pure: view-model only.
func renderOpenURLBox(p prompt, width, pending int) string {
	textW := cardTextWidth(width)
	title := styles.CardTitleStyle.Render(openURLTitle(p))
	if p.unsupported {
		body := strings.Join(wrapToWidth(openURLUnsupportedNotice, textW), "\n")
		footer := styles.CardHintStyle.Render(formUnsupportedLegend)
		return cardFrame(cardSections(title, body, footer), width, pending)
	}

	sections := make([]string, 0, 5)
	if p.Body != "" {
		sections = append(sections, strings.Join(wrapToWidth(p.Body, textW), "\n"))
	}
	origin := styles.CardHintStyle.Render(openURLOriginLabel) +
		styles.CardKeyStyle.Render(truncate(p.Origin, textW-len(openURLOriginLabel)))
	sections = append(sections, origin)
	sections = append(sections, strings.Join(wrapToWidth(openURLTrustNotice, textW), "\n"))
	sections = append(sections, openURLLegend(p))
	return cardFrame(cardSections(append([]string{title}, sections...)...), width, pending)
}

// openURLTitle is the card heading: the gate's own title, or a neutral fallback.
func openURLTitle(p prompt) string {
	if p.Title != "" {
		return p.Title
	}
	return openURLTitleFallback
}

// openURLLegend renders the footer hints for exactly the actions the gate offers,
// mirroring the key router (interaction.go openURLKey) one-for-one so a rendered
// key is always a key that does something.
func openURLLegend(p prompt) string {
	hints := make([]string, 0, 2)
	if p.offersAction(gate.FormActionAccept) {
		hints = append(hints, styleKeyHint(openURLCompleteHint))
	}
	if p.offersAction(gate.FormActionDecline) {
		hints = append(hints, styleKeyHint(openURLDeclineHint))
	}
	if len(hints) == 0 {
		return styles.CardHintStyle.Render(openURLNoActionsLegend)
	}
	return strings.Join(hints, "  ")
}

// choiceLegend is the muted key legend shown at the foot of a choice card. It keeps
// the "↑/↓ select" lead so the up/down affordance reads first; enter answers, 1–9 are
// quick accelerators, esc interrupts.
const choiceLegend = "↑/↓ select · enter · 1–9 · esc"

// freeTextLegend is the muted key legend shown at the foot of a free-text card.
const freeTextLegend = "enter submit · esc"

// choicePrefixWidth is the 2-cell cursor/indent prefix on every choice row
// ("▸ " when selected, "  " otherwise); the choice text wraps in the remaining
// columns.
const choicePrefixWidth = 2

// choiceChromeRows is the number of footer rows the choice card reserves inside its
// height budget: the "[o] other" hint and the key legend. Only these two rows count
// against the choice-window capacity — the title, blank separators and border are
// extra card chrome the auto-measured bottom box absorbs.
const choiceChromeRows = 2

// renderChoiceBox renders the numbered-choice card: a bold "<Question> · choice
// n/total" title, the visible window of choices (scrolled to keep selected in view)
// with the ▸ cursor and a highlighted selected row, then a footer of the [o] other
// hint and the key legend.
func renderChoiceBox(p prompt, width, height, pending int) string {
	textW := cardTextWidth(width)
	capacity := choiceWindowCap(height)
	start, end := choiceWindow(len(p.Choices), p.selected, capacity)
	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		rows = append(rows, choiceRow(i, p.Choices[i], i == p.selected, textW))
	}
	title := styles.CardTitleStyle.Render(choiceHeader(p, len(p.Choices)))
	body := strings.Join(rows, "\n")
	footer := styleKeyHint("[o] other") + "\n" + styles.CardHintStyle.Render(choiceLegend)
	return cardFrame(cardSections(title, body, footer), width, pending)
}

// choiceHeader is the "<Question> · choice n/total" card title, omitting the choice
// counter when there are no choices (defensive; choice mode always has some).
func choiceHeader(p prompt, total int) string {
	if total == 0 {
		return p.Question
	}
	return p.Question + " · choice " + strconv.Itoa(p.selected+1) + "/" + strconv.Itoa(total)
}

// choiceRow renders one numbered choice line. The selected row is "▸ N. text"
// highlighted as a filled bar spanning the card body (CardSelectedStyle sized to
// width); every other row is "  N. text" plain. The 1-based index means numbers past 9
// render normally (10., 11., …) — the 1–9 keys are only quick accelerators; ↑/↓ + enter
// reach any choice.
func choiceRow(index int, text string, selected bool, width int) string {
	label := strconv.Itoa(index+1) + ". " + text
	if selected {
		row := "▸ " + truncate(label, width-choicePrefixWidth)
		return styles.CardSelectedStyle.Width(width).Render(row)
	}
	return "  " + truncate(label, width-choicePrefixWidth)
}

// styleKeyHint styles one "[key] label" footer fragment: the bracketed accelerator in
// the bold brand accent (CardKeyStyle) and the trailing label muted (CardHintStyle), so
// a hint reads as a pressable key beside its meaning. A fragment with no space
// (defensive) is rendered whole as a key. Stripped of ANSI the result is the original
// fragment ("[y] once"), so the plain-text interaction contract is preserved.
func styleKeyHint(hint string) string {
	key, label, ok := strings.Cut(hint, " ")
	if !ok {
		return styles.CardKeyStyle.Render(hint)
	}
	return styles.CardKeyStyle.Render(key) + " " + styles.CardHintStyle.Render(label)
}

// cardSections joins the non-empty card sections (title, body, footer) with a blank
// line between them, giving the card its airy padded body. An empty section (e.g. a
// permission gate with no description) is skipped so no double blank line appears.
func cardSections(sections ...string) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// cardRailWidth is the display columns the gate card's left rail prefix ("▌ ") consumes on
// every content row — the panel's horizontal frame. It matches the user-message card's
// barWidth; cardTextWidth subtracts it so content sits flush behind the rail.
const cardRailWidth = 2

// cardFrame frames the assembled card content as a blue PANEL — the user-message card
// treatment tinted blue: a left ▌ rail (CardRailStyle) down every row, the whole block filled
// with the subtle blue CardPanelBg, and one rail-only pad row bracketing the content top and
// bottom (so the card reads as a padded panel). It trails the faint "(+N more pending)" note,
// OUTSIDE the panel, when the queue is deeper than one (pending > 1).
//
// The two pad rows keep the frame exactly two rows tall, so surface.go's boxBorderH height
// reservation stays correct. Content lines were wrapped/sized to cardTextWidth(width) so they
// sit behind the rail; a blank content line (the \n\n between sections) renders as a rail-only
// row, keeping the rail unbroken. The selected-choice row carries its own brighter-blue fill
// (CardSelectedStyle) spanning cardTextWidth, so it reads as a filled bar on the panel.
func cardFrame(content string, width, pending int) string {
	open, reset := styles.DeriveBackgroundSGR(styles.CardPanelBg)
	rail := styles.CardRailStyle.Render(styles.AccentBar)
	pad := styles.FillLineBackgroundWith(rail, width, open, reset)
	rows := make([]string, 0, strings.Count(content, "\n")+3)
	rows = append(rows, pad)
	for _, line := range strings.Split(content, "\n") {
		row := rail
		if line != "" {
			row = rail + " " + line
		}
		rows = append(rows, styles.FillLineBackgroundWith(row, width, open, reset))
	}
	rows = append(rows, pad)
	box := strings.Join(rows, "\n")
	if pending > 1 {
		note := styles.CardHintStyle.Render("(+" + strconv.Itoa(pending-1) + " more pending)")
		return box + "\n" + note
	}
	return box
}

// cardTextWidth is the usable text width inside the card body: the panel width less the left
// rail prefix (cardRailWidth), floored at 1. Descriptions, questions, choice rows and the key
// legend are wrapped/sized to this width, and the selected-choice highlight spans it.
func cardTextWidth(width int) int {
	w := width - cardRailWidth
	if w < 1 {
		w = 1
	}
	return w
}

// truncate clips s to at most width display runes, appending "…" when it overflows.
// A non-positive width returns "". It keeps a long choice on a single row so the
// window's row count stays predictable.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// choiceWindowCap is the number of choice rows the box can show given its height
// budget: height less the non-choice chrome rows ([o] other + the key legend),
// floored at 0.
func choiceWindowCap(height int) int {
	capacity := height - choiceChromeRows
	if capacity < 0 {
		return 0
	}
	return capacity
}

// choiceWindow returns the [start,end) half-open slice of choices the box shows,
// scrolled so selected stays inside the window of at most capacity rows. With a
// capacity of 0 or an empty list it returns an empty window. When the list fits,
// the whole range is returned; otherwise the window slides so it is roughly
// centred on selected, clamped to the list bounds.
func choiceWindow(total, selected, capacity int) (int, int) {
	if total <= 0 || capacity <= 0 {
		return 0, 0
	}
	if capacity >= total {
		return 0, total
	}
	start := selected - capacity/2
	if start < 0 {
		start = 0
	}
	if start > total-capacity {
		start = total - capacity
	}
	return start, start + capacity
}
