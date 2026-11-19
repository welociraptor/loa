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
	Diffs    []promptDiff
}

// promptDiff is one dependency's change, the subject of a review.
type promptDiff struct {
	ID     int64
	Title  string
	Branch string
	Base   string
	Diff   string
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
// weaken "commit, do not push, stay in scope". A review is judged differently,
// so its contract says so.
func taskPrompt(task *store.Task, name string, source []byte, fields promptFields) (string, error) {
	spec, err := specForRole(task.Role)
	if err != nil {
		spec = roleSpec{}
	}
	return taskPromptFor(task, spec, name, source, fields)
}

func taskPromptFor(task *store.Task, spec roleSpec, name string, source []byte, fields promptFields) (string, error) {
	rendered, err := renderTemplate(name, source, fields)
	if err != nil {
		return "", err
	}
	rendered = strings.TrimRight(rendered, "\n")

	if spec.deliverable == deliverableVerdict {
		if diffs := diffSection(fields.Diffs); diffs != "" {
			rendered += "\n\n" + diffs
		}
	}
	return rendered + "\n\n" + contractFor(task, spec), nil
}

// diffSection is the change the reviewer is asked about, not a summary it has
// to reconstruct. An empty section is omitted; the caller has already refused a
// review with nothing to look at.
func diffSection(diffs []promptDiff) string {
	if len(diffs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## The change under review\n\n")
	for _, d := range diffs {
		fmt.Fprintf(&b, "### Task #%d: %s\n\nBranch `%s` from `%s`:\n\n```diff\n%s\n```\n\n",
			d.ID, d.Title, d.Branch, d.Base, d.Diff)
	}
	return strings.TrimRight(b.String(), "\n")
}

// contract is what a worker wakes up to and cannot opt out of.
func contract(task *store.Task) string {
	return contractFor(task, roleSpec{})
}

func contractFor(task *store.Task, spec roleSpec) string {
	if spec.deliverable == deliverableVerdict {
		return reviewContract(task)
	}

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

// reviewContract replaces the commit gate with the verdict gate. A review may
// leave a commit or not; only the document finishes it.
func reviewContract(task *store.Task) string {
	var b strings.Builder

	b.WriteString("## Contract\n\n")
	b.WriteString("- Write `.loa/verdict.json` in this worktree. It is the deliverable, and a review without it fails.\n")
	b.WriteString("- The document is `{\"verdict\":\"approve|changes|blocked\",\"notes\":[{\"file\":\"...\",\"issue\":\"...\",\"suggestion\":\"...\"}],\"tasks\":[...]}`. Unknown fields are refused.\n")
	b.WriteString("- A commit cannot finish a review, and a review does not need one.\n")
	b.WriteString("- Do not push, do not switch branches, do not touch any other checkout.\n")
	if len(task.Scopes) > 0 {
		fmt.Fprintf(&b, "- Stay inside %s. Another agent is excluded from these while you hold them.\n", strings.Join(task.Scopes, ", "))
	}
	b.WriteString("- If the task cannot be done as described, say why and stop rather than making unrelated changes.\n\n")
	b.WriteString("Finish with a short summary of what you found and what the human must verify.")

	return b.String()
}
