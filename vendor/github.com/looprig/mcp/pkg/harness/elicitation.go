// This file turns a server's request for human input into a Harness gate, and a
// human's answer back into an MCP elicitation response.
//
// # The shape of the problem
//
// Elicitation is the one place where an untrusted third-party server gets to put
// words in front of a person and ask them to type something. Everything here
// follows from that. The server chooses the message, the field names, and the
// schema; none of it is an instruction to this host, and all of it is input to
// be validated before a human ever sees it (CLAUDE.md: validate at every
// boundary; design §Elicitation: "Unsupported or unsafe schema constructs are
// declined with a classified error").
//
// # Why a decline is the failure mode, and never an error
//
// Every refusal in this file returns ElicitDecline, not a Go error. MCP models
// "the human said no" as a first-class result, and a host that cannot safely ask
// the question is in exactly that position: it has decided, on the human's
// behalf, not to answer. Returning an error instead would make the server's
// request fail — which reads to the server as "this client is broken" rather
// than "this client refused", and on the initialize path would take the
// connection down over a schema this adapter simply would not render.
//
// The server is told "decline" and nothing else. Why it was declined goes to the
// host's Reporter, never to the server: "your field named api_key was rejected
// as credential-soliciting" is a probe result, and a server that can enumerate
// this host's rules can search for a phrasing that gets past them.
//
// # What is deliberately NOT here
//
// There is no response route back from a Harness Session's gate directory, and
// this file does not pretend otherwise. Deps.Gates is a host-implemented seam:
// the host renders the gate — in a TUI, over HTTP, or from a headless policy —
// and returns the answer. That is design §Elicitation's own picture, where the
// three renderers sit downstream of the gate. See the report accompanying this
// stage for why a real sessionruntime.Session cannot currently be that host.

package mcpharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/mcp/pkg/client"
)

// DefaultElicitationTimeout is the overall wall-clock bound on one elicitation.
//
// It is a WALL-CLOCK bound and that is the point (design §Elicitation: "active-
// time request timeout may pause, but an overall wall-clock deadline remains").
// A binding's Timeouts.Request bounds how long a server may take to answer US;
// this bounds how long a SERVER may keep a question open in front of a person.
// Without it, a server could park a gate forever — occupying a slot, holding a
// turn, and outliving anyone's interest in the answer — simply by never
// withdrawing it.
const DefaultElicitationTimeout = 5 * time.Minute

// maxPendingElicitations bounds how many elicitations one binding may have open
// at once.
//
// A server drives this number, so it needs a ceiling: elicitation is the one
// server-initiated request that consumes a scarce resource on this side — a
// human's attention — and a server that opens ten thousand of them would exhaust
// the host's gate directory and bury every real question. Past the bound, the
// newest is declined; the ones already in front of a person are not disturbed.
const maxPendingElicitations = 8

// Bounds on what one elicitation may put in front of a person. They are this
// adapter's own, and they bound a different thing from pkg/client's Limits:
// those stop a server exhausting this process, these stop a well-bounded request
// being an unrenderable one.
const (
	// maxElicitMessageBytes caps the server's prompt text.
	maxElicitMessageBytes = 4 << 10
	// maxElicitTitleBytes caps a rendered field label.
	maxElicitTitleBytes = 256
)

// elicitor serves one binding's elicitation requests. It holds the binding's
// state rather than looking it up, for the same reason handlersFor passes it: a
// server may elicit during its own initialization, while the Manager's table
// lock is held by a concurrent reconfiguration.
type elicitor struct {
	m  *Manager
	bs *bindingState
}

var _ client.ElicitationHandler = (*elicitor)(nil)

// Elicit serves one server request for human input.
//
// It is called on the connection's goroutine. ctx is the MCP request's context,
// so cancelling the request cancels the gate — which is what keeps design
// §Elicitation's "the originating MCP request remains cancellable" true without
// any bookkeeping of this file's own.
func (e *elicitor) Elicit(ctx context.Context, req client.ElicitRequest) (client.ElicitResult, error) {
	binding := e.bs.binding.Name

	if !e.bs.enterElicitation() {
		return e.decline(binding, fmt.Sprintf(
			"the server has %d elicitations open already; the newest was declined", maxPendingElicitations)), nil
	}
	defer e.bs.leaveElicitation()

	translated, err := e.translate(req)
	if err != nil {
		// A classified decline: the request was never renderable, so nobody was
		// asked. The server learns only that the answer is no.
		return e.decline(binding, err.Error()), nil
	}

	res, gateErr := e.ask(ctx, translated)
	if gateErr != nil {
		return e.cancelled(binding, gateErr.Error()), nil
	}
	return translated.answer(res)
}

// ask opens the gate and waits, under both a wall-clock deadline and the
// Manager's lifetime.
//
// The two cancellations are separate facts and both are required. ctx dies with
// the MCP request; m.ctx dies with the Manager, which is what makes design
// §Elicitation's "shutdown or interrupt resolves the request as cancelled" true
// — a Close during a pending elicitation must not wait for a human who has gone
// to lunch. context.AfterFunc is how the second is joined to the first: Go has
// no context merge, and a watcher goroutine per elicitation would outlive the
// ones nobody answers.
//
// The late-response check after the wait is the load-bearing part. GateOpener's
// contract says it must honor ctx, but a host is foreign code and this adapter
// does not get to assume it does: a host that answers after the deadline has
// produced an answer to a question that is no longer being asked, and design
// §Elicitation is explicit that late responses are rejected. So the context is
// re-checked and its answer wins over whatever OpenGate returned.
func (e *elicitor) ask(ctx context.Context, t *translation) (GateResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, e.m.elicitIn)
	defer cancel()
	stop := context.AfterFunc(e.m.ctx, cancel)
	defer stop()

	res, err := e.m.deps.Gates.OpenGate(callCtx, t.request)

	if callErr := callCtx.Err(); callErr != nil {
		// Late or abandoned. Whatever OpenGate said, it said it about a question
		// that had already been withdrawn, and adopting it now would let a slow
		// host commit a human's answer after the server stopped waiting for it.
		return GateResponse{}, fmt.Errorf("the request was withdrawn before it was answered: %w", callErr)
	}
	if err != nil {
		return GateResponse{}, fmt.Errorf("the gate could not be opened: %w", err)
	}
	return res, nil
}

// decline refuses a request without asking anyone, and tells the host why.
func (e *elicitor) decline(binding, why string) client.ElicitResult {
	e.m.report(Notice{
		Kind:    NoticeElicitationDeclined,
		Binding: binding,
		Message: why,
	})
	return client.ElicitResult{Action: client.ElicitDecline}
}

// cancelled resolves a request nobody answered. It is distinct from a decline:
// decline is a refusal, cancel is a withdrawal, and MCP gives them separate
// actions because a server may reasonably retry one and not the other.
func (e *elicitor) cancelled(binding, why string) client.ElicitResult {
	e.m.report(Notice{
		Kind:    NoticeElicitationDeclined,
		Binding: binding,
		Message: why,
	})
	return client.ElicitResult{Action: client.ElicitCancel}
}

// enterElicitation claims one of this binding's pending-elicitation slots.
func (bs *bindingState) enterElicitation() bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.eliciting >= maxPendingElicitations {
		return false
	}
	bs.eliciting++
	return true
}

// leaveElicitation releases a slot.
func (bs *bindingState) leaveElicitation() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.eliciting--
}

// translation is a validated elicitation: the gate to open, and what the answer
// must be turned back into.
//
// The two travel together because the second cannot be recovered from the first.
// A gate's form answers are map[string]string — that is gate.ParseFormAnswers'
// whole contract — while an MCP elicitation result must satisfy the server's
// JSON Schema, where a boolean field wants `true` and an integer field wants
// `42`. The field types are known only at translation, so they are carried
// rather than re-derived from a schema this adapter has already stopped
// trusting.
type translation struct {
	request GateRequest
	// fields maps a field name to the JSON type its answer must be re-encoded
	// as. It is nil for a URL elicitation, which has no answers.
	fields map[string]string
	mode   client.ElicitMode
}

// translate validates one request and builds its gate.
func (e *elicitor) translate(req client.ElicitRequest) (*translation, error) {
	if len(req.Message) > maxElicitMessageBytes {
		return nil, fmt.Errorf("the prompt is %d bytes, over this adapter's %d byte limit",
			len(req.Message), maxElicitMessageBytes)
	}
	switch req.Mode {
	case client.ElicitModeForm:
		return e.translateForm(req)
	case client.ElicitModeURL:
		return e.translateURL(req)
	default:
		// Fail closed on a mode this adapter does not know. A future MCP mode
		// must be declined, never guessed at: the modes differ in what they do
		// with the answer, and rendering an unknown one as a form would be this
		// host inventing a meaning for a server's request.
		return nil, fmt.Errorf("elicitation mode %q is not one this adapter serves", req.Mode)
	}
}

// elicitControls are the three answers a gate may come back with. They are the
// gate.FormAction* values, and they are reused verbatim for an open-url gate:
// the vocabulary is accept/decline/cancel on both sides of this file — MCP's
// ElicitResult actions and Harness's form actions agree — so a separate set for
// URL gates would be two names for one concept.
func elicitControls() []gate.Control {
	return []gate.Control{
		{Action: gate.FormActionAccept, Label: "Submit"},
		{Action: gate.FormActionDecline, Label: "Decline"},
		{Action: gate.FormActionCancel, Label: "Dismiss"},
	}
}

// answer turns a gate response back into an MCP elicitation result.
func (t *translation) answer(res GateResponse) (client.ElicitResult, error) {
	switch res.Action {
	case gate.FormActionAccept:
		if t.mode == client.ElicitModeURL {
			// A URL elicitation has no content: the human went and did the thing
			// (or said they did). Accept carries no answers.
			return client.ElicitResult{Action: client.ElicitAccept}, nil
		}
		content, err := t.encode(res.Values)
		if err != nil {
			// The host answered with something that does not fit the server's
			// schema. Declining is the only safe move: sending it would make the
			// SDK reject the result and fail the server's request, and coercing
			// it would put this adapter's guess where a human's answer should be.
			return client.ElicitResult{Action: client.ElicitDecline}, nil
		}
		return client.ElicitResult{Action: client.ElicitAccept, Content: content}, nil
	case gate.FormActionDecline:
		return client.ElicitResult{Action: client.ElicitDecline}, nil
	case gate.FormActionCancel:
		return client.ElicitResult{Action: client.ElicitCancel}, nil
	default:
		// A host that answered with an action nobody offered. Fail closed: an
		// unrecognized action is not an accept.
		return client.ElicitResult{Action: client.ElicitCancel}, nil
	}
}

// encode re-types the gate's string answers into the JSON the server's schema
// demands.
//
// A value for a field the schema does not have is refused rather than dropped.
// It means the host answered a different question from the one asked, and
// forwarding the rest would send a partial answer the server would read as
// complete.
func (t *translation) encode(values map[string]string) (json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		jsonType, ok := t.fields[name]
		if !ok {
			return nil, fmt.Errorf("the answer names field %q, which the schema does not declare", name)
		}
		raw, err := encodeField(jsonType, value)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		out[name] = raw
	}
	return json.Marshal(out)
}

// encodeField re-encodes one answer as its schema type.
func encodeField(jsonType, value string) (json.RawMessage, error) {
	switch jsonType {
	case jsonTypeString:
		return json.Marshal(value)
	case jsonTypeBoolean:
		// gate.ParseFormAnswers normalizes a confirm to exactly "true"/"false",
		// so anything else is a host that did not go through it.
		switch value {
		case "true":
			return json.RawMessage(`true`), nil
		case "false":
			return json.RawMessage(`false`), nil
		default:
			return nil, fmt.Errorf("%q is not a boolean", value)
		}
	case jsonTypeInteger:
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", value)
		}
		return json.Marshal(n)
	case jsonTypeNumber:
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", value)
		}
		return json.Marshal(f)
	default:
		return nil, fmt.Errorf("unsupported field type %q", jsonType)
	}
}

// The JSON Schema primitive types an MCP elicitation may request. MCP restricts
// an elicitation schema to a flat object of primitives, which is what makes the
// mapping onto gate.PromptSchema possible at all.
const (
	jsonTypeString  = "string"
	jsonTypeBoolean = "boolean"
	jsonTypeInteger = "integer"
	jsonTypeNumber  = "number"
)

// elicitSchema is the shape of an MCP elicitation's requestedSchema. It is a
// serialization boundary — a JSON Schema document — so it is decoded into a
// declared struct here and never carried as a map (CLAUDE.md: narrow to a
// concrete type immediately).
type elicitSchema struct {
	Type       string                `json:"type"`
	Properties map[string]elicitProp `json:"properties"`
	Required   []string              `json:"required"`
}

// elicitProp is one property of an elicitation schema.
type elicitProp struct {
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Enum        []json.RawMessage `json:"enum"`
	Format      string            `json:"format"`
	WriteOnly   bool              `json:"writeOnly"`
}

// translateForm validates a form request and builds its gate.
func (e *elicitor) translateForm(req client.ElicitRequest) (*translation, error) {
	var schema elicitSchema
	if err := json.Unmarshal(req.Schema, &schema); err != nil {
		return nil, fmt.Errorf("the requested schema is not readable JSON")
	}
	if schema.Type != "object" {
		return nil, fmt.Errorf("the requested schema is of type %q; an elicitation schema must be an object", schema.Type)
	}
	if len(schema.Properties) == 0 {
		return nil, fmt.Errorf("the requested schema declares no properties")
	}

	required := make(map[string]struct{}, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = struct{}{}
	}

	// Sorted, because Go's map iteration is randomized and a form whose fields
	// reshuffle between renders is a form a person cannot trust. JSON object
	// order is not recoverable from a decoded map, so name order is the only
	// stable order available.
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	fields := make([]gate.Field, 0, len(names))
	types := make(map[string]string, len(names))
	freeText := false
	for _, name := range names {
		prop := schema.Properties[name]
		if reason, solicits := solicitsCredential(name, prop); solicits {
			// Design §Elicitation: form requests that solicit credentials are
			// rejected, and sensitive authorization goes through URL elicitation
			// or pkg/auth instead. The whole form dies, not the field: a form
			// missing the field that mattered is one a person would fill in
			// anyway, and the server would receive a partial answer it never
			// asked for.
			return nil, fmt.Errorf("field %q solicits a credential (%s); "+
				"sensitive authorization must go through URL elicitation or the auth package, "+
				"never through durable form values", name, reason)
		}
		field, jsonType, err := translateField(name, prop)
		if err != nil {
			return nil, err
		}
		_, isRequired := required[name]
		field.Required = isRequired
		if field.Kind == gate.FieldText {
			freeText = true
		}
		fields = append(fields, field)
		types[name] = jsonType
	}

	// The body rule, applied after the fields so a structural signal — a
	// "format": "password", a field named "apiKey" — is what the Notice names
	// when both would fire. Only free-text forms are scanned: a secret cannot be
	// typed into a confirm or a select, so a body's request has nowhere to land.
	if freeText {
		if reason, solicits := bodySolicitsCredential(req.Message); solicits {
			return nil, fmt.Errorf("the request's message solicits a credential (%s); "+
				"sensitive authorization must go through URL elicitation or the auth package, "+
				"never through durable form values", reason)
		}
	}

	schemaOut := gate.PromptSchema{Fields: fields}
	// Validate against Harness's own form contract before anything is opened.
	// It is the authority on what a form gate can express — the field count, the
	// name bounds, the answerable kinds — and re-deriving those rules here would
	// be this adapter guessing at another package's invariants.
	if err := gate.ValidateFormSchema(schemaOut); err != nil {
		return nil, fmt.Errorf("the requested schema is not a form this host can render: %w", err)
	}

	title := fmt.Sprintf("%s requests input", e.bs.binding.Name)
	payload := gate.FormPayload{Title: title, Body: req.Message, Schema: schemaOut}
	return &translation{
		mode:   client.ElicitModeForm,
		fields: types,
		request: GateRequest{
			Kind:    gate.KindForm,
			Payload: payload,
			Prompt: gate.Prompt{
				Title:    title,
				Body:     req.Message,
				Schema:   schemaOut,
				Controls: elicitControls(),
			},
			// A form gate MAY be restorable as far as gate.ValidateGate is
			// concerned, but this one must not be: design §Elicitation says
			// pending elicitation is tied to a live connection, and restore
			// resolves the old gate as unavailable rather than resuming a stale
			// server request. A restorable gate would outlive the connection its
			// answer has to be delivered on, leaving a person answering a
			// question no server is still listening for.
			Restorable: false,
			Binding:    e.bs.binding.Name,
			LoopID:     e.bs.loopID(),
		},
	}, nil
}

// translateField maps one schema property onto a gate field.
func translateField(name string, prop elicitProp) (gate.Field, string, error) {
	label := prop.Title
	if label == "" {
		label = name
	}
	if len(label) > maxElicitTitleBytes {
		label = label[:maxElicitTitleBytes]
	}

	switch prop.Type {
	case jsonTypeBoolean:
		if len(prop.Enum) > 0 {
			return gate.Field{}, "", fmt.Errorf("field %q is a boolean with an enum, which is not a form this host renders", name)
		}
		return gate.Field{Name: name, Label: label, Kind: gate.FieldConfirm}, jsonTypeBoolean, nil
	case jsonTypeString, jsonTypeInteger, jsonTypeNumber:
		if len(prop.Enum) == 0 {
			// Integers and numbers become text fields and are re-typed on the
			// way back (see encodeField). gate.FieldKind has no numeric kind,
			// and inventing one to carry a server's number would be this
			// adapter's protocol reaching into Harness's.
			return gate.Field{Name: name, Label: label, Kind: gate.FieldText}, prop.Type, nil
		}
		options, err := enumOptions(name, prop.Enum)
		if err != nil {
			return gate.Field{}, "", err
		}
		return gate.Field{Name: name, Label: label, Kind: gate.FieldSelect, Options: options}, prop.Type, nil
	case "":
		return gate.Field{}, "", fmt.Errorf("field %q declares no type", name)
	default:
		// Objects, arrays, null, and anything MCP adds later. MCP already
		// restricts an elicitation schema to flat primitives, so this is a
		// server outside its own spec — decline rather than flatten it into a
		// text box a person would fill in wrongly.
		return gate.Field{}, "", fmt.Errorf("field %q is of type %q, which is not a primitive this host renders", name, prop.Type)
	}
}

// enumOptions renders an enum as select options.
//
// Every enum member becomes a string, whatever its JSON type, because a
// gate.Option's Value is a string and a form answer is a string. The
// re-encoding on the way back restores the schema's type, so an integer enum of
// [1, 2] offers "1" and "2" and answers 1 or 2.
func enumOptions(name string, members []json.RawMessage) ([]gate.Option, error) {
	options := make([]gate.Option, 0, len(members))
	for _, raw := range members {
		value, err := enumValue(raw)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		options = append(options, gate.Option{Value: value, Label: value})
	}
	return options, nil
}

// enumValue renders one enum member as the string a person picks.
func enumValue(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			// gate.ValidateFormSchema refuses an empty option value, and it is
			// right to: an unlabeled choice is one nobody can pick on purpose.
			return "", fmt.Errorf("an enum member is the empty string")
		}
		return s, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		// Rendered through the same formatting a re-encode reverses, so the
		// option a person picked round-trips to the number the server declared.
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	return "", fmt.Errorf("an enum member is neither a string nor a number")
}

// translateURL validates a URL request and builds its gate.
func (e *elicitor) translateURL(req client.ElicitRequest) (*translation, error) {
	origin, err := bareOrigin(req.URL)
	if err != nil {
		return nil, err
	}

	// The Prompt of an open-url gate is HOST-AUTHORED, end to end. req.Message is
	// not read here, and that is the security property — not an omission.
	//
	// The Prompt is the public gate envelope: it rides event.GateOpened into a
	// journal. gate.OpenURLPayload excludes the action URL structurally (its URL
	// field is `json:"-"`, the durable codec type has no URL field, and a strict
	// decode rejects a `url` key) because that URL is a CREDENTIAL — it carries
	// the PKCE challenge, the `state` token, sometimes a bearer token. All of
	// that is worth nothing if the server can hand the host the same secrets as
	// prose and have the host render them into the envelope itself.
	//
	// And it could: req.Message is chosen by the server and bounded only in
	// length. Scanning it for a URL would not fix this. The leak is not "a URL
	// appears in the message", it is "server-authored ephemeral text reaches a
	// durable record" — a server needs no URL to write "state=..." into a
	// journal, and any scanner would be a rule about the inputs someone thought
	// of rather than a property of every input a server can send. So the message
	// is not sanitized, not truncated, and not carried: it is never read on this
	// path at all. That is structural — there is no string here for a server to
	// choose.
	//
	// Nothing is lost that a human needs. The trust decision on an open-url gate
	// is made on the ORIGIN, and the origin is validated (bareOrigin) and stated
	// below; the renderer labels it and adds its own trust caution. A server's
	// account of why it wants authorization is exactly the text a hostile server
	// would forge, and it is not evidence for the decision being asked.
	//
	// This is the opposite of KindForm, where req.Message is the QUESTION and is
	// journaled deliberately (see translateForm): a form gate has no
	// durably-excluded secret for a message to re-introduce.
	title := fmt.Sprintf("%s asks you to open a link", e.bs.binding.Name)
	body := fmt.Sprintf("This will open %s in your browser.", origin)
	return &translation{
		mode: client.ElicitModeURL,
		request: GateRequest{
			Kind: gate.KindOpenURL,
			Payload: gate.OpenURLPayload{
				DisplayOrigin: origin,
				URL:           req.URL,
				// The server promises a completion notification only when it
				// gave the request an id to correlate one with. Claiming
				// RequiresCompletion without an id would park a human on a
				// signal that can never arrive.
				RequiresCompletion: req.ElicitationID != "",
			},
			Prompt: gate.Prompt{
				Title: title,
				Body:  body,
				// The origin travels as a field, not only as prose. It is what
				// the human's trust decision is made on, and gate.ValidateGate
				// REQUIRES a bare one here so a renderer can display it AS a
				// validated origin rather than parsing it back out of Body.
				// This is the same value bareOrigin already validated for the
				// durable DisplayOrigin — one origin, one check, two places
				// that need it.
				Origin:   origin,
				Controls: elicitControls(),
			},
			// Never restorable. gate.ValidateGate REFUSES a restorable open-url
			// gate, because the action target is not journaled and a restored
			// one could only present an origin with no URL behind it.
			Restorable: false,
			Binding:    e.bs.binding.Name,
			LoopID:     e.bs.loopID(),
		},
	}, nil
}

// bareOrigin validates a server's action URL and reduces it to the origin a
// human is shown.
//
// The scheme check is the security boundary. A server picks this URL and a host
// is being asked to hand it to a browser, so "javascript:", "data:", "file:",
// and anything else that is not an ordinary web fetch is refused outright —
// those are not links, they are code and local reads wearing a link's clothes.
//
// The result is scheme+host and nothing else, which is what gate.OpenURLPayload
// requires: it validates DisplayOrigin as a bare origin at both codec
// boundaries, precisely so a caller cannot pass the full authorization URL as
// the "origin" and have its secrets journaled verbatim.
func bareOrigin(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("the request carries no URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("the request's URL is not parseable")
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("the request's URL uses scheme %q; only http and https may be opened", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("the request's URL has no host")
	}
	// Host, never User: userinfo is a credential, and gate.OpenURLPayload
	// rejects an origin carrying one anyway.
	return parsed.Scheme + "://" + parsed.Host, nil
}

// # The credential rule
//
// Design §Elicitation: "form requests that solicit passwords, tokens, private
// keys, or other credentials are rejected", and "sensitive authorization is
// performed through URL elicitation or the auth package, not through durable
// form values". This is where that is decided, and the rule is stated here in
// full because a security rule nobody can find is one nobody can review.
//
// # What this rule IS, and what it is NOT
//
// It is a good-faith guardrail against ACCIDENTAL solicitation by a COOPERATIVE
// server: the integrator who adds an "api_key" field without thinking about
// where the answer ends up. Against that, it works, and it is worth having.
//
// It is NOT a security boundary against a HOSTILE server, and must never be
// relied on as one. Three reasons, all structural and none fixable by adding
// words to a list:
//
//   - It is a denylist over English tokens. Intent is not recoverable from a
//     third-party-authored schema; the rule can only recognize the shapes a
//     server chose to write down.
//   - "format": "password" and "writeOnly": true are the only RELIABLE signals
//     here, and they are reliable only because a server volunteered them. A
//     server that wants the value journaled simply does not declare them.
//   - Non-English, transliterated, or oblique naming walks past it. A field
//     named "mot_de_passe", or "value" under a message reading "the string from
//     your dashboard's top-right panel", tokenizes to nothing on either list.
//
// # The residual risk, stated plainly
//
// Form answers are journaled UNREDACTED — they are user-authored content, the
// same as command.UserInput. Redaction is not a second layer behind this check;
// there is no second layer. So: a determined server CAN still get a secret into
// the durable journal, by naming a field innocuously and asking for the secret
// in words this rule does not recognize. What this rule buys is that it has to
// be determined and deliberate, not careless. That is the whole claim. Anything
// stronger — a human confirmation before a free-text answer is journaled, or a
// per-binding policy refusing free-text fields from untrusted servers — is a
// control that does not exist yet and is an owner's decision, not this file's.
//
// A field solicits a credential when ANY of:
//
//  1. its JSON Schema says so: "format": "password", or "writeOnly": true. Both
//     are the schema author's own declaration that the value is a secret.
//  2. its NAME tokenizes to a credential word, or an adjacent credential pair.
//  3. its TITLE does the same. The title is what a person actually reads, so a
//     field named "f1" titled "API Key" is caught by this and not by (2).
//
// A field's Description is deliberately NOT matched. It is per-field prose, and
// prose about credentials is mostly prose telling a person NOT to enter one —
// "this is not your password" would reject itself. A rule that fires on that is
// one an integrator learns to work around by deleting their own safety text.
//
// # The body rule
//
// The form's MESSAGE is matched, under a narrower rule, because the field
// signals alone left an obvious hole: a server can name a field "value", give it
// no title, and put "paste your API key here" in the message. Every check above
// passes, and the answer is journaled. bodySolicitsCredential closes that
// particular shape.
//
// The body is prose, so the Description argument applies to it too — which is
// why the body rule is not "contains a credential token". Two conditions bound
// the false positives, and a false positive here is expensive: it declines a
// legitimate server's form for the words in its own message.
//
//  1. The body must pair a credential token with a SOLICITATION VERB — "enter",
//     "paste", "provide". "We never ask for your password" carries the token but
//     no verb, and is not a solicitation.
//  2. The form must have at least one FREE-TEXT field. A secret cannot be typed
//     into a confirm or a select, so a form whose every field is one of those has
//     nowhere for the body's request to land, whatever the body says.
//
// Those bounds are honest about what they leave: "we will never ask you to enter
// your password" alongside a free-text field still trips condition 1, because
// negation is not something a token list can see. That is a known, accepted
// false positive — it declines, an operator reads the Notice, and no secret is
// journaled. Fail closed.
//
// When the body solicits, the WHOLE form is rejected — the same rule as a bad
// field, for the same reason. There is no narrower unit available: the body is
// addressed to the form, not to a field, so there is no way to know WHICH
// free-text field it meant. Dropping every free-text field and rendering the
// rest would hand the server a partial answer it never asked for.
//
// Tokenization splits on non-alphanumerics AND camelCase boundaries, which is
// what makes the word list safe to keep short: "apiKey", "api_key", and
// "API-KEY" all tokenize to ["api","key"], while "author" tokenizes to
// ["author"] and so does not collide with the "auth" token. Matching raw
// substrings instead would reject "author", "authority", and "tokenizer".
//
// It is deliberately over-inclusive within that. A field genuinely named "token"
// in an MCP form is a credential essentially every time, and the cost of the
// rare false positive is a declined form and a Notice an operator can read —
// against the cost of a false negative, which is a bearer token in a durable
// journal record forever.

// credentialTokens are the single tokens that mark a field as a credential.
var credentialTokens = map[string]struct{}{
	"password": {}, "passwd": {}, "pwd": {}, "passphrase": {},
	"secret": {}, "token": {}, "credential": {}, "credentials": {},
	"auth": {}, "authorization": {}, "bearer": {}, "apikey": {},
	"otp": {}, "totp": {}, "mfa": {}, "pin": {}, "cvv": {}, "cvc": {},
}

// credentialPairs are adjacent token pairs that mark a field as a credential.
// They exist for the compounds whose halves are individually innocent: a "key"
// is a map key, and an "access" is a verb, but "accessKey" is a credential.
var credentialPairs = [][2]string{
	{"api", "key"}, {"access", "key"}, {"access", "token"},
	{"refresh", "token"}, {"private", "key"}, {"secret", "key"},
	{"client", "secret"}, {"signing", "key"}, {"ssh", "key"},
	{"session", "key"}, {"security", "code"}, {"card", "number"},
}

// solicitationVerbs are the verbs that turn prose ABOUT a credential into a
// request FOR one. The list is short and imperative on purpose: it is the only
// thing standing between the body rule and rejecting every form whose message
// mentions security at all. "submit" and "send" are absent deliberately — they
// are what every form's own button says.
var solicitationVerbs = map[string]struct{}{
	"enter": {}, "paste": {}, "provide": {}, "supply": {}, "type": {},
	"input": {}, "insert": {}, "copy": {}, "give": {},
}

// solicitsCredential reports whether a FIELD is asking for a secret, and which
// rule caught it. The reason is for the host's Reporter; it never reaches the
// server.
//
// This is a good-faith guardrail against accidental solicitation by a
// cooperative server, NOT a security boundary against a hostile one. It cannot
// be one: it is a denylist over English tokens, and intent is not inferable from
// a schema a third party wrote. "format": "password" and "writeOnly": true are
// dependable only where a server chose to declare them, and a server that wants
// the answer journaled will not; non-English or oblique naming ("mot_de_passe",
// a field called "value") reads as innocent to every rule below.
//
// Since form answers are journaled unredacted, with no redaction layer behind
// this check, a determined server can still put a secret in the durable journal.
// See "The credential rule" above for the full statement of the limit.
func solicitsCredential(name string, prop elicitProp) (string, bool) {
	if strings.EqualFold(prop.Format, "password") {
		return `the schema declares "format": "password"`, true
	}
	if prop.WriteOnly {
		return `the schema declares "writeOnly": true`, true
	}
	if reason, ok := credentialWords(name); ok {
		return "its name " + reason, true
	}
	if reason, ok := credentialWords(prop.Title); ok {
		return "its label " + reason, true
	}
	return "", false
}

// bodySolicitsCredential reports whether the form's MESSAGE asks a person for a
// secret, and which words caught it.
//
// Unlike the field rule this requires TWO signals — a credential token and a
// solicitation verb — because the body is prose and a bare token in prose is far
// more often a disclaimer than a request. The caller applies the third
// condition: only a form with a free-text field is scanned at all. See "The body
// rule" above.
func bodySolicitsCredential(body string) (string, bool) {
	reason, ok := credentialWords(body)
	if !ok {
		return "", false
	}
	for _, token := range tokenize(body) {
		if _, isVerb := solicitationVerbs[token]; isVerb {
			return fmt.Sprintf("%s alongside the solicitation verb %q", reason, token), true
		}
	}
	return "", false
}

// credentialWords reports whether s tokenizes to a credential word or pair.
func credentialWords(s string) (string, bool) {
	tokens := tokenize(s)
	for _, token := range tokens {
		if _, ok := credentialTokens[token]; ok {
			return fmt.Sprintf("contains the token %q", token), true
		}
	}
	for i := 0; i+1 < len(tokens); i++ {
		for _, pair := range credentialPairs {
			if tokens[i] == pair[0] && tokens[i+1] == pair[1] {
				return fmt.Sprintf("contains the token pair %q %q", pair[0], pair[1]), true
			}
		}
	}
	return "", false
}

// tokenize splits an identifier into lowercase words, breaking on
// non-alphanumerics and on camelCase boundaries.
//
// The camelCase split is what lets the word lists stay short and exact. Without
// it the lists would have to match substrings, and a substring match for "auth"
// rejects "author" while a substring match for "pin" rejects "pinned",
// "spinner", and "shipping".
func tokenize(s string) []string {
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			tokens = append(tokens, strings.ToLower(string(current)))
			current = nil
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		switch {
		case unicode.IsUpper(r):
			// A boundary before an uppercase run's start ("apiKey" -> api|Key)
			// and before its last letter when a lowercase follows ("APIKey" ->
			// API|Key).
			if i > 0 && unicode.IsLower(runes[i-1]) {
				flush()
			} else if i > 0 && i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]) {
				flush()
			}
			current = append(current, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			current = append(current, r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}
