package triage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loa/internal/store"
	"loa/internal/taskspec"
	"loa/internal/triage"
)

func TestContextWritesTheQueueTheAgentReads(t *testing.T) {
	dir := t.TempDir()
	q := triage.Build(time.Now(), []store.Task{
		{ID: 1, Title: "schema", Workspace: "nixos", Status: store.Done},
		{ID: 2, Title: "writer", Workspace: "nixos", Status: store.New, Body: "do the thing"},
	}, map[int64][]store.Dep{2: {{ID: 1, Status: store.Done}}})

	path, err := triage.Context(dir, q)
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from the function under test
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	for _, want := range []string{`"writer"`, `"do the thing"`, `"deps"`, `"done"`} {
		if !strings.Contains(text, want) {
			t.Errorf("queue.json is missing %s:\n%s", want, text)
		}
	}
}

func TestPromptNamesTheFileAndTheLimits(t *testing.T) {
	prompt := triage.Prompt("/tmp/triage/queue.json")
	for _, want := range []string{
		"/tmp/triage/queue.json",
		"proposal.json",
		`"reprioritise"`,
		"Never rewrite an existing task's body",
		"Do not use bash",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestReadRejectsAnInvalidProposal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "proposal.json"),
		[]byte(`{"tasks":[{"title":"no workspace here"}]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := triage.Read(dir); err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsAMissingProposal(t *testing.T) {
	if _, err := triage.Read(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no proposal was written") {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveKeepsTheProposalForReview(t *testing.T) {
	dir := t.TempDir()
	doc, err := taskspecParse(`{"tasks":[{"title":"a","workspace":"nixos"}],"notes":["#3 is ambiguous"]}`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	path, err := triage.Save(dir, doc, time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if filepath.Base(path) != "20261118T093000Z.json" {
		t.Errorf("path = %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("stat: %v", err)
	}
}

func taskspecParse(doc string) (taskspec.Document, error) {
	parsed, err := taskspec.Parse(strings.NewReader(doc))
	if err != nil {
		return taskspec.Document{}, err
	}
	return parsed, nil
}
