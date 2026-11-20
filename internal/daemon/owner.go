package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// runOwnerGone reports whether an active run's recorded owner is provably
// gone. The runtime lock only covers this process's XDG_RUNTIME_DIR, so a
// different host or runtime directory may still be working the same database.
// Anything short of proof of death leaves the row alone.
func runOwnerGone(owner string) (bool, string) {
	host, pid, err := parseOwner(owner)
	if err != nil {
		return false, fmt.Sprintf("cannot parse owner %q: %v; not proof of death", owner, err)
	}
	local, err := os.Hostname()
	if err != nil {
		return false, fmt.Sprintf("cannot identify this host: %v; not proof of death", err)
	}
	if host != local {
		return true, fmt.Sprintf("owner is on host %q, not this host %q", host, local)
	}

	err = syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return false, fmt.Sprintf("held by a live owner (process %d is alive)", pid)
	case errors.Is(err, syscall.ESRCH):
		return true, fmt.Sprintf("owner process %d is gone", pid)
	case errors.Is(err, syscall.EPERM):
		return false, fmt.Sprintf("held by a live owner (process %d is alive but cannot be signalled)", pid)
	default:
		return false, fmt.Sprintf("cannot tell whether owner process %d is alive: %v", pid, err)
	}
}

func parseOwner(owner string) (string, int, error) {
	slash := strings.LastIndexByte(owner, '/')
	if slash <= 0 || slash == len(owner)-1 {
		return "", 0, errors.New(`want "host/pid"`)
	}
	host := owner[:slash]
	pid, err := strconv.Atoi(owner[slash+1:])
	if err != nil {
		return "", 0, fmt.Errorf("pid %q is not a number", owner[slash+1:])
	}
	if pid <= 0 {
		return "", 0, fmt.Errorf("pid %d is not positive", pid)
	}
	return host, pid, nil
}
