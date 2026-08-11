package permission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// errSymlinkNotAllowed is returned by the platform openNoFollow when the
// final path component is a symlink (or, on Windows, a reparse point) the
// store refuses to traverse. store.go classifies it with errors.Is so it
// never needs a platform-specific error type.
var errSymlinkNotAllowed = errors.New("path is a symlink and may not be followed")

// errLockWouldBlock is returned by the platform lockExclusiveNonBlocking
// when the exclusive lock is already held elsewhere, mirroring flock's
// EWOULDBLOCK/EINTR-retry behavior on Unix and LOCKFILE_FAIL_IMMEDIATELY on
// Windows.
var errLockWouldBlock = errors.New("lock is held by another process")

// store.go is the single hardened workspace permission store.
//
// One Store serves one explicit permission-file path supplied by the
// consumer; the store never computes HOME-relative or otherwise implicit
// locations. An interactive workspace store re-reads the file for every
// query, so concurrent product processes observe each other's atomically
// renamed updates without watching. A read-only (headless) store loads one
// immutable snapshot at construction and never reloads.
//
// The Store satisfies the harness gate's structural RuleMatcher and
// RuleWriter contracts. Deny-before-allow ordering belongs to the gate;
// the store answers both queries independently.

// Config configures one Store.
type Config struct {
	// Path is the one explicit permission-file path. Interactive stores
	// require it; a read-only store with an empty Path uses an empty rule
	// set. The store never discovers HOME or any other implicit location.
	Path string
	// MaxFileBytes bounds the permission file size. Zero selects
	// DefaultMaxFileBytes.
	MaxFileBytes int64
	// FamilyEligible is the consumer's automatic-family eligibility catalog
	// predicate, used only to produce non-fatal diagnostics for manual
	// out-of-catalog allow families. Nil treats every allow family as out of
	// catalog. It never alters matching.
	FamilyEligible FamilyEligibility
}

// DefaultMaxFileBytes is the default permission-file size bound.
const DefaultMaxFileBytes int64 = 1 << 20

// filePerm is the exact required permission-file mode: owner read/write
// only. Any other mode is rejected.
const filePerm os.FileMode = 0o600

// dirPerm is the owner-only mode used when creating the store directory.
const dirPerm os.FileMode = 0o700

// lockSuffix names the sibling interprocess lock file.
const lockSuffix = ".lock"

// lockRetryInterval paces the non-blocking flock retry loop so lock waits
// stay cancellable through the caller's context.
const lockRetryInterval = 5 * time.Millisecond

// Store is the hardened workspace permission store.
type Store struct {
	path           string
	maxFileBytes   int64
	familyEligible FamilyEligibility
	readOnly       bool

	mu          sync.Mutex
	snapshot    []Rule // read-only stores only
	diagnostics []Diagnostic

	// Test seams. Production construction wires the real operations; tests
	// inject failures to prove rollback.
	euid       int
	renameFile func(oldPath, newPath string) error
	syncFile   func(file interface{ Sync() error }) error
}

// NewWorkspaceStore constructs the interactive read/write store for one
// workspace permission file. The file may not exist yet; an existing file
// must be secure and well-formed or construction fails. The returned
// diagnostics are the non-fatal findings of the initial load.
func NewWorkspaceStore(cfg Config) (*Store, []Diagnostic, error) {
	store, _, err := newWorkspaceStoreNoLoad(cfg)
	if err != nil {
		return nil, nil, err
	}
	rules, err := store.loadCurrent()
	if err != nil {
		return nil, nil, err
	}
	diagnostics := store.recordDiagnostics(rules)
	return store, diagnostics, nil
}

// newWorkspaceStoreNoLoad builds the interactive store without the fail-fast
// initial load. Tests use it to observe exact match-time failures.
func newWorkspaceStoreNoLoad(cfg Config) (*Store, []Diagnostic, error) {
	if cfg.Path == "" {
		return nil, nil, &FileError{Reason: FileMissing, Err: errors.New("workspace store requires one explicit permission-file path")}
	}
	if !filepath.IsAbs(cfg.Path) {
		return nil, nil, &FileError{Path: cfg.Path, Reason: FileMalformed, Err: errors.New("permission-file path must be absolute")}
	}
	return newStore(cfg, false), nil, nil
}

// NewReadOnlyStore constructs the headless store. A configured path is
// loaded once as an immutable snapshot; a missing, malformed, insecure,
// oversized, or unsupported configured file fails startup. An empty path
// yields an empty rule set. The store never watches or reloads the file and
// rejects every write.
func NewReadOnlyStore(cfg Config) (*Store, []Diagnostic, error) {
	store := newStore(cfg, true)
	if cfg.Path == "" {
		return store, nil, nil
	}
	if !filepath.IsAbs(cfg.Path) {
		return nil, nil, &FileError{Path: cfg.Path, Reason: FileMalformed, Err: errors.New("permission-file path must be absolute")}
	}
	rules, err := store.loadFile(true)
	if err != nil {
		return nil, nil, err
	}
	store.snapshot = rules
	diagnostics := store.recordDiagnostics(rules)
	return store, diagnostics, nil
}

func newStore(cfg Config, readOnly bool) *Store {
	maxBytes := cfg.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFileBytes
	}
	return &Store{
		path:           cfg.Path,
		maxFileBytes:   maxBytes,
		familyEligible: cfg.FamilyEligible,
		readOnly:       readOnly,
		euid:           os.Geteuid(),
		renameFile:     os.Rename,
		syncFile:       func(file interface{ Sync() error }) error { return file.Sync() },
	}
}

// MatchesDeny reports whether any stored deny rule matches the requirement.
// Any load failure fails closed as an error; the gate rejects the call.
func (s *Store) MatchesDeny(ctx context.Context, requirement tool.Requirement) (bool, error) {
	return s.matches(ctx, requirement, EffectDeny)
}

// MatchesAllow reports whether any stored allow rule matches the
// requirement.
func (s *Store) MatchesAllow(ctx context.Context, requirement tool.Requirement) (bool, error) {
	return s.matches(ctx, requirement, EffectAllow)
}

func (s *Store) matches(ctx context.Context, requirement tool.Requirement, effect Effect) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	rules, err := s.currentRules()
	if err != nil {
		return false, err
	}
	if requirement.Kind == CapabilityCommandExecute {
		// Shell segments are matched independently and may be covered by
		// different rules: allow requires every segment covered, deny
		// triggers on any segment (bashrule.go).
		if effect == EffectAllow {
			return commandAllowCovered(rules, requirement.Match), nil
		}
		return commandDenyMatched(rules, requirement.Match), nil
	}
	for _, rule := range rules {
		if rule.Effect == effect && matchesRequirement(rule, requirement) {
			return true, nil
		}
	}
	return false, nil
}

// currentRules returns the immutable snapshot for read-only stores and a
// fresh hardened load for interactive stores. Atomic-rename updates make a
// lock-free read observe one complete file version.
func (s *Store) currentRules() ([]Rule, error) {
	if s.readOnly {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.snapshot, nil
	}
	return s.loadCurrent()
}

func (s *Store) loadCurrent() ([]Rule, error) {
	rules, err := s.loadFile(false)
	if err != nil {
		return nil, err
	}
	s.recordDiagnostics(rules)
	return rules, nil
}

// Diagnostics returns the non-fatal rule diagnostics of the most recent
// load. Diagnostics are reported separately from fatal file errors and
// never alter rule precedence.
func (s *Store) Diagnostics() []Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Diagnostic(nil), s.diagnostics...)
}

func (s *Store) recordDiagnostics(rules []Rule) []Diagnostic {
	diagnostics := diagnoseRules(rules, s.familyEligible)
	s.mu.Lock()
	s.diagnostics = diagnostics
	s.mu.Unlock()
	return append([]Diagnostic(nil), diagnostics...)
}

// WriteRules atomically appends the complete displayed allow-candidate
// batch to the workspace file. It locks the workspace, re-reads and merges
// under the lock, writes an owner-only temporary file, fsyncs it, renames
// it into place, and fsyncs the directory. Any failure leaves the prior
// complete file intact and returns an error, so the approved call is
// blocked rather than silently downgraded to a once-only approval.
func (s *Store) WriteRules(ctx context.Context, candidates []tool.RuleCandidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return &FileError{Path: s.path, Reason: FileReadOnly, Err: errors.New("read-only permission store cannot persist approvals")}
	}

	incoming := make([]Rule, 0, len(candidates))
	for index, candidate := range candidates {
		rule, err := candidateRule(index, candidate)
		if err != nil {
			return &FileError{Path: s.path, Reason: FileCandidateInvalid, Err: err}
		}
		incoming = append(incoming, rule)
	}

	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, dirPerm); err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if err := s.checkDirectory(directory); err != nil {
		return err
	}

	unlock, err := s.acquireLock(ctx, directory)
	if err != nil {
		return err
	}
	defer unlock()

	existing, err := s.loadFile(false)
	if err != nil {
		return err
	}
	merged := mergeRules(existing, incoming)
	encoded, err := encodeFile(merged)
	if err != nil {
		return withPath(err, s.path)
	}
	if int64(len(encoded)) > s.maxFileBytes {
		return &FileError{Path: s.path, Reason: FileTooLarge, Err: fmt.Errorf("merged file would be %d bytes (limit %d)", len(encoded), s.maxFileBytes)}
	}
	if err := s.replaceFile(directory, encoded); err != nil {
		return err
	}
	s.recordDiagnostics(merged)
	return nil
}

// replaceFile performs the owner-only temp write, fsync, atomic rename, and
// directory fsync. On any failure the temporary file is removed and the
// prior complete file remains.
func (s *Store) replaceFile(directory string, encoded []byte) error {
	temp, err := os.CreateTemp(directory, ".permissions-*.tmp")
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	tempPath := temp.Name()
	fail := func(reason FileErrorReason, cause error) error {
		_ = temp.Close()        // best-effort cleanup; the primary error is reported
		_ = os.Remove(tempPath) // best-effort cleanup; the prior file is intact
		return &FileError{Path: s.path, Reason: reason, Err: cause}
	}
	if err := temp.Chmod(filePerm); err != nil {
		return fail(FileIO, err)
	}
	if _, err := temp.Write(encoded); err != nil {
		return fail(FileIO, err)
	}
	if err := s.syncFile(temp); err != nil {
		return fail(FileIO, err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath) // best-effort cleanup; the prior file is intact
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if err := s.renameFile(tempPath, s.path); err != nil {
		_ = os.Remove(tempPath) // best-effort cleanup; the prior file is intact
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	// #nosec G304 -- directory is the parent of the one consumer-configured
	// permission-file path, opened only to fsync the completed rename.
	dir, err := os.OpenFile(directory, os.O_RDONLY, 0)
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	defer dir.Close()
	if err := s.syncFile(dir); err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	return nil
}

// acquireLock takes the per-workspace interprocess lock: an exclusive flock
// on the sibling lock file, retried non-blocking so ctx cancellation is
// honored. flock associates the lock with the open file description, so
// separate Store instances contend correctly whether they live in one
// process or many.
func (s *Store) acquireLock(ctx context.Context, directory string) (func(), error) {
	lockFile, err := openNoFollow(s.path+lockSuffix, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, &FileError{Path: s.path, Reason: FileLock, Err: err}
	}
	for {
		err := lockExclusiveNonBlocking(lockFile)
		if err == nil {
			return func() {
				// Best-effort release: closing the descriptor also drops the
				// lock even if the explicit unlock fails.
				_ = unlockFile(lockFile)
				_ = lockFile.Close()
			}, nil
		}
		if !errors.Is(err, errLockWouldBlock) {
			_ = lockFile.Close() // best-effort cleanup; the lock error is reported
			return nil, &FileError{Path: s.path, Reason: FileLock, Err: err}
		}
		select {
		case <-ctx.Done():
			_ = lockFile.Close() // best-effort cleanup; cancellation is reported
			return nil, &FileError{Path: s.path, Reason: FileLock, Err: ctx.Err()}
		case <-time.After(lockRetryInterval):
		}
	}
}

// loadFile performs one hardened read of the permission file. required
// selects the headless-startup behavior where a configured file must
// exist; an interactive store treats a missing file as an empty rule set.
func (s *Store) loadFile(required bool) ([]Rule, error) {
	file, err := openNoFollow(s.path, os.O_RDONLY, 0)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if required {
				return nil, &FileError{Path: s.path, Reason: FileMissing, Err: err}
			}
			return nil, nil
		case errors.Is(err, errSymlinkNotAllowed):
			return nil, &FileError{Path: s.path, Reason: FileSymlink, Err: err}
		default:
			return nil, &FileError{Path: s.path, Reason: FileIO, Err: err}
		}
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if err := s.checkFileInfo(file, info); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(file, s.maxFileBytes+1))
	if err != nil {
		return nil, &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if int64(len(data)) > s.maxFileBytes {
		return nil, &FileError{Path: s.path, Reason: FileTooLarge, Err: fmt.Errorf("file exceeds %d bytes", s.maxFileBytes)}
	}
	rules, err := decodeFile(data)
	if err != nil {
		return nil, withPath(err, s.path)
	}
	return rules, nil
}

// checkFileInfo enforces the hardening matrix on an opened file: regular
// type, exact owner-only mode, expected owner, single link, and bounded
// size. The stat comes from the open descriptor, so the checks bind to the
// bytes actually read.
func (s *Store) checkFileInfo(file *os.File, info fs.FileInfo) error {
	mode := info.Mode()
	if !mode.IsRegular() {
		return &FileError{Path: s.path, Reason: FileNotRegular, Err: fmt.Errorf("mode %v is not a regular file", mode)}
	}
	if perm := mode.Perm(); perm != filePerm {
		return &FileError{Path: s.path, Reason: FileModeUnexpected, Err: fmt.Errorf("mode %04o, require exactly %04o", perm, filePerm)}
	}
	if err := s.checkFileIdentity(file, info); err != nil {
		return err
	}
	if info.Size() > s.maxFileBytes {
		return &FileError{Path: s.path, Reason: FileTooLarge, Err: fmt.Errorf("size %d exceeds %d bytes", info.Size(), s.maxFileBytes)}
	}
	return nil
}

// checkDirectory rejects a store directory another user could tamper with.
func (s *Store) checkDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if !info.IsDir() {
		return &FileError{Path: s.path, Reason: FileNotRegular, Err: errors.New("store parent is not a directory")}
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return &FileError{Path: s.path, Reason: FileModeUnexpected, Err: fmt.Errorf("store directory mode %04o is group/world writable", perm)}
	}
	return s.checkDirIdentity(directory, info)
}

// mergeRules appends the incoming rules that are not already present,
// preserving every existing record (including denies) untouched.
func mergeRules(existing, incoming []Rule) []Rule {
	merged := append([]Rule(nil), existing...)
	seen := make(map[string]struct{}, len(existing))
	for _, rule := range existing {
		seen[rule.identity()] = struct{}{}
	}
	for _, rule := range incoming {
		key := rule.identity()
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, rule)
	}
	return merged
}

// withPath attaches the store path to a codec FileError.
func withPath(err error, path string) error {
	var fileErr *FileError
	if errors.As(err, &fileErr) && fileErr.Path == "" {
		fileErr.Path = path
	}
	return err
}

// candidateRule converts one displayed, validated allow candidate into its
// durable schema-v2 record. The conversion is strict: an unsupported kind,
// syntax, or grant pairing rejects the whole batch and nothing persists.
func candidateRule(index int, candidate tool.RuleCandidate) (Rule, error) {
	rule := Rule{Effect: EffectAllow, Capability: candidate.Kind}
	switch candidate.Kind {
	case CapabilityCommandExecute:
		if candidate.GrantClass != GrantClassCommandStart {
			return Rule{}, &RuleError{Index: index, Reason: "command candidate requires grant class " + GrantClassCommandStart}
		}
		return commandCandidateRule(index, rule, candidate)
	case CapabilityNetwork:
		switch candidate.GrantClass {
		case "", GrantClassNetworkProxyTarget:
			transport, host, port, ok := parseNetworkTargetMatch(candidate.Match)
			if !ok {
				return Rule{}, &RuleError{Index: index, Reason: "network candidate match is not a canonical target"}
			}
			rule.Class, rule.Transport, rule.Host, rule.Port = ClassNetworkTarget, transport, host, port
		case ClassNetworkBroad:
			bound, ok := parseCommandBoundMatch(candidate.Match)
			if !ok || bound.Target != candidate.GrantTarget {
				return Rule{}, &RuleError{Index: index, Reason: "broad egress candidate match is not command-bound"}
			}
			rule.Class, rule.Command, rule.Target = ClassNetworkBroad, bound.Command, bound.Target
		default:
			return Rule{}, &RuleError{Index: index, Reason: "unsupported network grant class " + candidate.GrantClass}
		}
	case CapabilityFilesystemRead, CapabilityFilesystemWrite:
		return filesystemCandidateRule(index, rule, candidate)
	default:
		return Rule{}, &RuleError{Index: index, Reason: "capability " + candidate.Kind + " has no durable rule representation"}
	}
	if err := rule.validate(index); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// commandCandidateRule parses the command candidate display syntax:
// Bash(tokens:*) or an exact normalized command. The stored canonical
// representation is structured; a raw string prefix is never stored.
//
// Two refusals keep the display-string channel unambiguous (spec, "Bare
// Bash(*) ... is never proposed automatically"): a candidate whose grant
// target — the exact normalized command the approval was FOR — itself lives
// in the Bash(...) rule-syntax namespace is refused outright, so a literal
// command such as `Bash(*)` or `Bash(rm:*)` can never be laundered into a
// wildcard or family record (ProposeCommandCandidate withholds the candidate
// for such commands; this is the store-side backstop). And the bare wildcard
// is file-only syntax no tool ever displays, so it is never accepted here;
// hand authoring uses the structured file records, which name the class
// explicitly and are therefore never ambiguous.
func commandCandidateRule(index int, rule Rule, candidate tool.RuleCandidate) (Rule, error) {
	match := candidate.Match
	if collidesWithBashRuleSyntax(candidate.GrantTarget) {
		return Rule{}, &RuleError{Index: index, Reason: "command collides with the Bash(...) rule syntax and has no reusable candidate"}
	}
	// The predicate IS the discriminator: its complement is the exact-command
	// arm, so display syntax is only ever parsed inside the guarded branches
	// and a new namespace must extend collidesWithBashRuleSyntax before any
	// parser arm can read it — an unguarded prefix cannot be laundered
	// through an exact candidate.
	switch {
	case !collidesWithBashRuleSyntax(match):
		rule.Class, rule.Command = ClassCommandInvoke, match
	case match == bashRuleSyntaxPrefix+"*)":
		return Rule{}, &RuleError{Index: index, Reason: "bare wildcard Bash(*) is never a displayed candidate; author a structured wildcard record instead"}
	case strings.HasSuffix(match, ":*)"):
		body := strings.TrimSuffix(strings.TrimPrefix(match, bashRuleSyntaxPrefix), ":*)")
		tokens, err := parseFamilyCandidateTokens(body)
		if err != nil {
			var ruleErr *RuleError
			if errors.As(err, &ruleErr) {
				ruleErr.Index = index
			}
			return Rule{}, err
		}
		rule.Class, rule.Tokens, rule.TrailingArguments = ClassCommandInvokeFamily, tokens, true
	default:
		return Rule{}, &RuleError{Index: index, Reason: "unsupported Bash(...) candidate syntax"}
	}
	if err := rule.validate(index); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// filesystemCandidateRule converts path, tree, and command-bound host
// candidates.
func filesystemCandidateRule(index int, rule Rule, candidate tool.RuleCandidate) (Rule, error) {
	write := candidate.Kind == CapabilityFilesystemWrite
	pick := func(read, writeClass string) string {
		if write {
			return writeClass
		}
		return read
	}
	switch candidate.GrantClass {
	case ClassFilesystemHostRead, ClassFilesystemHostWrite:
		if candidate.GrantClass != pick(ClassFilesystemHostRead, ClassFilesystemHostWrite) {
			return Rule{}, &RuleError{Index: index, Reason: "host grant class direction does not match capability"}
		}
		bound, ok := parseCommandBoundMatch(candidate.Match)
		if !ok || bound.Target != "" {
			return Rule{}, &RuleError{Index: index, Reason: "host filesystem candidate match is not command-bound"}
		}
		rule.Class, rule.Command = candidate.GrantClass, bound.Command
	case "", ClassFilesystemPathRead, ClassFilesystemPathWrite, ClassFilesystemTreeRead, ClassFilesystemTreeWrite:
		if root, isTree := strings.CutPrefix(candidate.Match, "tree:"); isTree {
			rule.Class, rule.Root = pick(ClassFilesystemTreeRead, ClassFilesystemTreeWrite), root
		} else {
			rule.Class, rule.Path = pick(ClassFilesystemPathRead, ClassFilesystemPathWrite), candidate.Match
		}
	default:
		return Rule{}, &RuleError{Index: index, Reason: "unsupported filesystem grant class " + candidate.GrantClass}
	}
	if err := rule.validate(index); err != nil {
		return Rule{}, err
	}
	return rule, nil
}
