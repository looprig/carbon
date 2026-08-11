// Package httpconn is the session wrapper this module's two network transports
// share: a protocol.Conn that delegates to an SDK-backed session and classifies
// everything that can go wrong underneath it.
//
// It exists for the reason internal/httpsec does. Streamable HTTP and legacy SSE
// carry the same protocol over the same HTTP layer, so a failure means the same
// thing on both, and the taxonomy that says so should be written once. The
// alternative is two copies of a hierarchy of causes that is subtle in exactly
// the places it matters — which cause outranks which, and which of them may
// safely be rendered into a message — and a second copy of that is a second
// place for a credential to end up in a log.
//
// Only the transport's display name differs, which is why it is a field.
package httpconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/looprig/mcp/internal/httpsec"
	"github.com/looprig/mcp/internal/limits"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/pkg/auth"
	"github.com/looprig/mcp/pkg/client"
)

// Operation names carried by the errors these transports return. They are the
// shared vocabulary of the taxonomy: a caller branches on an Error's Op, so two
// transports naming the same operation differently would be two things to learn.
const (
	OpNew        = "new"
	OpConnect    = "connect"
	OpInitialize = "initialize"
	OpClose      = "close"
	// The catalog list operations, named as they appear in an error.
	OpListTools             = "list_tools"
	OpListPrompts           = "list_prompts"
	OpListResources         = "list_resources"
	OpListResourceTemplates = "list_resource_templates"
	// The request operations.
	OpCallTool     = "call_tool"
	OpGetPrompt    = "get_prompt"
	OpReadResource = "read_resource"
	OpSubscribe    = "subscribe"
	OpUnsubscribe  = "unsubscribe"
	OpSetLogLevel  = "set_log_level"
)

// Conn is one MCP session over an HTTP transport.
//
// It is thin, and it is thin on purpose: the SDK's transport owns the wire, so
// there is nothing here to hold but the session and the record of what went
// wrong underneath it.
//
// What it adds to protocol.Session — which already implements protocol.Conn — is
// classification. Only this layer can tell "the server spoke badly" from "the
// remote server is gone" from "a credential was refused", because only it holds
// the Diagnostics that recorded the cause at the point the cause was still a
// value. A transport that returned the session directly would report every one
// of those as the same opaque failure.
type Conn struct {
	session *protocol.Session
	diags   *httpsec.Diagnostics
	// name is the transport's display name, as it appears in an error's message
	// ("the SSE server answered 500"). It is this module's own word for the
	// transport, never anything a server supplied.
	name string
	// endpoint and origin are held only so that cause can keep the request URL
	// out of an error message.
	endpoint string
	origin   string
}

// New returns the Conn for one session. name is the transport's display name;
// endpoint and origin are its validated URL and canonical origin, held so that
// a failure can be described without quoting the request URL.
func New(session *protocol.Session, diags *httpsec.Diagnostics, name, endpoint, origin string) *Conn {
	return &Conn{session: session, diags: diags, name: name, endpoint: endpoint, origin: origin}
}

// Initialize performs the MCP handshake.
func (c *Conn) Initialize(ctx context.Context) (protocol.InitializeResult, error) {
	res, err := c.session.Initialize(ctx)
	if err != nil {
		return protocol.InitializeResult{}, c.classify(ctx, OpInitialize, err)
	}
	return res, nil
}

// The catalog list methods. Each is a straight delegation to the session, with
// the transport's own classification applied to a failure: only this layer can
// tell "the server spoke badly" from "the remote server is gone", and a list that
// fails during discovery must be classified the same way a handshake would be.
//
// The four are generated through listVia rather than written out because they
// differ only in the method they call — and a hand-written fourth copy is where
// the classification would eventually go missing.

// ListTools fetches one page of tools.
func (c *Conn) ListTools(ctx context.Context, cursor string) (protocol.ToolPage, error) {
	return listVia(ctx, c, OpListTools, cursor, c.session.ListTools)
}

// ListPrompts fetches one page of prompts.
func (c *Conn) ListPrompts(ctx context.Context, cursor string) (protocol.PromptPage, error) {
	return listVia(ctx, c, OpListPrompts, cursor, c.session.ListPrompts)
}

// ListResources fetches one page of resources.
func (c *Conn) ListResources(ctx context.Context, cursor string) (protocol.ResourcePage, error) {
	return listVia(ctx, c, OpListResources, cursor, c.session.ListResources)
}

// ListResourceTemplates fetches one page of resource templates.
func (c *Conn) ListResourceTemplates(ctx context.Context, cursor string) (protocol.ResourceTemplatePage, error) {
	return listVia(ctx, c, OpListResourceTemplates, cursor, c.session.ListResourceTemplates)
}

// listVia runs one list method and classifies its failure. The page type is the
// only thing that varies, so it is the only type parameter.
func listVia[P any](
	ctx context.Context,
	c *Conn,
	op string,
	cursor string,
	fetch func(context.Context, string) (P, error),
) (P, error) {
	page, err := fetch(ctx, cursor)
	if err != nil {
		var zero P
		return zero, c.classify(ctx, op, err)
	}
	return page, nil
}

// The request methods. Like the list methods they delegate to the session and
// classify the failure here, where the cause is knowable.

// CallTool invokes a tool by its raw server name.
func (c *Conn) CallTool(ctx context.Context, rawName string, args json.RawMessage, opts protocol.CallOptions) (protocol.ToolResult, error) {
	res, err := c.session.CallTool(ctx, rawName, args, opts)
	if err != nil {
		return protocol.ToolResult{}, c.classify(ctx, OpCallTool, err)
	}
	return res, nil
}

// GetPrompt fetches a prompt's messages.
func (c *Conn) GetPrompt(ctx context.Context, name string, args map[string]string) (protocol.PromptResult, error) {
	res, err := c.session.GetPrompt(ctx, name, args)
	if err != nil {
		return protocol.PromptResult{}, c.classify(ctx, OpGetPrompt, err)
	}
	return res, nil
}

// ReadResource reads a resource by URI.
func (c *Conn) ReadResource(ctx context.Context, uri string) (protocol.ResourceResult, error) {
	res, err := c.session.ReadResource(ctx, uri)
	if err != nil {
		return protocol.ResourceResult{}, c.classify(ctx, OpReadResource, err)
	}
	return res, nil
}

// Subscribe asks the server to report changes to a resource.
func (c *Conn) Subscribe(ctx context.Context, uri string) error {
	if err := c.session.Subscribe(ctx, uri); err != nil {
		return c.classify(ctx, OpSubscribe, err)
	}
	return nil
}

// Unsubscribe asks the server to stop reporting changes to a resource.
func (c *Conn) Unsubscribe(ctx context.Context, uri string) error {
	if err := c.session.Unsubscribe(ctx, uri); err != nil {
		return c.classify(ctx, OpUnsubscribe, err)
	}
	return nil
}

// SetLogLevel asks the server to send logs at or above level.
func (c *Conn) SetLogLevel(ctx context.Context, level string) error {
	if err := c.session.SetLogLevel(ctx, level); err != nil {
		return c.classify(ctx, OpSetLogLevel, err)
	}
	return nil
}

// Close ends the session. The SDK's close drains the conversation and then
// issues the DELETE that releases the server's session state.
//
// It is idempotent, via Session. A server that has already forgotten the session
// is not a close failure: there is nothing left to release, which is what Close
// is for.
func (c *Conn) Close(ctx context.Context) error {
	if err := c.session.Close(ctx); err != nil {
		return client.NewError(client.FailureTransportClosed, "", OpClose,
			"the "+c.name+" session could not be closed", err)
	}
	return nil
}

// classify turns a session failure into a typed error.
//
// The order is a hierarchy of causes, most specific first, because the SDK
// reports very different things identically — "the connection failed" covers a
// refused credential, a 500, an oversized body and a cancelled caller alike.
// The specific causes come from diagnostics, which recorded them at the point
// they happened; the SDK's error text is only the symptom, and much of it is
// flattened with %v on the way up, so the cause is not reliably in the chain by
// the time it arrives here.
//
// The caller's own context is read first: a cancelled caller is a cancellation,
// whatever the read that noticed it returned.
func (c *Conn) classify(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return client.NewError(client.FailureCancelled, "", op,
			"the "+c.name+" transport was cancelled", err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return client.NewError(deadlineClass(op), "", op,
			"the "+c.name+" server did not answer in time", err)
	}

	// An auth failure, from the provider or from the chain. The message is
	// always this package's own text plus the auth error's class: an auth
	// error's cause routinely quotes a URL or a header, and client.Error renders
	// a wrapped cause verbatim when no explicit message is given.
	if aerr := c.diags.AuthError(); aerr != nil {
		return client.NewError(authClass(aerr), "", op, authMessage(aerr), aerr)
	}
	var aerr *auth.Error
	if errors.As(err, &aerr) {
		return client.NewError(authClass(aerr), "", op, authMessage(aerr), err)
	}

	// A bound this transport enforced. It is reported ahead of the HTTP status
	// because a 200 that overruns the limit is the limit's verdict, not the
	// status's.
	if lerr := c.diags.LimitError(); lerr != nil {
		return client.NewError(client.FailureLimitExceeded, "", op,
			"the "+c.name+" server's response exceeded a limit: "+lerr.Error(), lerr)
	}

	// A server that started a message and stopped. It is reported ahead of the
	// status for the reason the limit is: whatever the response line said, the
	// stream is what ended the session.
	if serr := c.diags.StallError(); serr != nil {
		return client.NewError(client.FailureServerProtocol, "", op,
			"the "+c.name+" server stalled mid-frame: "+serr.Error(), serr)
	}

	if status, ok := c.diags.Status(); ok {
		return client.NewError(statusClass(status), "", op,
			fmt.Sprintf("the %s server answered %d %s",
				c.name, status, http.StatusText(status)), err)
	}

	// Nothing was recorded: the request never got far enough to have a status,
	// or the session broke on something the server said rather than on the
	// transport underneath it. Either way this is not a class this package can
	// name more precisely without guessing.
	return client.NewError(client.FailureServerProtocol, "", op, c.cause(err), err)
}

// cause renders err for a client.Error's message, with the request URL kept out
// of it.
//
// Its existence is not tidiness. client.Error renders its wrapped cause verbatim
// when no explicit message is given, and every network failure this transport
// can suffer arrives wrapped in a *url.Error — whose text quotes the request
// URL, whose query is a place people put access tokens. Leaving the message
// empty therefore prints the credential, which a test here proves.
//
// So the message is never the chain's text. It is built from the one part of it
// that is URL-free by construction: url.Error's inner Err, which holds the
// actual fault ("connection refused") without the URL its parent formats in.
// The endpoint is then scrubbed anyway, because "by construction" is a claim
// about the stdlib's formatting that this package would rather not bet a
// credential on. An unrecognized error contributes no text at all — the class
// and the operation still say what happened, and the cause stays reachable
// through errors.Is and errors.As for a caller that wants to inspect it.
func (c *Conn) cause(err error) string {
	var uerr *url.Error
	if !errors.As(err, &uerr) || uerr.Err == nil {
		return "the " + c.name + " request failed"
	}
	text := uerr.Err.Error()
	if text == "" {
		return "the " + c.name + " request failed"
	}
	// Belt and braces: if the URL appears anyway, it becomes the origin, which
	// is what RedactedOrigin would have said.
	text = strings.ReplaceAll(text, c.endpoint, c.origin)
	return "the " + c.name + " request failed: " + text
}

// authClass maps an auth failure onto the client taxonomy. The mapping is the
// fixed 1:1 one documented in pkg/auth/errors.go — that package is a leaf and
// must not import pkg/client, so a transport is where the translation lives.
//
// An unknown class maps to FailureAuthFailed rather than to something more
// specific: a class this code does not recognize is an auth failure whose nature
// it cannot state, and inventing a nature for it would be a fail-open.
func authClass(err *auth.Error) client.FailureClass {
	switch err.Class {
	case auth.ClassInvalidConfig:
		return client.FailureInvalidConfig
	case auth.ClassNoToken, auth.ClassRequired:
		return client.FailureAuthRequired
	case auth.ClassDenied:
		return client.FailureAuthDenied
	case auth.ClassExpired:
		return client.FailureAuthExpired
	default:
		return client.FailureAuthFailed
	}
}

// authMessage renders an auth failure for a client.Error's message.
//
// It uses the auth error's own text, which is safe by construction: pkg/auth
// bounds and normalizes it, refuses to render its cause, and documents that its
// Msg is secret-free. What must not happen is leaving the message empty and
// letting client.Error substitute the wrapped chain, which is not any of those
// things.
func authMessage(err *auth.Error) string {
	text, _ := limits.TruncateText(err.Error(), auth.MaxMessageBytes)
	return text
}

// statusClass maps an HTTP status onto the client taxonomy.
//
// 401 and 403 become auth classes because that is what they mean, and because a
// caller deciding whether to start a login should not have to parse a number out
// of a string. 401 is Required rather than Expired: the distinction is whether a
// credential was sent and has aged out, which is the provider's knowledge and
// not the status line's — an OAuthProvider that refreshed and still got 401
// reports Expired itself, through the path above.
//
// Everything else is the remote's HTTP behavior, which is one class: a 404, a
// 429 and a 500 differ in what to do about them, not in what they are, and the
// status is in the message for the caller that cares.
func statusClass(status int) client.FailureClass {
	switch status {
	case http.StatusUnauthorized:
		return client.FailureAuthRequired
	case http.StatusForbidden:
		return client.FailureAuthDenied
	default:
		return client.FailureRemoteHTTP
	}
}

// deadlineClass reports which failure a blown deadline is, for op.
//
// The distinction is the caller's to act on, not cosmetic: a startup timeout
// means the binding never came up and may be retried or dropped, while a
// deadline on a request means this call ran out of time on a binding that is
// otherwise fine. Reporting every timeout as a startup timeout — which is what
// this did when startup was the only thing that could time out — would tell a
// caller its healthy binding had failed to start.
func deadlineClass(op string) client.FailureClass {
	switch op {
	case OpConnect, OpInitialize:
		return client.FailureStartupTimeout
	default:
		return client.FailureDeadline
	}
}
