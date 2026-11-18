package runner

import (
	"fmt"
	"strings"

	"loa/internal/store"
)

// prompt is what a worker wakes up to. It carries the contract, because the
// agent cannot see the runner and must not have to infer it.
func prompt(task *store.Task, worktree, repo, branch string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Task #%d: %s\n\n", task.ID, task.Title)
	if body := strings.TrimSpace(task.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "## Where you are\n\n")
	fmt.Fprintf(&b, "A dedicated git worktree of %s at %s, created for this task alone, checked out on branch %s.\n\n", repo, worktree, branch)

	b.WriteString("## Contract\n\n")
	b.WriteString("- Commit your work to the current branch. Anything uncommitted when you exit is discarded, and a run that adds no commit counts as a failure.\n")
	b.WriteString("- Do not push, do not switch branches, do not touch any other checkout.\n")
	if len(task.Scopes) > 0 {
		fmt.Fprintf(&b, "- Stay inside %s. Another agent is excluded from these while you hold them.\n", strings.Join(task.Scopes, ", "))
	}
	b.WriteString("- If the task cannot be done as described, say why and stop rather than making unrelated changes.\n\n")
	b.WriteString("Finish with a short summary of what changed and what the human must verify.")

	return b.String()
}
