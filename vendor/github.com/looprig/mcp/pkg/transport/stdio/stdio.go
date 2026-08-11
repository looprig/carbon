// Package stdio is the MCP stdio transport: it runs an MCP server as a child
// process and speaks the protocol over that child's stdin and stdout.
//
// # What this transport owns
//
// It owns the process. The MCP Go SDK ships a command transport that also does
// (it takes an *exec.Cmd and starts it), and this package deliberately does not
// use it: that transport starts the command itself — leaving no seam for a host
// that confines its servers — and signals only the process it started, which
// leaves anything the server spawned running with the pipes it inherited. This
// package therefore starts, groups, terminates and reaps the child itself, and
// uses the SDK for what the SDK is for: framing and the protocol.
//
// # Trust
//
// The child is untrusted from the moment it exists. Its stdout is the protocol
// and is read by nothing but the SDK; its stderr is bounded diagnostics; its
// exit status is a fact to report, never a fact to believe. Its environment is
// built from an allowlist rather than inherited, because a server that does not
// need a credential must not be handed every credential this process holds.
//
// # Shutdown
//
// Shutdown is graceful by necessity, not by politeness. The MCP stdio shutdown
// is "close the child's stdin", and an SDK-based server treats a read of EOF as
// "stop now" — dropping any reply it had not written yet and exiting on an
// error. So Close drains the MCP session first (no new requests, in-flight ones
// finish), and only then closes the stream, terminates the group and reaps it.
package stdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/limits"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/pkg/client"
)

// kind is the transport kind this package reports.
const kind = "stdio"

// The contracts this package exists to satisfy.
var (
	_ client.TransportFactory = (*factory)(nil)
	_ protocol.Conn           = (*conn)(nil)
)

// Operation names carried by the errors this package returns.
const (
	opNew        = "new"
	opConnect    = "connect"
	opInitialize = "initialize"
	opClose      = "close"
	// The catalog list operations, named as they appear in an error.
	opListTools             = "list_tools"
	opListPrompts           = "list_prompts"
	opListResources         = "list_resources"
	opListResourceTemplates = "list_resource_templates"
	// The request operations.
	opCallTool     = "call_tool"
	opGetPrompt    = "get_prompt"
	opReadResource = "read_resource"
	opSubscribe    = "subscribe"
	opUnsubscribe  = "unsubscribe"
	opSetLogLevel  = "set_log_level"
)

// DefaultStderrLimit is the stderr capture size used when Config.StderrLimit is
// zero. It is generous enough to hold a stack trace and small enough that a
// server looping on its error path costs nothing.
const DefaultStderrLimit = 8 << 10

// timings are the bounds on every wait this transport performs. They are a
// value rather than constants so that this package's own tests can drive the
// shutdown escalation without spending its real grace periods; nothing outside
// the package can set them, and Connect always starts from defaultTimings.
type timings struct {
	// exit is how long the child gets to exit on its own after its stdin is
	// closed — the MCP shutdown sequence — before it is signalled.
	exit time.Duration
	// terminate is how long the child's group gets after SIGTERM before it is
	// killed.
	terminate time.Duration
	// reap bounds the wait for the kill to land. A process that survives
	// SIGKILL is stuck in the kernel and no amount of waiting fixes it; the
	// bound is what stops Close hanging on it.
	reap time.Duration
	// stderrDrain bounds the wait for the stderr copier to reach EOF when a
	// diagnostic needs the tail. The child is dead by then, so EOF is imminent
	// — unless a descendant still holds the pipe, which is exactly the case
	// that must not block an error from being reported.
	stderrDrain time.Duration
	// exitObserve bounds the wait, on a failure path, for the reaper to publish
	// an exit that has already happened. EOF on the child's stdout means the
	// child closed it, i.e. it is dead or dying; this is the window for Wait to
	// return, so the failure can name the exit status.
	exitObserve time.Duration
}

func defaultTimings() timings {
	return timings{
		exit:        2 * time.Second,
		terminate:   2 * time.Second,
		reap:        2 * time.Second,
		stderrDrain: 500 * time.Millisecond,
		exitObserve: 250 * time.Millisecond,
	}
}

// stderrTailBytes is how much of the captured stderr a diagnostic message
// carries. The whole capture is available for a host that wants it; an error
// string is not the place for kilobytes of a server's chatter.
const stderrTailBytes = 400

// Var is one explicit environment entry for the child.
type Var struct {
	// Name is the variable name. It must be non-empty and contain no '=' or
	// NUL byte.
	Name string
	// Value is the value, which may be a secret: it is never logged, never put
	// in an error, and never part of RedactedOrigin.
	Value string
}

// EnvAllowlist is the child's environment, built from nothing.
//
// This is an allowlist and not a filter, and the difference is the whole point:
// a variable that is not named here is absent from the child, so a credential
// this process holds for some other purpose cannot reach a server by default.
// The zero value gives the child an empty environment.
type EnvAllowlist struct {
	// Vars are explicit name/value pairs. A name here wins over the same name
	// in PassThrough.
	Vars []Var
	// PassThrough names variables to copy from this process's environment, if
	// they are set. An unset name is simply absent from the child; it is not an
	// error, because "PATH if there is one" is a legitimate thing to ask for.
	PassThrough []string
}

// Config configures a stdio transport.
type Config struct {
	// Command is the MCP server executable: either an absolute path, or a bare
	// name resolved on PATH. It is never a shell string — this transport does
	// not have a shell to give it to — and a relative path is rejected, because
	// what it names depends on a working directory the caller did not state.
	//
	// It is resolved once, by New, so a command that does not exist is a
	// configuration error at construction rather than a connection failure
	// later.
	Command string
	// Args are the command's arguments, passed to execve as separate strings.
	// An argument may carry a secret, so args never appear in RedactedOrigin or
	// in any error this package produces.
	Args []string
	// Dir is the child's working directory. It must be absolute if set; empty
	// means this process's working directory.
	Dir string
	// Env is the child's environment allowlist. The zero value gives the child
	// no environment at all.
	Env EnvAllowlist
	// Launcher creates the process. Nil means a plain argv exec, with the child
	// leading its own process group. A host that confines its servers supplies
	// its own; see ProcessLauncher.
	Launcher ProcessLauncher
	// StderrLimit bounds the child's captured stderr, in bytes. Zero selects
	// DefaultStderrLimit; a negative value is a configuration error. The
	// capture keeps the most recent bytes and drops the rest.
	StderrLimit int
}

// factory is the TransportFactory. It is immutable once New has returned it,
// which is what makes Kind, RedactedOrigin and Connect safe to call
// concurrently.
type factory struct {
	// path is the resolved absolute executable.
	path        string
	args        []string
	dir         string
	env         EnvAllowlist
	launcher    ProcessLauncher
	stderrLimit int
	// times bounds every wait a connection from this factory performs.
	times timings
	// origin is the precomputed RedactedOrigin: it never depends on anything
	// that could change, so it cannot drift from what was validated.
	origin string
}

// New validates cfg and returns a stdio TransportFactory.
//
// It fails closed: every violation is an *client.Error of class
// FailureInvalidConfig. Config errors name the offending field, and the command
// — which is not a secret — but never an argument's or a variable's value,
// which may be.
//
// No process is started until Connect. The COMMAND, though, is resolved here:
// New calls exec.LookPath, which reads $PATH and yields the absolute path
// stored on the factory (see Config.Command). That is deliberate — a command
// that does not exist is a configuration error, and it is worth learning at
// construction rather than on a connection attempt later.
//
// Config.Env is the opposite and stays so: New only validates the allowlist's
// shape, and the variables' VALUES are read from the environment at Connect, so
// a factory built early does not capture an environment that has since changed.
func New(cfg Config) (client.TransportFactory, error) {
	fail := func(msg string) error {
		return client.NewError(client.FailureInvalidConfig, "", opNew, msg, nil)
	}

	path, err := resolveCommand(cfg.Command)
	if err != nil {
		return nil, fail(err.Error())
	}
	for i, arg := range cfg.Args {
		if strings.ContainsRune(arg, 0) {
			// The index, not the value: an argument may be a credential.
			return nil, fail(fmt.Sprintf("Args[%d] contains a NUL byte", i))
		}
	}
	dir := cfg.Dir
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return nil, fail(fmt.Sprintf("Dir %q is not absolute: a working directory must be stated, not inferred", dir))
		}
		dir = filepath.Clean(dir)
	}
	if err := cfg.Env.validate(); err != nil {
		return nil, fail(err.Error())
	}
	if cfg.StderrLimit < 0 {
		return nil, fail(fmt.Sprintf("StderrLimit is %d: it must be positive, or zero for the default", cfg.StderrLimit))
	}
	stderrLimit := cfg.StderrLimit
	if stderrLimit == 0 {
		stderrLimit = DefaultStderrLimit
	}
	launcher := cfg.Launcher
	if launcher == nil {
		launcher = osLauncher{}
	}

	return &factory{
		path: path,
		// Detached copies: a Definition is immutable after validation, and a
		// caller that keeps its slices must not be able to rewrite what was
		// validated.
		args:        slices.Clone(cfg.Args),
		dir:         dir,
		env:         cfg.Env.clone(),
		launcher:    launcher,
		stderrLimit: stderrLimit,
		times:       defaultTimings(),
		origin:      fmt.Sprintf("%s (%d args)", filepath.Base(path), len(cfg.Args)),
	}, nil
}

// resolveCommand turns a configured command into an absolute executable path.
//
// A bare name is resolved on PATH. A path must be absolute: "./server" and
// "bin/server" name different files depending on a working directory the caller
// has not stated (and which is not Config.Dir — os/exec resolves a relative
// program against this process's directory, not the child's), so they are
// rejected rather than guessed at.
//
// The field name is a format argument rather than a literal prefix throughout:
// these messages name a Config field, which is capitalized, and an error string
// that starts with a capital letter is otherwise a lint (ST1005).
func resolveCommand(command string) (string, error) {
	const field = "Command"
	switch {
	case command == "":
		return "", fmt.Errorf("%s is empty", field)
	case strings.ContainsRune(command, 0):
		return "", fmt.Errorf("%s contains a NUL byte", field)
	}
	if strings.ContainsRune(command, filepath.Separator) {
		if !filepath.IsAbs(command) {
			return "", fmt.Errorf("%s %q is a relative path: use an absolute path, or a bare name to resolve on PATH", field, command)
		}
		command = filepath.Clean(command)
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("%s %q is not an executable: %v", field, command, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("%s %q could not be resolved to an absolute path: %v", field, command, err)
	}
	return abs, nil
}

// clone returns a deep copy of the allowlist.
func (a EnvAllowlist) clone() EnvAllowlist {
	return EnvAllowlist{
		Vars:        slices.Clone(a.Vars),
		PassThrough: slices.Clone(a.PassThrough),
	}
}

// validate reports the first malformed entry. Messages name variables, never
// values: a name is configuration, a value may be a secret.
func (a EnvAllowlist) validate() error {
	seen := make(map[string]struct{}, len(a.Vars))
	for i, v := range a.Vars {
		if err := validEnvName(v.Name); err != nil {
			return fmt.Errorf("Env.Vars[%d]: %w", i, err)
		}
		if strings.ContainsRune(v.Value, 0) {
			return fmt.Errorf("Env.Vars[%d] (%s): value contains a NUL byte", i, v.Name)
		}
		if _, dup := seen[v.Name]; dup {
			return fmt.Errorf("Env.Vars[%d]: duplicate name %s", i, v.Name)
		}
		seen[v.Name] = struct{}{}
	}
	passed := make(map[string]struct{}, len(a.PassThrough))
	for i, name := range a.PassThrough {
		if err := validEnvName(name); err != nil {
			return fmt.Errorf("Env.PassThrough[%d]: %w", i, err)
		}
		if _, dup := passed[name]; dup {
			return fmt.Errorf("Env.PassThrough[%d]: duplicate name %s", i, name)
		}
		passed[name] = struct{}{}
	}
	return nil
}

// validEnvName rejects names execve could not carry, or that could smuggle a
// second entry into the environment.
func validEnvName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case strings.ContainsRune(name, '='):
		return fmt.Errorf("name %q contains '='", name)
	case strings.ContainsRune(name, 0):
		return errors.New("name contains a NUL byte")
	}
	return nil
}

// build assembles the child's environment from nothing, using lookup for the
// pass-through names. The result is always non-nil — an empty environment is a
// choice, and os/exec reads nil as "inherit everything".
//
// Names not on the allowlist are absent, whatever this process's environment
// holds. Explicit Vars win over a pass-through of the same name; the result
// never contains a duplicate name, so nothing depends on which entry a given
// libc happens to pick.
func (a EnvAllowlist) build(lookup func(string) (string, bool)) []string {
	explicit := make(map[string]struct{}, len(a.Vars))
	for _, v := range a.Vars {
		explicit[v.Name] = struct{}{}
	}
	env := make([]string, 0, len(a.Vars)+len(a.PassThrough))
	for _, name := range a.PassThrough {
		if _, overridden := explicit[name]; overridden {
			continue
		}
		if value, ok := lookup(name); ok {
			env = append(env, name+"="+value)
		}
	}
	for _, v := range a.Vars {
		env = append(env, v.Name+"="+v.Value)
	}
	return env
}

// Kind implements client.TransportFactory.
func (f *factory) Kind() string { return kind }

// RedactedOrigin implements client.TransportFactory. It is the command's base
// name and how many arguments it is given — enough to tell two bindings apart
// in a log, and nothing that could be a credential: an argument's text, a
// variable's value and the full path are all withheld.
func (f *factory) RedactedOrigin() string { return f.origin }

// Connect starts the server and returns a connection ready to be initialized.
// It does not perform the handshake: that is Initialize's job, so that a
// process that dies before it says anything is reported as what it is.
//
// ctx bounds starting. It does not bound the connection: the child outlives
// ctx, and Close is what ends it. A ctx that is already done, or that is
// cancelled during the start, leaves nothing running.
func (f *factory) Connect(ctx context.Context, cfg protocol.ConnectConfig) (protocol.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, connectErr(err, "the stdio transport was not started")
	}

	pipes, err := newStdio()
	if err != nil {
		return nil, client.NewError(client.FailureTransportClosed, "", opConnect,
			"could not create the server's pipes", err)
	}

	proc, err := f.launcher.Start(ctx, ProcessSpec{
		Path:   f.path,
		Args:   f.args,
		Dir:    f.dir,
		Env:    f.env.build(os.LookupEnv),
		Stdin:  pipes.childStdin,
		Stdout: pipes.childStdout,
		Stderr: pipes.childStderr,
	})
	// The child's ends belong to the child now — or to nobody, if the start
	// failed. Either way this process must not hold them: an unclosed write end
	// of the child's stdout means the read end never sees EOF, so a dead server
	// would look like a silent one forever.
	pipes.closeChildEnds()
	if err != nil {
		pipes.closeParentEnds()
		return nil, client.NewError(client.FailureTransportClosed, "", opConnect,
			"the stdio server could not be started", err)
	}
	if isNilProcess(proc) {
		// A launcher that reports success with no process. It is foreign code
		// — an application's confinement wrapper — and this is checked because
		// the alternative is a nil dereference on the reaper's goroutine, which
		// costs the host its whole process rather than this one connection.
		pipes.closeParentEnds()
		return nil, client.NewError(client.FailureTransportClosed, "", opConnect,
			"the launcher returned no process and no error", nil)
	}

	c := &conn{
		proc:        proc,
		times:       f.times,
		stderr:      newRing(f.stderrLimit),
		stdin:       pipes.parentStdin,
		stdout:      pipes.parentStdout,
		stderrRead:  pipes.parentStderr,
		exited:      make(chan struct{}),
		stderrDrain: make(chan struct{}),
	}
	// stdout is reserved for MCP framing: it is handed to the SDK and read by
	// nothing else, here or anywhere.
	c.session = protocol.NewSession(&mcp.IOTransport{
		Reader: pipes.parentStdout,
		Writer: pipes.parentStdin,
	}, cfg)

	go c.captureStderr()
	go c.reap()

	if err := ctx.Err(); err != nil {
		// Cancelled during the start. The process exists, so it must not be
		// left behind just because nothing will ever use it.
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, connectErr(err, "the stdio transport was cancelled while starting")
	}
	return c, nil
}

// isNilProcess reports whether a launcher handed back no process. A nil check
// alone is not enough: a launcher returning a typed nil pointer produces a
// non-nil interface holding nil, which panics on the first method call.
func isNilProcess(p Process) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

// connectErr classifies a context failure during the start.
func connectErr(err error, msg string) error {
	class := client.FailureCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		class = client.FailureStartupTimeout
	}
	return client.NewError(class, "", opConnect, msg, err)
}

// conn is one connection to one child process: the MCP session on top, and the
// process, pipes and stderr capture underneath.
type conn struct {
	proc    Process
	session *protocol.Session
	stderr  *ring
	times   timings

	// stdin and stdout are this process's ends of the child's stdin and stdout.
	// The SDK owns them once the session exists and closes both when the
	// session closes; they are held here only so a connection that never got a
	// session can still release them.
	stdin  *os.File
	stdout *os.File
	// stderrRead is this process's end of the child's stderr, drained by
	// captureStderr.
	stderrRead *os.File

	// exited is closed once the child has been reaped and status/waitErr are
	// final.
	exited  chan struct{}
	status  ExitStatus
	waitErr error

	// stderrDrain is closed once the stderr copier has seen EOF, so a
	// diagnostic can know the capture is complete rather than in flight.
	stderrDrain chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// captureStderr drains the child's stderr into the bounded ring. It reads until
// EOF, which arrives when every writer — the child and anything it spawned that
// inherited the pipe — has let go.
func (c *conn) captureStderr() {
	defer close(c.stderrDrain)
	// The ring is fixed-size, so this copy is bounded in memory no matter how
	// much the child writes. (internal/limits' BoundedReader is the wrong tool
	// here: it fails a stream that exceeds its bound, and a chatty server is
	// not a failure — it is a server whose oldest chatter stops mattering.)
	_, _ = io.Copy(c.stderr, c.stderrRead)
}

// reap waits for the child and publishes how it ended. It is the only caller of
// Process.Wait, so nothing can double-reap and no zombie survives the child.
func (c *conn) reap() {
	status, err := c.proc.Wait()
	c.status = status
	c.waitErr = err
	// The write above happens-before every read, which happens after this
	// channel is closed.
	close(c.exited)
}

// exitStatus reports the child's exit, if it has been reaped.
func (c *conn) exitStatus() (ExitStatus, bool) {
	select {
	case <-c.exited:
		return c.status, c.waitErr == nil
	default:
		return ExitStatus{}, false
	}
}

// Initialize performs the MCP handshake.
func (c *conn) Initialize(ctx context.Context) (protocol.InitializeResult, error) {
	res, err := c.session.Initialize(ctx)
	if err != nil {
		return protocol.InitializeResult{}, c.classify(ctx, opInitialize, err)
	}
	return res, nil
}

// The catalog list methods. Each is a straight delegation to the session, with
// the transport's own classification applied to a failure: only this layer can
// tell "the server spoke badly" from "the child process is gone", and a list that
// fails during discovery must be classified the same way a handshake would be.
//
// The four are generated through listVia rather than written out because they
// differ only in the method they call — and a hand-written fourth copy is where
// the classification would eventually go missing.

// ListTools fetches one page of tools.
func (c *conn) ListTools(ctx context.Context, cursor string) (protocol.ToolPage, error) {
	return listVia(ctx, c, opListTools, cursor, c.session.ListTools)
}

// ListPrompts fetches one page of prompts.
func (c *conn) ListPrompts(ctx context.Context, cursor string) (protocol.PromptPage, error) {
	return listVia(ctx, c, opListPrompts, cursor, c.session.ListPrompts)
}

// ListResources fetches one page of resources.
func (c *conn) ListResources(ctx context.Context, cursor string) (protocol.ResourcePage, error) {
	return listVia(ctx, c, opListResources, cursor, c.session.ListResources)
}

// ListResourceTemplates fetches one page of resource templates.
func (c *conn) ListResourceTemplates(ctx context.Context, cursor string) (protocol.ResourceTemplatePage, error) {
	return listVia(ctx, c, opListResourceTemplates, cursor, c.session.ListResourceTemplates)
}

// listVia runs one list method and classifies its failure. The page type is the
// only thing that varies, so it is the only type parameter.
func listVia[P any](
	ctx context.Context,
	c *conn,
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
func (c *conn) CallTool(ctx context.Context, rawName string, args json.RawMessage, opts protocol.CallOptions) (protocol.ToolResult, error) {
	res, err := c.session.CallTool(ctx, rawName, args, opts)
	if err != nil {
		return protocol.ToolResult{}, c.classify(ctx, opCallTool, err)
	}
	return res, nil
}

// GetPrompt fetches a prompt's messages.
func (c *conn) GetPrompt(ctx context.Context, name string, args map[string]string) (protocol.PromptResult, error) {
	res, err := c.session.GetPrompt(ctx, name, args)
	if err != nil {
		return protocol.PromptResult{}, c.classify(ctx, opGetPrompt, err)
	}
	return res, nil
}

// ReadResource reads a resource by URI.
func (c *conn) ReadResource(ctx context.Context, uri string) (protocol.ResourceResult, error) {
	res, err := c.session.ReadResource(ctx, uri)
	if err != nil {
		return protocol.ResourceResult{}, c.classify(ctx, opReadResource, err)
	}
	return res, nil
}

// Subscribe asks the server to report changes to a resource.
func (c *conn) Subscribe(ctx context.Context, uri string) error {
	if err := c.session.Subscribe(ctx, uri); err != nil {
		return c.classify(ctx, opSubscribe, err)
	}
	return nil
}

func (c *conn) Unsubscribe(ctx context.Context, uri string) error {
	if err := c.session.Unsubscribe(ctx, uri); err != nil {
		return c.classify(ctx, opUnsubscribe, err)
	}
	return nil
}

// SetLogLevel asks the server to send logs at or above level.
func (c *conn) SetLogLevel(ctx context.Context, level string) error {
	if err := c.session.SetLogLevel(ctx, level); err != nil {
		return c.classify(ctx, opSetLogLevel, err)
	}
	return nil
}

// classify turns a session failure into a typed error, distinguishing the two
// things the SDK reports identically: a server that spoke badly, and a server
// that is not there any more.
//
// The caller's own context is read first — a cancelled caller is a
// cancellation, whatever the read that noticed it returned — and the child's
// exit second, because "the connection closed" is a symptom and "the process
// exited 2 saying this" is the cause.
func (c *conn) classify(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return client.NewError(client.FailureCancelled, "", op, "the stdio transport was cancelled", err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return client.NewError(deadlineClass(op), "", op,
			"the stdio server did not answer in time", err)
	}
	// A failure with a live child is a protocol failure; a failure with a dead
	// one is the death. The child may have died a microsecond ago and not been
	// reaped yet, so the reaper gets a bounded moment to catch up rather than
	// this reporting the wrong cause on a race.
	select {
	case <-c.exited:
	case <-time.After(c.times.exitObserve):
	}
	if status, ok := c.exitStatus(); ok {
		return client.NewError(client.FailureTransportClosed, "", op,
			"the stdio server process exited ("+status.String()+")"+c.stderrTail(), err)
	}
	return client.NewError(client.FailureServerProtocol, "", op, "", err)
}

// stderrTail renders the end of the child's captured stderr for a diagnostic,
// or "" if it said nothing. The text is bounded twice over — by the ring's
// capacity, then by stderrTailBytes — and client.NewError bounds and normalizes
// whatever survives, so a server cannot make an error message large or
// terminal-hostile.
func (c *conn) stderrTail() string {
	// The child is gone by the time this is called, so EOF is imminent; the
	// bound is for the case where a descendant still holds the pipe.
	select {
	case <-c.stderrDrain:
	case <-time.After(c.times.stderrDrain):
	}
	tail := c.stderr.Tail(stderrTailBytes)
	if len(tail) == 0 {
		return ""
	}
	text, _ := limits.TruncateText(strings.TrimSpace(string(tail)), stderrTailBytes)
	if text == "" {
		return ""
	}
	if dropped := c.stderr.Dropped(); dropped > 0 {
		return fmt.Sprintf(": stderr tail (%d earlier bytes dropped): %s", dropped, text)
	}
	return ": stderr tail: " + text
}

// Close shuts the connection and the child down, in the only order that does
// not lose data:
//
//  1. drain the MCP session — no new requests, in-flight ones finish — and let
//     the SDK close the stream, which closes the child's stdin and is the MCP
//     stdio shutdown signal;
//  2. give the child a bounded moment to exit on its own;
//  3. SIGTERM its process group, then SIGKILL it;
//  4. reap.
//
// Closing stdin first, before step 1, is what an impatient shutdown does, and
// it is why this one is not: an SDK-based server reads EOF as "stop", drops the
// replies it has not written, and exits on an error.
//
// It is idempotent. A child that exited on its own — a crash — is not a close
// failure: there is nothing left to release, which is what Close is for. Only
// an unreapable process is an error.
func (c *conn) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { c.closeErr = c.shutdown(ctx) })
	return c.closeErr
}

func (c *conn) shutdown(ctx context.Context) error {
	// The session's close is graceful and closes the stream behind it. Its
	// error is not this method's verdict: for a child that already crashed it
	// reports the crash, which Close does not treat as a failure to close. What
	// matters is whether the process is gone, which is settled below.
	sessErr := c.session.Close(ctx)

	// Belt and braces: a connection that was never initialized has no SDK
	// session to have closed the pipes, and the child's stdin must be closed
	// for it to stop. Closing an already-closed file is a no-op worth having
	// over a leaked descriptor.
	c.closeParentPipes()

	err := c.terminate(ctx)

	// The copier ends at EOF, which the reaped child guarantees; closing the
	// read end is what unblocks it if a descendant is still holding the write
	// end open.
	select {
	case <-c.stderrDrain:
	case <-time.After(c.times.stderrDrain):
	}
	if cerr := c.stderrRead.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
		err = errors.Join(err, cerr)
	}

	if err != nil {
		return client.NewError(client.FailureTransportClosed, "", opClose,
			"the stdio server process could not be shut down", errors.Join(err, sessErr))
	}
	return nil
}

// terminate walks the escalation and returns only when the child is reaped, or
// when it has proved it cannot be.
func (c *conn) terminate(ctx context.Context) error {
	if c.awaitExit(ctx, c.times.exit) {
		return c.waitErr
	}
	if err := c.proc.Terminate(); err != nil {
		// Signalling failed for a reason other than "already gone": do not wait
		// on a request that was never delivered — escalate.
		return c.kill(ctx, err)
	}
	if c.awaitExit(ctx, c.times.terminate) {
		return c.waitErr
	}
	return c.kill(ctx, nil)
}

// kill is the last resort: destroy the group and wait for the reap.
//
// prior is whatever went wrong earlier in the escalation — typically a SIGTERM
// that could not be delivered. It is reported only if the kill does not settle
// the matter: a step that was recovered from is not a failure, and Close's
// verdict is about whether the process is gone, not about how much persuasion
// it took.
func (c *conn) kill(ctx context.Context, prior error) error {
	if err := c.proc.Kill(); err != nil {
		return errors.Join(prior, fmt.Errorf("killing the server process: %w", err))
	}
	if c.awaitExit(ctx, c.times.reap) {
		return c.waitErr
	}
	return errors.Join(prior, fmt.Errorf("the server process (pid %d) survived being killed", c.proc.Pid()))
}

// awaitExit reports whether the child was reaped within d, or before ctx ended.
// A done ctx does not skip the wait entirely — it collapses it to a poll — so
// that a caller whose deadline has already passed still reaps a child that has
// already exited, instead of signalling a corpse.
func (c *conn) awaitExit(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.exited:
		return true
	case <-ctx.Done():
		select {
		case <-c.exited:
			return true
		default:
			return false
		}
	case <-timer.C:
		return false
	}
}

// closeParentPipes releases this process's ends of the child's stdin and
// stdout. Errors are not reported: os.ErrClosed is the expected outcome — the
// SDK closes both when the session closes — and on any other, nothing here can
// act and the child's fate is what Close reports. This is a safety net for the
// connection that never got a session, not a step in the sequence.
func (c *conn) closeParentPipes() {
	closeAll(c.stdin, c.stdout)
}

// stdioPipes is the six file descriptors a child needs, split into the ends
// each side keeps.
type stdioPipes struct {
	parentStdin  *os.File // write: the child's stdin
	childStdin   *os.File // read
	parentStdout *os.File // read: the child's stdout
	childStdout  *os.File // write
	parentStderr *os.File // read: the child's stderr
	childStderr  *os.File // write
}

// newStdio creates the three pipes. Explicit pipes, rather than os/exec's
// StdoutPipe, because the ends must outlive the reap: os/exec closes the pipes
// it made as part of Wait, which races every read still in flight.
func newStdio() (*stdioPipes, error) {
	p := &stdioPipes{}
	var err error
	if p.childStdin, p.parentStdin, err = os.Pipe(); err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	if p.parentStdout, p.childStdout, err = os.Pipe(); err != nil {
		p.closeAll()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if p.parentStderr, p.childStderr, err = os.Pipe(); err != nil {
		p.closeAll()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	return p, nil
}

// closeChildEnds drops this process's copies of the child's ends, which the
// child now holds. Until they are dropped, this process is itself a writer on
// the child's stdout and stderr, so no reader here would ever see EOF.
func (p *stdioPipes) closeChildEnds() {
	closeAll(p.childStdin, p.childStdout, p.childStderr)
}

// closeParentEnds drops the ends this process kept. It is the unwind for a
// start that never happened.
func (p *stdioPipes) closeParentEnds() {
	closeAll(p.parentStdin, p.parentStdout, p.parentStderr)
}

func (p *stdioPipes) closeAll() {
	p.closeChildEnds()
	p.closeParentEnds()
}

// closeAll closes every non-nil file, ignoring errors: these are pipe ends
// being abandoned on a path that already failed, and a close error on one is
// not something any caller can act on.
func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
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
	case opConnect, opInitialize:
		return client.FailureStartupTimeout
	default:
		return client.FailureDeadline
	}
}
