package sessionruntime

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// workspace_coordinator.go implements the session-scoped workspace mutation
// coordinator (design §"Native checkpoint boundary and workspace gate" and
// §"File-tool optimistic concurrency and binding"). ONE workspaceCoordinator is
// shared by every primer and delegate loop in a session; it hands out mutation
// permits and background-process lifetime leases that realize the exclusion model:
//
//   - A PathMutation permit is SHARED across DIFFERENT canonical paths (many run
//     concurrently) but EXCLUSIVE against overlapping canonical path/tree scopes.
//   - A WholeMutation permit (Bash and other unknown-path mutators) is EXCLUSIVE
//     against every mutation and writable lifetime lease.
//   - A Checkpoint permit is the snapshot/restore gate. It excludes mutations and
//     writable lifetime leases while remaining compatible with read-only lifetimes.
//   - A scoped lifetime writer excludes overlapping path/scoped writers, while a
//     broad lifetime writer excludes every mutating operation.
//
// FAIRNESS / STARVATION-FREEDOM: a newer waiter never jumps ahead of an older
// conflicting writer. Disjoint scoped operations may proceed concurrently.
//
// CANCELLATION: Acquire is ctx-aware. A ctx that is done before or during the wait
// returns a typed *AcquireCanceledError and removes the waiter from the queue (or, if
// the grant raced in, releases it) so nothing leaks and no permit is stranded.

// LeaseHealth reports whether the workspace lease that underpins harness-managed
// mutations is currently healthy. A structured mutator MUST NOT commit when Healthy
// returns an error (fail-secure). A nil LeaseHealth means "no lease to verify" and is
// treated as always healthy (the bare/no-lease deployment).
type LeaseHealth interface {
	Healthy() error
}

// permitClass is the coordinator's internal exclusion class. The public
// WorkspaceOperation values remain unchanged; lifetime classes are a separate
// capability and never overload an operation value.
type permitClass uint8

const (
	classPathMutation permitClass = iota
	classWholeMutation
	classCheckpoint
	classLifetimeRead
	classLifetimeScopedWrite
	classLifetimeBroadWrite
)

// waiter is one queued Acquire. ready is closed exactly once, under the coordinator
// mutex, when the waiter is granted; granted records that grant under the same mutex
// so a racing ctx cancellation can distinguish "granted" from "still queued" without
// a data race.
type waiter struct {
	class   permitClass
	scopes  []string
	ready   chan struct{}
	granted bool
}

// workspaceCoordinator is the session-scoped mutation coordinator. All state is
// guarded by mu; the wake scan is the single place that transitions a queued waiter
// to granted, so the exclusion invariants live in exactly one function.
type workspaceCoordinator struct {
	health LeaseHealth

	mu          sync.Mutex
	sharedCount int                  // active PathMutation permits (all paths)
	exclusive   bool                 // an active whole/checkpoint permit
	active      map[*waiter]struct{} // every active mutation/lifetime permit
	queue       []*waiter            // FIFO of ungranted acquirers
}

// newWorkspaceCoordinator returns a session-scoped coordinator whose Healthy
// delegates to health (nil ⇒ always healthy).
func newWorkspaceCoordinator(health LeaseHealth) *workspaceCoordinator {
	return &workspaceCoordinator{health: health, active: make(map[*waiter]struct{})}
}

// Acquire blocks until the requested permit is granted or ctx is done. A done ctx
// (before enqueue, or during the wait) returns a typed *AcquireCanceledError and
// leaves no waiter queued and no permit stranded.
func (c *workspaceCoordinator) Acquire(ctx context.Context, operation tool.WorkspaceOperation, canonicalPath string) (tool.WorkspacePermit, error) {
	class, err := classifyOperation(operation, canonicalPath)
	if err != nil {
		return nil, err
	}
	scopes := []string(nil)
	if class == classPathMutation {
		scopes = []string{canonicalPath}
	}
	return c.acquire(ctx, &waiter{
		class:  class,
		scopes: scopes,
		ready:  make(chan struct{}),
	})
}

// AcquireLifetime reserves a prepared process's authoritative workspace access
// until the returned permit is released. Read-only leases coexist with every
// operation. Scoped and broad writes participate in the same FIFO conflict
// queue as structured mutations and checkpoints.
func (c *workspaceCoordinator) AcquireLifetime(ctx context.Context, access tool.WorkspaceAccess) (tool.WorkspacePermit, error) {
	class, scopes, err := classifyLifetimeAccess(access)
	if err != nil {
		return nil, err
	}
	return c.acquire(ctx, &waiter{
		class:  class,
		scopes: scopes,
		ready:  make(chan struct{}),
	})
}

func (c *workspaceCoordinator) acquire(ctx context.Context, w *waiter) (tool.WorkspacePermit, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, &AcquireCanceledError{Cause: ctxErr}
	}

	c.mu.Lock()
	c.queue = append(c.queue, w)
	c.wakeLocked()
	c.mu.Unlock()

	select {
	case <-w.ready:
		return c.finishReadyAcquire(ctx, w)
	case <-ctx.Done():
		c.mu.Lock()
		defer c.mu.Unlock()
		if w.granted {
			// The grant raced in as ctx fired: we own a live permit nobody else can
			// see, so release it here (releaseLocked re-runs the wake scan) and report
			// the cancellation.
			c.releaseLocked(w)
			return nil, &AcquireCanceledError{Cause: ctx.Err()}
		}
		c.removeFromQueueLocked(w)
		// Removing an ungranted waiter can unblock a waiter behind it (e.g. a canceled
		// exclusive that was holding back a shared waiter on a free path), so re-run the
		// wake scan — keeping "wake after any queue mutation" a uniform, local discipline.
		c.wakeLocked()
		return nil, &AcquireCanceledError{Cause: ctx.Err()}
	}
}

// finishReadyAcquire completes the ready side of Acquire's select. Cancellation
// must be checked again because ready and ctx.Done can become selectable together;
// if cancellation won the race before the caller observed ready, release the
// otherwise-stranded grant and report the typed cancellation.
func (c *workspaceCoordinator) finishReadyAcquire(
	ctx context.Context,
	w *waiter,
) (tool.WorkspacePermit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		c.releaseLocked(w)
		return nil, &AcquireCanceledError{Cause: ctxErr}
	}
	return &grantedPermit{coord: c, w: w}, nil
}

// Healthy reports lease health, delegating to the injected LeaseHealth (nil ⇒ nil).
func (c *workspaceCoordinator) Healthy() error {
	if c.health == nil {
		return nil
	}
	return c.health.Healthy()
}

// wakeLocked grants every currently-grantable waiter. It is the SOLE grant
// point, called under mu after every enqueue, cancellation, and release.
//
// FIFO writer preference is conflict-aware: a waiter cannot jump ahead of an
// older queued waiter it conflicts with. Disjoint path/scoped writers may still
// proceed, and read-only lifetime leases may coexist with a queued or active
// checkpoint because they cannot mutate the snapshot.
func (c *workspaceCoordinator) wakeLocked() {
	i := 0
	for i < len(c.queue) {
		w := c.queue[i]
		if !c.grantableLocked(i, w) {
			i++
			continue
		}
		c.activateLocked(w)
		c.grantLocked(i, w)
	}
}

func (c *workspaceCoordinator) grantableLocked(index int, candidate *waiter) bool {
	for active := range c.active {
		if waitersConflict(active, candidate) {
			return false
		}
	}
	for i := 0; i < index; i++ {
		if waitersConflict(c.queue[i], candidate) {
			return false
		}
	}
	return true
}

func (c *workspaceCoordinator) activateLocked(w *waiter) {
	c.active[w] = struct{}{}
	switch w.class {
	case classPathMutation:
		c.sharedCount++
	case classWholeMutation, classCheckpoint:
		c.exclusive = true
	}
}

// grantLocked removes queue[index] and marks/opens the waiter as granted. The caller
// holds mu.
func (c *workspaceCoordinator) grantLocked(index int, w *waiter) {
	c.queue = append(c.queue[:index], c.queue[index+1:]...)
	w.granted = true
	close(w.ready)
}

// releaseLocked returns a granted permit's resources and re-runs the wake scan. The
// caller holds mu. It is idempotent at the permit boundary via grantedPermit's Once.
func (c *workspaceCoordinator) releaseLocked(w *waiter) {
	delete(c.active, w)
	switch w.class {
	case classWholeMutation, classCheckpoint:
		c.exclusive = false
	case classPathMutation:
		if c.sharedCount > 0 {
			c.sharedCount--
		}
	}
	c.wakeLocked()
}

// removeFromQueueLocked removes an ungranted waiter from the queue (a no-op if it is
// no longer present). The caller holds mu.
func (c *workspaceCoordinator) removeFromQueueLocked(w *waiter) {
	for i, q := range c.queue {
		if q == w {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return
		}
	}
}

// grantedPermit is the tool.WorkspacePermit returned by a successful Acquire. Release
// is idempotent (Once) so callers may safely defer it immediately after acquisition.
type grantedPermit struct {
	coord *workspaceCoordinator
	w     *waiter
	once  sync.Once
}

// Release returns the permit's resources exactly once.
func (p *grantedPermit) Release() {
	p.once.Do(func() {
		p.coord.mu.Lock()
		defer p.coord.mu.Unlock()
		p.coord.releaseLocked(p.w)
	})
}

// classifyOperation validates the operation/path pairing and maps it to an internal
// exclusion class. PathMutation requires a non-empty path; whole/checkpoint require an
// empty path.
func classifyOperation(operation tool.WorkspaceOperation, path string) (permitClass, error) {
	switch operation {
	case tool.WorkspaceOperationPathMutation:
		if path == "" {
			return 0, &WorkspacePathError{Operation: operation, Reason: "a path mutation requires a non-empty canonical path"}
		}
		return classPathMutation, nil
	case tool.WorkspaceOperationWholeMutation:
		if path != "" {
			return 0, &WorkspacePathError{Operation: operation, Reason: "a whole-workspace operation requires an empty canonical path"}
		}
		return classWholeMutation, nil
	case tool.WorkspaceOperationCheckpoint:
		if path != "" {
			return 0, &WorkspacePathError{Operation: operation, Reason: "a whole-workspace operation requires an empty canonical path"}
		}
		return classCheckpoint, nil
	default:
		return 0, &InvalidWorkspaceOperationError{Operation: operation}
	}
}

func classifyLifetimeAccess(access tool.WorkspaceAccess) (permitClass, []string, error) {
	writePaths := access.WritePaths()
	writeTrees := access.WriteTrees()
	switch access.Kind {
	case tool.WorkspaceAccessReadOnly:
		if len(writePaths) != 0 || len(writeTrees) != 0 {
			return 0, nil, &WorkspaceLifetimeAccessError{
				Kind:   access.Kind,
				Reason: "read-only access must not declare write scopes",
			}
		}
		return classLifetimeRead, nil, nil
	case tool.WorkspaceAccessBroadWrite:
		if len(writePaths) != 0 || len(writeTrees) != 0 {
			return 0, nil, &WorkspaceLifetimeAccessError{
				Kind:   access.Kind,
				Reason: "broad-write access must not declare scoped paths",
			}
		}
		return classLifetimeBroadWrite, nil, nil
	case tool.WorkspaceAccessScopedWrite:
		if len(writePaths)+len(writeTrees) == 0 {
			return 0, nil, &WorkspaceLifetimeAccessError{
				Kind:   access.Kind,
				Reason: "scoped-write access requires at least one write scope",
			}
		}
	default:
		return 0, nil, &WorkspaceLifetimeAccessError{
			Kind:   access.Kind,
			Reason: "unrecognized workspace access kind",
		}
	}

	scopes := make([]string, 0, len(writePaths)+len(writeTrees))
	for _, scope := range writePaths {
		if err := validateLifetimeScope(scope); err != nil {
			return 0, nil, err
		}
		scopes = append(scopes, scope)
	}
	for _, scope := range writeTrees {
		if err := validateLifetimeScope(scope); err != nil {
			return 0, nil, err
		}
		scopes = append(scopes, scope)
	}
	return classLifetimeScopedWrite, scopes, nil
}

func validateLifetimeScope(scope string) error {
	if scope == "" || !filepath.IsAbs(scope) || filepath.Clean(scope) != scope {
		return &WorkspaceLifetimeAccessError{
			Kind:   tool.WorkspaceAccessScopedWrite,
			Reason: "write scopes must be absolute canonical paths",
		}
	}
	return nil
}

func waitersConflict(left, right *waiter) bool {
	if left.class == classLifetimeRead || right.class == classLifetimeRead {
		return false
	}
	if globalWorkspaceWriter(left.class) || globalWorkspaceWriter(right.class) {
		return true
	}
	for _, leftScope := range left.scopes {
		for _, rightScope := range right.scopes {
			if workspaceScopesOverlap(leftScope, rightScope) {
				return true
			}
		}
	}
	return false
}

func globalWorkspaceWriter(class permitClass) bool {
	switch class {
	case classWholeMutation, classCheckpoint, classLifetimeBroadWrite:
		return true
	default:
		return false
	}
}

func workspaceScopesOverlap(left, right string) bool {
	relative, err := filepath.Rel(left, right)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	relative, err = filepath.Rel(right, left)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// AcquireCanceledError reports that an Acquire returned because its ctx was done
// before or during the wait. Cause is the ctx error (context.Canceled or
// context.DeadlineExceeded). No permit was granted; no waiter remains queued.
type AcquireCanceledError struct{ Cause error }

func (e *AcquireCanceledError) Error() string {
	return "sessionruntime: workspace permit acquisition canceled: " + e.Cause.Error()
}

func (e *AcquireCanceledError) Unwrap() error { return e.Cause }

// InvalidWorkspaceOperationError reports an Acquire with an operation value the
// coordinator does not recognize (fail-secure: no permit is granted).
type InvalidWorkspaceOperationError struct{ Operation tool.WorkspaceOperation }

func (e *InvalidWorkspaceOperationError) Error() string {
	return "sessionruntime: invalid workspace operation: " + strconv.Itoa(int(e.Operation))
}

// WorkspacePathError reports an Acquire whose canonicalPath does not match its
// operation (a path mutation with an empty path, or a whole-workspace operation with
// a non-empty path).
type WorkspacePathError struct {
	Operation tool.WorkspaceOperation
	Reason    string
}

func (e *WorkspacePathError) Error() string {
	return "sessionruntime: invalid workspace permit path: " + e.Reason
}

// WorkspaceLifetimeAccessError reports an invalid authoritative access summary.
// Invalid and non-canonical scope sets fail secure without enqueuing a waiter.
type WorkspaceLifetimeAccessError struct {
	Kind   tool.WorkspaceAccessKind
	Reason string
}

func (e *WorkspaceLifetimeAccessError) Error() string {
	return "sessionruntime: invalid workspace lifetime access: " + e.Reason
}

var _ tool.WorkspaceCoordinator = (*workspaceCoordinator)(nil)
var _ tool.WorkspaceLifetimeCoordinator = (*workspaceCoordinator)(nil)
