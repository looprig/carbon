// Command carbon is the Carbon TUI entry point and composition root. It parses the CLI
// invocation (--list / --resume / --data-dir), opens the session-store factory (one on-disk
// fsstore-backed session store shared by every session), and either prints the session list
// (--list) or hands the shared TUI runtime (runtime.Run) a thunk that opens/resumes the persisted
// Carbon session. It is wiring only: all runtime behavior (logging, signal teardown, the TUI)
// lives in tui, and all Session/persistence behavior lives in the internal app package.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/sandbox"
	"github.com/looprig/tui"
	"github.com/looprig/tui/runtime"
)

// bannerName is the Carbon's user-facing banner name shown in the TUI session-ready
// notice (passed through runtime.Banner).
const bannerName = "Carbon"

const dataDirUsage = "session store root (default ~/.looprig/carbon/store)"

// Process exit codes main returns via os.Exit. exitOK / exitRuntime mirror the runtime's
// codes; exitUsage is the boundary-failure code for a malformed invocation or a
// persistence/list failure (distinct from a TUI run error, which runtime.Run owns).
const (
	exitOK     = 0
	exitUsage  = 2
	exitFailed = 1
)

// cliFlags is the parsed CLI invocation: whether to list sessions and exit (--list), which
// session to resume (--resume <uuid>; zero = new session), the session store root
// (--data-dir; empty = the ~/.looprig/carbon/store default), the selected access profile
// (--access-profile readonly|trusted|unconfined; default trusted), and the explicit
// unconfined acknowledgement (--acknowledge-unconfined; required to select unconfined). There
// is no positional agent name because Carbon is one fixed Rig.
type cliFlags struct {
	list             bool
	credentialList   bool
	credentialLogin  string
	credentialLogout string
	resume           uuid.UUID
	dataDir          string
	// accessProfile is the session-fixed product access profile, validated at this
	// boundary against exactly the three known names before the Rig is constructed.
	accessProfile carbon.AccessProfile
	// acknowledgeUnconfined is the explicit opt-in required to select the unconfined
	// profile (direct host execution). Selecting unconfined without it fails closed.
	acknowledgeUnconfined bool
}

// FlagParseError reports a malformed CLI invocation (an unknown flag, a non-UUID --resume
// value, the mutually-exclusive --list + --resume combination, or an unexpected positional
// arg). It is a typed boundary error: untrusted CLI input is validated here, before any
// wiring runs, and is errors.As-recoverable.
type FlagParseError struct {
	Reason string
	Cause  error
}

func (e *FlagParseError) Error() string {
	if e.Cause != nil {
		return "carbon: " + e.Reason + ": " + e.Cause.Error()
	}
	return "carbon: " + e.Reason
}
func (e *FlagParseError) Unwrap() error { return e.Cause }

// parseFlags parses args (os.Args[1:]) into a cliFlags, validating every value at this
// boundary: --resume must be a canonical UUID (parsed via uuid.UnmarshalText, fail-closed),
// --list and --resume are mutually exclusive (a list-and-resume request is ambiguous), and
// no positional args are accepted because Carbon is one fixed Rig. It
// uses an isolated FlagSet (ContinueOnError, discarded output) so a bad flag returns a
// typed error rather than calling os.Exit, keeping main the single exit point and making
// the parser unit-testable.
func parseFlags(args []string) (cliFlags, error) {
	args, commandErr := normalizeCredentialCommandArgs(args)
	if commandErr != "" {
		return cliFlags{}, &FlagParseError{Reason: commandErr}
	}
	fs := flag.NewFlagSet("carbon", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		list                  = fs.Bool("list", false, "list resumable sessions and exit")
		credentialList        = fs.Bool("credentials-list", false, "list configured credentials and exit")
		credentialListAlias   = fs.Bool("list-credentials", false, "list configured credentials and exit")
		credentialListAlias2  = fs.Bool("credential-list", false, "list configured credentials and exit")
		credentialLogin       = fs.String("login", "", "explicitly start credential login for a provider")
		credentialLoginAlias  = fs.String("credential-login", "", "explicitly start credential login for a provider")
		credentialLogout      = fs.String("logout", "", "explicitly log out credential://provider/name")
		credentialLogoutAlias = fs.String("credential-logout", "", "explicitly log out credential://provider/name")
		resume                = fs.String("resume", "", "resume the session with this id")
		dataDir               = fs.String("data-dir", "", dataDirUsage)
		accessProfile         = fs.String("access-profile", string(carbon.DefaultAccessProfile), "session access profile: readonly|trusted|unconfined")
		ackUnconfined         = fs.Bool("acknowledge-unconfined", false, "acknowledge that --access-profile unconfined runs commands directly on the host with no OS confinement")
	)
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, &FlagParseError{Reason: "invalid flags", Cause: err}
	}

	// Carbon takes no positional args: reject any so a typo'd flag (e.g. a bare "list"
	// instead of "-list") fails loud at the boundary rather than being silently ignored.
	if fs.NArg() > 0 {
		return cliFlags{}, &FlagParseError{Reason: "unexpected argument " + strconv.Quote(fs.Arg(0))}
	}

	// Validate the access-profile name at this boundary (untrusted CLI input): only the
	// three known names are accepted, so an unknown value fails closed rather than
	// silently defaulting to a surprising authority. The name is validated before the Rig
	// is constructed.
	profile, ok := carbon.ParseAccessProfile(strings.ToLower(strings.TrimSpace(*accessProfile)))
	if !ok {
		return cliFlags{}, &FlagParseError{Reason: "invalid --access-profile " + strconv.Quote(*accessProfile) + " (want readonly|trusted|unconfined)"}
	}

	// Unconfined requires an explicit, separate acknowledgement: selecting direct host
	// execution by accident must be impossible.
	if profile == carbon.AccessUnconfined && !*ackUnconfined {
		return cliFlags{}, &FlagParseError{Reason: "--access-profile unconfined requires --acknowledge-unconfined (it runs commands directly on the host with no OS confinement)"}
	}

	login := strings.TrimSpace(*credentialLogin)
	if login == "" {
		login = strings.TrimSpace(*credentialLoginAlias)
	}
	logout := strings.TrimSpace(*credentialLogout)
	if logout == "" {
		logout = strings.TrimSpace(*credentialLogoutAlias)
	}
	out := cliFlags{
		list: *list, credentialList: *credentialList || *credentialListAlias || *credentialListAlias2,
		credentialLogin: login, credentialLogout: logout,
		dataDir: strings.TrimSpace(*dataDir), accessProfile: profile, acknowledgeUnconfined: *ackUnconfined,
	}

	// Detect whether --resume was explicitly given (vs left at its empty default): an
	// explicit --resume with an empty/whitespace value is a malformed invocation, rejected
	// at the boundary rather than silently treated as "no resume".
	var resumeGiven bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "resume" {
			resumeGiven = true
		}
	})
	if resumeGiven {
		v := strings.TrimSpace(*resume)
		if v == "" {
			return cliFlags{}, &FlagParseError{Reason: "--resume requires a session id"}
		}
		var id uuid.UUID
		if err := id.UnmarshalText([]byte(v)); err != nil {
			return cliFlags{}, &FlagParseError{Reason: "invalid --resume session id", Cause: err}
		}
		out.resume = id
	}

	if out.list && !out.resume.IsZero() {
		return cliFlags{}, &FlagParseError{Reason: "--list and --resume are mutually exclusive"}
	}
	var credentialLoginGiven, credentialLogoutGiven, credentialListGiven bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "login", "credential-login":
			credentialLoginGiven = true
		case "logout", "credential-logout":
			credentialLogoutGiven = true
		case "credentials-list", "list-credentials", "credential-list":
			credentialListGiven = true
		}
	})
	if credentialLoginGiven && out.credentialLogin == "" {
		return cliFlags{}, &FlagParseError{Reason: "--login requires a provider"}
	}
	if credentialLogoutGiven && out.credentialLogout == "" {
		return cliFlags{}, &FlagParseError{Reason: "--logout requires a credential reference"}
	}
	if (credentialListGiven || out.credentialList || out.credentialLogin != "" || out.credentialLogout != "") && !out.resume.IsZero() {
		return cliFlags{}, &FlagParseError{Reason: "credential commands cannot be combined with --resume"}
	}
	commands := 0
	if out.list {
		commands++
	}
	if out.credentialList {
		commands++
	}
	if out.credentialLogin != "" {
		commands++
	}
	if out.credentialLogout != "" {
		commands++
	}
	if commands > 1 {
		return cliFlags{}, &FlagParseError{Reason: "credential and session commands are mutually exclusive"}
	}
	return out, nil
}

// normalizeCredentialCommandArgs accepts explicit subcommand spellings in
// addition to the flag spelling. It is intentionally narrow: only
// `credentials|credential list`, `login <provider>`, and `logout <reference>`
// are recognized, so ordinary positional typos remain rejected below.
func normalizeCredentialCommandArgs(args []string) ([]string, string) {
	if len(args) == 0 {
		return args, ""
	}
	if args[0] == "login" || args[0] == "logout" {
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return nil, "credential " + args[0] + " requires an argument"
		}
		return append([]string{"--" + args[0], args[1]}, args[2:]...), ""
	}
	if args[0] != "credentials" && args[0] != "credential" {
		return args, ""
	}
	if len(args) < 2 {
		return nil, "credential command requires list, login, or logout"
	}
	switch args[1] {
	case "list":
		return append([]string{"--credentials-list"}, args[2:]...), ""
	case "login":
		if len(args) < 3 || strings.TrimSpace(args[2]) == "" {
			return nil, "credential login requires a provider"
		}
		return append([]string{"--login", args[2]}, args[3:]...), ""
	case "logout":
		if len(args) < 3 || strings.TrimSpace(args[2]) == "" {
			return nil, "credential logout requires a credential reference"
		}
		return append([]string{"--logout", args[2]}, args[3:]...), ""
	default:
		return nil, "unknown credential command " + strconv.Quote(args[1])
	}
}

// listSessions prints the session list (id, status, last-active, title) to w, from the store's
// listing catalog (most-recently-active first). It is the --list path: it reads the listing
// index only — no session lease, no replay — so it is cheap and cannot contend a running
// session. The catalog returns a single error (not per-entry), and an empty store prints a
// friendly note rather than nothing.
func listSessions(ctx context.Context, factory *carbon.SessionStoreFactory, w io.Writer) error {
	metas, err := factory.List(ctx)
	if err != nil {
		return err
	}
	return printSessions(w, metas)
}

// printSessions renders the session rows (id, status, last-active, title) to w in the order
// given (the catalog's own most-recently-active-first ordering — the CLI does not re-sort). An
// empty list prints a friendly note; an untitled session shows "(untitled)". It is pure
// formatting, unit-testable without a store.
func printSessions(w io.Writer, metas []sessionstore.SessionMeta) error {
	if len(metas) == 0 {
		fmt.Fprintln(w, "no sessions yet")
		return nil
	}
	for _, m := range metas {
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(w, "%s  %-7s  %s  %s\n",
			m.SessionID, m.Status, m.LastActiveAt.Format(time.RFC3339), title)
	}
	return nil
}

func printCredentials(w io.Writer, summaries []carbon.CredentialSummary) error {
	if len(summaries) == 0 {
		fmt.Fprintln(w, "no credentials yet")
		return nil
	}
	for _, summary := range summaries {
		fmt.Fprintf(w, "%s  provider=%s  transport=%s  scheme=%s  usage=%s  status=%s\n",
			summary.Reference, summary.Provider, summary.Transport, summary.Scheme, summary.Usage, summary.Status)
	}
	return nil
}

func printCredentialLogout(w io.Writer, outcome carbon.CredentialLogoutOutcome) error {
	remote := "not-attempted"
	if outcome.RemoteRevocationAttempted {
		if outcome.RemoteRevoked {
			remote = "revoked"
		} else if outcome.RemoteRevocationError {
			remote = "failed"
		} else {
			remote = "not-confirmed"
		}
	}
	fmt.Fprintf(w, "%s  local_catalog=%s  local_state=%s  remote_revocation=%s\n",
		outcome.Reference, boolStatus(outcome.LocalCatalogDeleted), boolStatus(outcome.LocalStateDeleted), remote)
	return nil
}

func boolStatus(value bool) string {
	if value {
		return "deleted"
	}
	return "not-deleted"
}

// sessionOpen is the SessionStoreFactory.Open-shaped process composition seam. Production
// binds it directly to the shared factory; tests can observe selector decisions without
// opening an on-disk store.
type sessionOpen func(context.Context, carbon.SessionSelector, carbon.Config) (tui.Agent, error)

// openThunk builds the tui.OpenAgent the runtime drives. It returns a closure that opens a
// persisted Carbon session: the FIRST call honors resume (a non-zero id restores that
// session); every later call (a /clear reopen) starts a fresh NEW session, so /clear never
// re-restores the same id. The CLI serializes lifecycle handoff by closing the live session
// before invoking this opener for /clear. cfg applies to every open, including a /clear
// reopen. Every
// open (or, on the first call, resume) addresses its session by name in the SHARED store, so a
// /clear reopen's new session is independent of the one it replaces. The returned thunk yields
// a tui.Agent (the persisted session adapter exposes current active selection and independent
// focused-loop routing for the CLI).
func openThunk(openSession sessionOpen, resume uuid.UUID, cfg carbon.Config) tui.OpenAgent {
	var opened bool
	return func(c context.Context) (tui.Agent, error) {
		sel := carbon.SessionSelector{}
		if !opened {
			sel.Resume = resume // only the first open resumes; /clear reopens start fresh
		}
		opened = true
		return openSession(c, sel, cfg)
	}
}

// cliRunner is the runtime.Run-shaped runtime seam used to prove process ownership order.
type cliRunner func(context.Context, tui.OpenAgent, runtime.Banner) int

// runCLIWithStore runs the CLI while the shared session store is live. runtime.Run closes its
// current session before returning; the store close therefore always happens after session
// shutdown, including runtime-error exits. A store teardown error maps to process failure.
func runCLIWithStore(ctx context.Context, open tui.OpenAgent, banner runtime.Banner, runCLI cliRunner, closeStore func() error) int {
	exit := runCLI(ctx, open, banner)
	if err := closeStore(); err != nil {
		return exitFailed
	}
	return exit
}

// run is the testable composition root: it parses flags, resolves the store root, opens the
// session-store factory (closed once on return), handles --list (print + exit) or builds the
// persisted openThunk and delegates to runtime.Run. It returns a process exit code and never calls
// os.Exit, so main stays the single exit point. ctx is the process root (signal-aware);
// out/errOut are the list + error sinks.
func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags, ferr := parseFlags(args)
	if ferr != nil {
		fmt.Fprintln(errOut, ferr)
		return exitUsage
	}

	// Resolve Carbon's home exactly once for this process invocation. The
	// resolved absolute value is carried in Config so credential, model, and
	// session composition never repeat ambient home discovery.
	cfg := carbon.Config{AccessProfile: flags.accessProfile}
	home, herr := carbon.LooprigHome(cfg)
	if herr != nil {
		fmt.Fprintln(errOut, "home:", herr)
		return exitFailed
	}
	cfg.HomeDir = home

	// Resolve the store root: the explicit --data-dir, or the ~/.looprig/carbon/store default
	// (relative to cfg's resolved home directory).
	dataDir := flags.dataDir
	if dataDir == "" {
		dd, derr := carbon.DefaultDataDirIn(home)
		if derr != nil {
			fmt.Fprintln(errOut, "persistence:", derr)
			return exitFailed
		}
		dataDir = dd
	}

	// Credential commands are explicit, short-lived catalog operations. They
	// do not construct a session store or start the TUI, and login is gated by
	// the provider registration policy before any browser/network path exists.
	if flags.credentialList {
		summaries, err := carbon.ListCredentials(ctx, cfg)
		if err != nil {
			fmt.Fprintln(errOut, "credentials list:", err)
			return exitFailed
		}
		if err := printCredentials(out, summaries); err != nil {
			fmt.Fprintln(errOut, "credentials list:", err)
			return exitFailed
		}
		return exitOK
	}
	if flags.credentialLogin != "" {
		if err := carbon.LoginCredential(ctx, cfg, flags.credentialLogin); err != nil {
			fmt.Fprintln(errOut, "credentials login:", err)
			return exitFailed
		}
		fmt.Fprintln(out, "credential login complete")
		return exitOK
	}
	if flags.credentialLogout != "" {
		outcome, err := carbon.LogoutCredential(ctx, cfg, flags.credentialLogout)
		if outcome.Reference != "" {
			if printErr := printCredentialLogout(out, outcome); printErr != nil {
				fmt.Fprintln(errOut, "credentials logout:", printErr)
				return exitFailed
			}
		}
		if err != nil {
			fmt.Fprintln(errOut, "credentials logout:", err)
			return exitFailed
		}
		return exitOK
	}

	// Open the session-store factory: the process-level composition root that owns the single
	// on-disk store shared by every session. A failure to open it fails loud — persistence is
	// the point. It is closed once here on return, after runtime.Run (and every session it opened)
	// finishes.
	factory, perr := carbon.NewSessionStoreFactory(dataDir)
	if perr != nil {
		fmt.Fprintln(errOut, "persistence:", perr)
		return exitFailed
	}
	// --list: print the session list and exit (no TUI). It reads only the listing catalog, so
	// it is cheap even with many sessions.
	if flags.list {
		if err := listSessions(ctx, factory, out); err != nil {
			_ = factory.Close()
			fmt.Fprintln(errOut, "list:", err)
			return exitFailed
		}
		if err := factory.Close(); err != nil {
			fmt.Fprintln(errOut, "persistence close:", err)
			return exitFailed
		}
		return exitOK
	}

	// Selecting unconfined execution surfaces an explicit warning before the session opens:
	// the profile runs commands directly on the host with the invoking user's authority and
	// no OS confinement. The acknowledgement flag was already required at the boundary.
	if flags.accessProfile == carbon.AccessUnconfined {
		fmt.Fprintln(errOut, "carbon: WARNING: --access-profile unconfined runs commands directly on the host with no OS confinement (real HOME, full filesystem and network authority).")
	}

	// The initial open honors --resume; every /clear reopen starts a FRESH persisted session.
	// The selected access profile applies to every open. runtime.Run owns logging, signal
	// teardown, the TUI, the session-identifying startup banner, and bounded Close. cfg was
	// already built above (the single point of Config construction this run resolves both the
	// store root and the session-opening modes through).
	open := openThunk(func(ctx context.Context, sel carbon.SessionSelector, cfg carbon.Config) (tui.Agent, error) {
		return factory.Open(ctx, sel, cfg)
	}, flags.resume, cfg)
	runCLI := func(ctx context.Context, open tui.OpenAgent, banner runtime.Banner) int {
		return runtime.Run(ctx, open, banner, tui.WithSessionBrowser(factory.SessionBrowser(cfg)))
	}
	return runCLIWithStore(ctx, open, runtime.Banner{Name: bannerName}, runCLI, factory.Close)
}

func main() {
	// MUST be the FIRST line of main() (SPEC §6): a no-op on darwin, but on Linux it
	// re-executes the process as the stage-2 sandbox helper before any other goroutine,
	// fd, or thread state exists. Wiring it from day one means no retrofit later.
	sandbox.Init()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
