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

func TestPlanPromptCallsThePlanADependencyGraph(t *testing.T) {
	prompt := triage.PlanPrompt("ship the report", []string{"plan", "implement", "review"})
	for _, want := range []string{
		"ship the report",
		"dependency graph, not prose",
		"plan.json",
		`"phases"`,
		"barrier",
		"plan, implement, review",
		"Do not use bash",
		"Do not apply anything",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestReadPlanRequiresPhasesAndValidTasks(t *testing.T) {
	dir := t.TempDir()
	valid := `{"phases":[{"name":"one","tasks":[{"title":"a","workspace":"demo"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(valid), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	doc, err := triage.ReadPlan(dir)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if len(doc.Phases) != 1 || doc.Phases[0].Tasks[0].Title != "a" {
		t.Fatalf("plan = %+v", doc)
	}

	cases := map[string]string{
		"missing":       ``,
		"invalid":       `{"phases":[{"name":"one","tasks":[{"title":"a"}]}]}`,
		"no phases":     `{"tasks":[{"title":"a","workspace":"demo"}]}`,
		"unknown field": `{"phases":[{"name":"one","owner":"me","tasks":[]}]}`,
	}
	for name, body := range cases {
		sub := t.TempDir()
		if body != "" {
			if err := os.WriteFile(filepath.Join(sub, "plan.json"), []byte(body), 0o600); err != nil {
				t.Fatalf("%s: write: %v", name, err)
			}
		}
		if _, err := triage.ReadPlan(sub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSavePlanKeepsAPlanWithTheProposals(t *testing.T) {
	dir := t.TempDir()
	doc := taskspec.Document{Phases: []taskspec.Phase{{
		Name:  "one",
		Tasks: []taskspec.Task{{Title: "a", Workspace: "demo"}},
	}}}
	path, err := triage.SavePlan(dir, doc, time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if filepath.Base(path) != "plan-20261118T093000Z.json" {
		t.Errorf("path = %s", path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from the function under test
	if err != nil {
		t.Fatalf("read saved plan: %v", err)
	}
	parsed, err := taskspec.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse saved plan: %v", err)
	}
	applied, err := taskspec.Validate(parsed)
	if err != nil {
		t.Fatalf("validate saved plan: %v", err)
	}
	if len(applied.Tasks) != 1 || applied.Tasks[0].Title != "a" {
		t.Errorf("saved plan = %+v", applied)
	}
}
