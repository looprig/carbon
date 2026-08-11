//go:build aix || darwin || dragonfly || freebsd || hurd || illumos || linux || netbsd || openbsd || solaris

package collab

import (
	"context"
	"net"
)

func dialEndpoint(ctx context.Context, endpoint string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", endpoint)
}

func platformSupported() bool { return true }
