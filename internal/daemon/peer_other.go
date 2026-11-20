//go:build !linux

package daemon

import (
	"errors"
	"net"
)

// The control socket's ownership check is Linux-specific. On any other platform
// the daemon refuses every connection rather than silently dropping the check.
func checkPeerUID(*net.UnixConn) error {
	return errors.New("peer credentials are not supported on this platform")
}
