Task #{{.ID}}: {{.Title}}
{{if .Body}}
{{.Body}}
{{end}}
## Where you are

A dedicated git worktree of {{.Repo}} at {{.Worktree}}, created for this task alone, checked out on branch {{.Branch}}.
