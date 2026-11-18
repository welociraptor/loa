package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Agent runs one task in one directory and returns the text it finished with.
// It is an interface so the runner can be tested without a model.
type Agent interface {
	Run(ctx context.Context, dir, prompt string) (string, error)
}

// Pi invokes the pi CLI in print mode: one prompt, one turn, no session.
type Pi struct {
	Bin   string   // default "pi"
	Model string   // empty uses the configured default
	Tools []string // default Allowlist
}

// Allowlist is the tool set a worker gets. Named explicitly rather than
// inherited, so a change to the human's default settings cannot silently widen
// what an autonomous agent may do.
var Allowlist = []string{"read", "edit", "write", "bash", "grep", "find", "ls"}

// Run invokes pi once, in print mode, with the given prompt as the only turn.
func (p Pi) Run(ctx context.Context, dir, prompt string) (string, error) {
	bin := p.Bin
	if bin == "" {
		bin = "pi"
	}
	tools := p.Tools
	if len(tools) == 0 {
		tools = Allowlist
	}

	args := []string{"--print", "--no-session", "--tools", strings.Join(tools, ",")}
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	// "--" keeps a prompt that starts with a dash from being read as a flag.
	args = append(args, "--", prompt)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	killProcessGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", bin, err, Tail(stderr.String(), 1000))
	}
	return stdout.String(), nil
}

// Tail returns the last n characters of s, on a rune boundary.
func Tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return "…" + string(runes[len(runes)-n:])
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, Tail(stderr.String(), 500))
	}
	return strings.TrimSpace(stdout.String()), nil
}
