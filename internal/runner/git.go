package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// gitOutput runs one git command. Git is still used here for one thing only:
// reading the state of a worktree Herdr created. The runner does not create
// worktrees.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, tail(stderr.String(), 500))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// commitsAhead counts the commits a branch has beyond the commit it started
// from. This, not the agent's own report, is what decides success.
func commitsAhead(ctx context.Context, worktree, base string) (int, error) {
	out, err := gitOutput(ctx, worktree, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// diffBetween is the change a branch carries against its base. The three-dot
// form uses the branches' merge base, so a configured base that moved since the
// task branched does not put unrelated reverse commits into the review.
func diffBetween(ctx context.Context, repoPath, base, branch string) (string, error) {
	return gitOutput(ctx, repoPath, "diff", "--no-color", base+"..."+branch)
}

// deleteBranch removes a branch nobody has checked out. The runner asked Herdr
// for this branch name, so it also owns cleaning it up when an attempt produced
// nothing; otherwise every failed attempt litters the repository.
func deleteBranch(ctx context.Context, repoPath, branch string) error {
	_, err := gitOutput(ctx, repoPath, "branch", "--delete", "--force", branch)
	return err
}

// SummaryOf turns an agent's terminal output into the part worth keeping. The
// pane is a screen, not a transcript: it carries box drawing, blank padding and
// a status bar, and the useful text is at the end. It is exported only because
// its filtering is worth testing on a recorded screen.
func SummaryOf(screen string, lines int) string {
	var kept []string
	for _, line := range strings.Split(screen, "\n") {
		if isChrome(line) {
			continue
		}
		kept = append(kept, strings.TrimRight(line, " \t"))
	}
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return tail(strings.Join(kept, "\n"), 2000)
}

// isChrome reports a line that is decoration: blank, or box drawing only.
func isChrome(line string) bool {
	for _, r := range line {
		switch r {
		case ' ', '\t', '─', '━', '│', '┃', '┌', '┐', '└', '┘', '├', '┤', '┬', '┴', '┼', '╭', '╮', '╯', '╰':
		default:
			return false
		}
	}
	return true
}

func tail(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return "…" + string(runes[len(runes)-n:])
}
