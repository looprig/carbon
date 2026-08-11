//go:build !aix && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !linux && !netbsd && !openbsd && !solaris

package collab

import (
	"context"
	"net"
)

func dialEndpoint(context.Context, string) (net.Conn, error) {
	return nil, ErrUnsupportedPlatform
}

func platformSupported() bool { return false }
