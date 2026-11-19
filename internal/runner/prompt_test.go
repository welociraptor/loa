package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loa/internal/store"
)

func TestRenderTemplateReplacesEveryField(t *testing.T) {
	fields := promptFields{
		ID:       7,
		Title:    "wire the report",
		Body:     "it must count attempts",
		Scopes:   []string{"repo:loa", "repo:nixos"},
		Branch:   "loa/7.1",
		Worktree: "/tmp/worktree",
		Attempt:  2,
		Repo:     "/tmp/repo",
	}
	source := []byte("id={{.ID}} title={{.Title}} body={{.Body}} " +
		"scopes={{range $i, $s := .Scopes}}{{if $i}},{{end}}{{$s}}{{end}} " +
		"branch={{.Branch}} worktree={{.Worktree}} attempt={{.Attempt}} repo={{.Repo}}")

	got, err := renderTemplate("fields.md", source, fields)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "id=7 title=wire the report body=it must count attempts " +
		"scopes=repo:loa,repo:nixos branch=loa/7.1 worktree=/tmp/worktree attempt=2 repo=/tmp/repo"
	if got != want {
		t.Errorf("rendered = %q, want %q", got, want)
	}
}

func TestPromptHashIsStableAndSensitive(t *testing.T) {
	first := promptHash([]byte("same bytes"))
	again := promptHash([]byte("same bytes"))
	changed := promptHash([]byte("same bytez"))
	if first != again {
		t.Errorf("hash changed for identical bytes: %s != %s", first, again)
	}
	if first == changed {
		t.Errorf("hash did not change with the bytes: %s", first)
	}
	if len(first) != 64 {
		t.Errorf("hash = %q, want 64 hex characters", first)
	}
}

func TestMissingPromptFileNamesItsRoleAndFile(t *testing.T) {
	spec, err := specForRole("planner")
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	_, err = readPrompt(t.TempDir(), spec)
	if err == nil {
		t.Fatal("a missing prompt file was accepted")
	}
	for _, want := range []string{"planner", "planner.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestUnknownRoleIsRefused(t *testing.T) {
	if _, err := specForRole("wizard"); err == nil || !strings.Contains(err.Error(), "wizard") {
		t.Errorf("err = %v, want the unknown role named", err)
	}
}

func TestDefaultRoleKeepsTheOldPrompt(t *testing.T) {
	// The implementer file is the old prompt text before the contract, moved to
	// data. Rendering it for a default task must therefore produce the same
	// prompt the runner has always sent.
	source, err := os.ReadFile(filepath.Join("..", "..", "prompts", "implementer.md"))
	if err != nil {
		t.Fatalf("read implementer prompt: %v", err)
	}
	spec, err := specForRole("")
	if err != nil {
		t.Fatalf("default role: %v", err)
	}
	if spec.prompt != "implementer.md" {
		t.Fatalf("default prompt = %q, want implementer.md", spec.prompt)
	}

	task := &store.Task{ID: 1, Title: "add a file", Body: "make it", Attempts: 1}
	got, err := taskPrompt(task, spec.prompt, source,
		fieldsFor(task, "/work/tree", "/repo", "loa/1.1"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	want := `Task #1: add a file

make it

## Where you are

A dedicated git worktree of /repo at /work/tree, created for this task alone, checked out on branch loa/1.1.

## Contract

- Commit your work to the current branch. Anything uncommitted when you exit is discarded, and a run that adds no commit counts as a failure.
- Do not push, do not switch branches, do not touch any other checkout.
- If the task cannot be done as described, say why and stop rather than making unrelated changes.

Finish with a short summary of what changed and what the human must verify.`
	if got != want {
		t.Errorf("prompt = %q\nwant %q", got, want)
	}
}

func TestContractCarriesTheScopes(t *testing.T) {
	task := &store.Task{ID: 2, Title: "scoped", Scopes: []string{"repo:loa", "svc:api"}}
	got := contract(task)
	if !strings.Contains(got, "Stay inside repo:loa, svc:api.") {
		t.Errorf("contract = %q, missing the scopes", got)
	}
}
