//go:build !unix

package runner

import "os/exec"

// killProcessGroup is a no-op where process groups are not available; the
// runner targets NixOS and only the direct child is killed there.
func killProcessGroup(*exec.Cmd) {}
