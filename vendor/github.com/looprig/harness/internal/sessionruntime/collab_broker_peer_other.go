//go:build aix || android || dragonfly || freebsd || hurd || illumos || ios || netbsd || openbsd || solaris

package sessionruntime

import "net"

func collabPeerUIDSupported() bool { return false }

// Some Unix variants expose peer credentials through platform-specific APIs;
// when this narrow build does not expose one, the broker still enforces the
// owner-only socket and accepts the platform's unavailable credential signal.
func collabPeerUID(net.Conn) (uint32, bool) { return 0, false }
