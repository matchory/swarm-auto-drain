//go:build linux

package identity

import (
	"fmt"
	"net"
	"syscall"
)

//nolint:gochecknoinits // build-tag-driven dispatch; cannot be expressed as a const
func init() {
	readPeerCredImpl = readPeerCredLinux
}

// readPeerCredLinux retrieves SO_PEERCRED from the underlying socket
// file descriptor. SyscallConn → Control is the documented pattern for
// reaching the FD without racing the netpoller — Control runs the
// callback synchronously while the runtime guarantees the FD is valid.
func readPeerCredLinux(conn *net.UnixConn) (PeerCred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCred{}, fmt.Errorf("syscall conn: %w", err)
	}
	var ucred *syscall.Ucred
	var sockoptErr error
	controlErr := raw.Control(func(fd uintptr) {
		ucred, sockoptErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if controlErr != nil {
		return PeerCred{}, fmt.Errorf("raw control: %w", controlErr)
	}
	if sockoptErr != nil {
		return PeerCred{}, fmt.Errorf("getsockopt SO_PEERCRED: %w", sockoptErr)
	}
	return PeerCred{
		PID: uint32(ucred.Pid),
		UID: ucred.Uid,
		GID: ucred.Gid,
	}, nil
}
