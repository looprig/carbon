//go:build !windows && !darwin && !linux

package local

import "path/filepath"

func openPlatform(root string) (platform, error) {
	clean := filepath.Clean(root)
	if root == "" || !filepath.IsAbs(root) || root != clean {
		return nil, errInsecure
	}
	return nil, &UnsupportedPlatformError{}
}
