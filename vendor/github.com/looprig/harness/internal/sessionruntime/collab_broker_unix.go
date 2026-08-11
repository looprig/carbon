//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package sessionruntime

import (
	"net"
	"os"
)

func collabPlatformSupported() bool { return true }

func listenCollabEndpoint(endpoint string) (net.Listener, error) {
	return net.Listen("unix", endpoint)
}

func collabUID() uint32 {
	uid, ok := collabUint32FromNonNegativeInt(os.Getuid())
	if !ok {
		return 0
	}
	return uid
}
