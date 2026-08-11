// Package writefile exposes the standard workspace file writer.
package writefile

import (
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/filemutation"
)

type Tool = filemutation.WriteFile
type Option = filemutation.FileMutatorOption
type LeaseUnhealthyError = filemutation.LeaseUnhealthyError
type StaleFileError = filemutation.StaleFileError
type FileCreateConflictError = filemutation.FileCreateConflictError
type IrregularFileError = filemutation.IrregularFileError

func New(root string, observations tool.WorkspaceObservations, options ...Option) *Tool {
	return filemutation.NewWriteFile(root, observations, options...)
}

func WithMutationCoordinator(coordinator tool.WorkspaceCoordinator) Option {
	return filemutation.WithMutationCoordinator(coordinator)
}

func WithHostWrites() Option {
	return filemutation.WithHostWrites()
}
