//go:build linux

package sessionruntime

import (
	"net"

	"golang.org/x/sys/unix"
)

func collabPeerUIDSupported() bool { return true }

func collabPeerUID(conn net.Conn) (uint32, bool) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid uint32
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			controlErr = err
			return
		}
		uid = cred.Uid
	})
	if err != nil || controlErr != nil {
		return 0, false
	}
	return uid, true
}
