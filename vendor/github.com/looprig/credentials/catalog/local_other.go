//go:build !windows && !darwin && !linux

package catalog

import (
	"context"
	"path/filepath"
)

type unsupportedPlatform struct{ rootPath string }

func openPlatform(root string) (platform, error) {
	clean := filepath.Clean(root)
	if root == "" || !filepath.IsAbs(root) || root != clean {
		return nil, errInsecure
	}
	return nil, errUnsupported
}

func (*unsupportedPlatform) root() string                         { return "" }
func (*unsupportedPlatform) ensureData() (bool, error)            { return false, errUnsupported }
func (*unsupportedPlatform) close() error                         { return nil }
func (*unsupportedPlatform) validateRoot() error                  { return errUnsupported }
func (*unsupportedPlatform) validateLock() error                  { return errUnsupported }
func (*unsupportedPlatform) lock(context.Context) (func(), error) { return nil, errUnsupported }
func (*unsupportedPlatform) read(string) ([]byte, error)          { return nil, errUnsupported }
func (*unsupportedPlatform) writeTemp([]byte) (string, error)     { return "", errUnsupported }
func (*unsupportedPlatform) rename(string, string) error          { return errUnsupported }
func (*unsupportedPlatform) remove(string) (bool, error)          { return false, errUnsupported }
func (*unsupportedPlatform) syncDir() error                       { return errUnsupported }
