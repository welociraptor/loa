//go:build !linux

package daemon

import (
	"errors"
	"net"
)

// The control socket's ownership check is Linux-specific. On any other platform
// the daemon refuses to start rather than serving connections it cannot verify.
func checkControlSocketPlatform() error {
	return errors.New("the control socket is Linux-only")
}

func checkPeerUID(*net.UnixConn) error {
	return errors.New("peer credentials are not supported on this platform")
}
