//go:build windows || plan9 || js || wasip1

package sessionruntime

import "net"

func collabPeerUIDSupported() bool { return false }

func collabPlatformSupported() bool { return false }

func listenCollabEndpoint(string) (net.Listener, error) {
	return nil, errCollabBrokerUnsupportedPlatform
}

func collabPeerUID(net.Conn) (uint32, bool) { return 0, false }

func collabUID() uint32 { return 0 }
