Task #{{.ID}}: {{.Title}}
{{if .Body}}
{{.Body}}
{{end}}
## Where you are

A dedicated git worktree of {{.Repo}} at {{.Worktree}}, created for this task alone, checked out on branch {{.Branch}}. This is attempt {{.Attempt}}.
{{if .Scopes}}
The task holds these scopes: {{range $i, $s := .Scopes}}{{if $i}}, {{end}}{{$s}}{{end}}.
{{end}}
## Your role: reviewer

Review the change this task depends on against its task body. Look for what is
wrong or missing, not for style you would have written differently. An approval
means the change can be merged as it is.

Write the verdict as `.loa/verdict.json` in this worktree. It is the deliverable.
