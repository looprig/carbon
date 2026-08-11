// Package editfile exposes the standard workspace file editor.
package editfile

import (
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/filemutation"
)

type Tool = filemutation.EditFile
type Option = filemutation.FileMutatorOption
type LeaseUnhealthyError = filemutation.LeaseUnhealthyError
type StaleFileError = filemutation.StaleFileError
type IrregularFileError = filemutation.IrregularFileError

func New(root string, observations tool.WorkspaceObservations, options ...Option) *Tool {
	return filemutation.NewEditFile(root, observations, options...)
}

func WithMutationCoordinator(coordinator tool.WorkspaceCoordinator) Option {
	return filemutation.WithMutationCoordinator(coordinator)
}

func WithHostWrites() Option {
	return filemutation.WithHostWrites()
}
