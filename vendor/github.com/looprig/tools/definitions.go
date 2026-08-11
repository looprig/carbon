// Package tools provides independent definition builders for Looprig's standard
// tools. Concrete constructors and options live in focused subpackages.
package tools

import (
	"context"
	"net/http"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/askuser"
	"github.com/looprig/tools/bash"
	"github.com/looprig/tools/editfile"
	"github.com/looprig/tools/fetch"
	"github.com/looprig/tools/glob"
	"github.com/looprig/tools/grep"
	"github.com/looprig/tools/internal/definition"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/process"
	"github.com/looprig/tools/readfile"
	"github.com/looprig/tools/task"
	"github.com/looprig/tools/websearch"
	"github.com/looprig/tools/writefile"
)

type DefinitionBuildError = definition.BuildError

func GlobDefinition(readGuard loop.ReadGuard, options ...glob.GlobOption) tool.Definition {
	sealed := append([]glob.GlobOption(nil), options...)
	return tool.NewDefinition("Glob", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(readGuard) {
			return nil, &DefinitionBuildError{Definition: "Glob", Dependency: "read_guard"}
		}
		return []tool.InvokableTool{glob.NewGlob(bindings.Workspace.Root, readGuard, sealed...)}, nil
	})
}

func GrepDefinition(readGuard loop.ReadGuard, options ...grep.GrepOption) tool.Definition {
	sealed := append([]grep.GrepOption(nil), options...)
	return tool.NewDefinition("Grep", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(readGuard) {
			return nil, &DefinitionBuildError{Definition: "Grep", Dependency: "read_guard"}
		}
		return []tool.InvokableTool{grep.NewGrep(bindings.Workspace.Root, readGuard, sealed...)}, nil
	})
}

func TaskDefinitions() tool.Definition {
	return tool.NewBundleDefinition("Tasks", []string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return task.NewTools(), nil
	})
}

func AskUserDefinition() tool.Definition {
	return tool.NewDefinition("AskUser", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{askuser.NewAskUser()}, nil
	})
}

func WebSearchDefinition(provider websearch.SearchProvider) tool.Definition {
	return tool.NewDefinition("WebSearch", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(provider) {
			return nil, &DefinitionBuildError{Definition: "WebSearch", Dependency: "provider"}
		}
		return []tool.InvokableTool{websearch.NewWebSearch(provider)}, nil
	})
}

func FetchDefinition(client *http.Client) tool.Definition {
	return tool.NewDefinition("Fetch", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(client) {
			return nil, &DefinitionBuildError{Definition: "Fetch", Dependency: "client"}
		}
		return []tool.InvokableTool{fetch.NewFetch(client)}, nil
	})
}

func ReadFileDefinition(readGuard loop.ReadGuard, options ...readfile.ReadFileOption) tool.Definition {
	sealed := append([]readfile.ReadFileOption(nil), options...)
	return tool.NewDefinition("ReadFile", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(readGuard) {
			return nil, &DefinitionBuildError{Definition: "ReadFile", Dependency: "read_guard"}
		}
		return []tool.InvokableTool{readfile.NewReadFile(bindings.Workspace.Root, readGuard, loopObservations(bindings.Workspace.Observations), sealed...)}, nil
	})
}

// WriteFileDefinition accepts writefile.Option values such as
// writefile.WithHostWrites(). Do not pass writefile.WithMutationCoordinator
// here: this entry point already injects the session-bound coordinator, and a
// caller-supplied one is applied after it and silently wins, defeating the
// PathMutation permit and lease-health check with no error.
func WriteFileDefinition(options ...writefile.Option) tool.Definition {
	sealed := append([]writefile.Option(nil), options...)
	return tool.NewDefinition("WriteFile", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		opts := append([]writefile.Option{writefile.WithMutationCoordinator(bindings.Workspace.Coordinator)}, sealed...)
		return []tool.InvokableTool{writefile.New(bindings.Workspace.Root, loopObservations(bindings.Workspace.Observations), opts...)}, nil
	})
}

// EditFileDefinition accepts editfile.Option values such as
// editfile.WithHostWrites(). Do not pass editfile.WithMutationCoordinator
// here: this entry point already injects the session-bound coordinator, and a
// caller-supplied one is applied after it and silently wins, defeating the
// PathMutation permit and lease-health check with no error.
func EditFileDefinition(options ...editfile.Option) tool.Definition {
	sealed := append([]editfile.Option(nil), options...)
	return tool.NewDefinition("EditFile", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		opts := append([]editfile.Option{editfile.WithMutationCoordinator(bindings.Workspace.Coordinator)}, sealed...)
		return []tool.InvokableTool{editfile.New(bindings.Workspace.Root, loopObservations(bindings.Workspace.Observations), opts...)}, nil
	})
}

func Bash(options ...bash.BashOption) tool.Definition {
	factory, initErr := bash.NewFactory(options...)
	return tool.NewDefinition("Bash", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if initErr != nil {
			return nil, initErr
		}
		return []tool.InvokableTool{factory(bindings.Workspace.Root, bindings.Workspace.Coordinator, bindings.Workspace.Observations)}, nil
	})
}

// AsyncProcessRunnerResolver resolves the concrete tool.AsyncProcessRunner a
// session-supervised Bash definition binds to, from the Harness-validated
// bindings.LoopID at Build (design spec "Workspace coordination": "At
// definition Build, Tools invokes the resolver with the validated
// bindings.LoopID"). Tools owns this resolver shape; a product composition
// root supplies the concrete implementation over its own per-role
// executor set. Runner selection is therefore complete before the concrete
// Bash tool is ever invoked — it never derives from invocation-time
// provenance.
type AsyncProcessRunnerResolver func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error)

// BashDefinition builds the session-supervised Bash tool: unlike Bash, a
// SUPERVISED call (background, or a present yield_time_ms — bash/prepare.go's
// normalizeSupervision) routes through the shared, runner-free
// process.Supervisor (bash/supervised.go). resolver supplies the concrete
// tool.AsyncProcessRunner: Build calls it exactly once, only after Harness
// has validated bindings, with the validated bindings.LoopID, and rejects a
// resolver error or a nil/typed-nil returned runner without ever producing a
// tool. options configure the underlying BashTool exactly like Bash's own —
// resolved once here, never reapplied per Build.
func BashDefinition(resolver AsyncProcessRunnerResolver, options ...bash.BashOption) tool.Definition {
	factory, initErr := bash.NewSupervisedFactory(options...)
	return tool.NewDefinition("Bash", tool.RequiresWorkspace|tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if initErr != nil {
			return nil, initErr
		}
		if resolver == nil {
			return nil, &DefinitionBuildError{Definition: "Bash", Dependency: "resolver"}
		}
		runner, err := resolver(ctx, bindings.LoopID)
		if err != nil {
			return nil, &DefinitionBuildError{Definition: "Bash", Dependency: "runner", Cause: err}
		}
		if workspace.IsNil(runner) {
			return nil, &DefinitionBuildError{Definition: "Bash", Dependency: "runner"}
		}
		built, err := factory(bindings, runner)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{built}, nil
	})
}

// ProcessOutputDefinition builds the read-only ProcessOutput tool bound to
// this session's shared, runner-free process.Supervisor (process/
// output_tool.go). Argument-free: unlike BashDefinition it captures no
// resolver and no options — a caller's owned process is read through the
// same registry entry Bash and its two sibling definitions share, keyed by
// process.SupervisorResourceKey alone.
func ProcessOutputDefinition() tool.Definition {
	return tool.NewDefinition("ProcessOutput", tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		supervisor, err := resolveProcessSupervisor(ctx, bindings)
		if err != nil {
			return nil, err
		}
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return []tool.InvokableTool{process.NewProcessOutput(supervisor, owner)}, nil
	})
}

// ProcessInputDefinition builds the mutating ProcessInput tool over the same
// shared supervisor entry ProcessOutputDefinition resolves (process/
// input_tool.go). Argument-free for the identical reason.
func ProcessInputDefinition() tool.Definition {
	return tool.NewDefinition("ProcessInput", tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		supervisor, err := resolveProcessSupervisor(ctx, bindings)
		if err != nil {
			return nil, err
		}
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return []tool.InvokableTool{process.NewProcessInput(supervisor, owner)}, nil
	})
}

// ProcessStopDefinition builds the mutating ProcessStop tool over the same
// shared supervisor entry (process/stop_tool.go). Argument-free for the
// identical reason.
func ProcessStopDefinition() tool.Definition {
	return tool.NewDefinition("ProcessStop", tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		supervisor, err := resolveProcessSupervisor(ctx, bindings)
		if err != nil {
			return nil, err
		}
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return []tool.InvokableTool{process.NewProcessStop(supervisor, owner)}, nil
	})
}

// resolveProcessSupervisor obtains this session's ONE shared, runner-free
// process.Supervisor session resource, keyed by
// process.SupervisorResourceKey, through bindings.Process.Registry — the
// keyed registry, never a package global. Harness's own Definition.Build
// already validated that bindings.Process and its Registry are present
// before this factory ever runs (tool.RequiresProcessServices' bindings
// validation), so the only failures reachable here are the registry's own
// (e.g. session closing) or an unexpected resource already occupying this
// key.
//
// bash/supervised.go's runSupervised independently resolves the SAME
// registry entry the first time a supervised Bash call actually executes.
// Both call sites key on process.SupervisorResourceKey and type-assert the
// result to the identical exported process.SupervisorResource — there is no
// longer a private per-package wrapper type on either side — so whichever of
// the four process-backed definitions' GetOrCreate call reaches this key
// FIRST still hands every later caller, regardless of package, a resource it
// can type-assert successfully.
func resolveProcessSupervisor(ctx context.Context, bindings tool.Bindings) (*process.Supervisor, error) {
	resource, err := bindings.Process.Registry.GetOrCreate(ctx, process.SupervisorResourceKey, process.NewSupervisorResource)
	if err != nil {
		return nil, &DefinitionBuildError{Definition: "Process", Dependency: "process_registry", Cause: err}
	}
	sr, ok := resource.(*process.SupervisorResource)
	if !ok || sr == nil || sr.Supervisor == nil {
		return nil, &DefinitionBuildError{Definition: "Process", Dependency: "process_registry"}
	}
	return sr.Supervisor, nil
}

func loopObservations(shared tool.WorkspaceObservations) tool.WorkspaceObservations {
	if workspace.IsNil(shared) {
		return tool.NewWorkspaceObservations()
	}
	return shared
}
