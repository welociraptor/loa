//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// killProcessGroup puts the agent in its own process group and kills the whole
// group on cancellation. Killing only the direct child leaves whatever the
// worker spawned — a dev server, a test run, a sleep — holding the worktree.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The child leads its own group, so its pid is the group id.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
