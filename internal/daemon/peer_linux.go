//go:build linux

package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkControlSocketPlatform is the startup gate for the Linux-only control
// socket. The ownership check below cannot be implemented portably, so the
// daemon refuses to start rather than accepting connections it cannot verify.
func checkControlSocketPlatform() error { return nil }

// checkPeerUID reads SO_PEERCRED from the accepted connection and refuses
// anyone but this process's owner. The socket is not a privilege boundary, but
// it is still not a place for another user's requests.
func checkPeerUID(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}

	var (
		cred    *syscall.Ucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("peer credentials: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("peer credentials: %w", credErr)
	}

	uid := os.Geteuid()
	if int(cred.Uid) != uid {
		return fmt.Errorf("peer uid %d is not this daemon's uid %d", cred.Uid, uid)
	}
	return nil
}
