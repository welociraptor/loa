package runner

import (
	"fmt"
	"strings"
	"text/template"

	"loa/internal/store"
)

// promptFields is the task as a prompt file sees it. Templates are data, not
// code: they may lay out these fields but cannot add tools or weaken the
// contract appended below.
type promptFields struct {
	ID       int64
	Title    string
	Body     string
	Scopes   []string
	Branch   string
	Worktree string
	Attempt  int
	Repo     string
}

// fieldsFor is the one place task and attempt state become template data.
func fieldsFor(task *store.Task, worktree, repo, branch string) promptFields {
	return promptFields{
		ID:       task.ID,
		Title:    task.Title,
		Body:     strings.TrimSpace(task.Body),
		Scopes:   task.Scopes,
		Branch:   branch,
		Worktree: worktree,
		Attempt:  task.Attempts,
		Repo:     repo,
	}
}

// renderTemplate executes a prompt file. A field the file names that does not
// exist is an error, so a typo cannot silently render an empty instruction.
func renderTemplate(name string, source []byte, fields promptFields) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(string(source))
	if err != nil {
		return "", fmt.Errorf("parse prompt %s: %w", name, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, fields); err != nil {
		return "", fmt.Errorf("render prompt %s: %w", name, err)
	}
	return b.String(), nil
}

// taskPrompt is the file's intent followed by the contract. The contract stays
// in code because the judgement enforces it; a prompt file must not be able to
// weaken "commit, do not push, stay in scope".
func taskPrompt(task *store.Task, name string, source []byte, fields promptFields) (string, error) {
	rendered, err := renderTemplate(name, source, fields)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(rendered, "\n") + "\n\n" + contract(task), nil
}

// contract is what a worker wakes up to and cannot opt out of.
func contract(task *store.Task) string {
	var b strings.Builder

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
