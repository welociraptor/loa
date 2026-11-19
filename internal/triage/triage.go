// Package triage prepares what a triage agent is given, and reads back what it
// proposes. It never writes to the queue: a proposal is a document that a person
// applies with `loa import`.
package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loa/internal/store"
	"loa/internal/taskspec"
)

// Queue is the queue as the agent sees it: what exists, what state it is in, and
// what waits on what. Bodies are included so "this task is too big" is a
// judgement it can actually make.
type Queue struct {
	Now   time.Time `json:"now"`
	Tasks []Item    `json:"tasks"`
}

// Item is one task in the triage context.
type Item struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Body        string   `json:"body,omitempty"`
	Workspace   string   `json:"workspace"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Attempts    int      `json:"attempts"`
	MaxAttempts int      `json:"max_attempts"`
	Scopes      []string `json:"scopes,omitempty"`
	Result      string   `json:"result,omitempty"`
	Deps        []Dep    `json:"deps,omitempty"`
}

// Dep is a task this one waits on.
type Dep struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// Context writes the queue into dir and returns the file the agent should read.
func Context(dir string, q Queue) (string, error) {
	raw, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return "", fmt.Errorf("triage: encode queue: %w", err)
	}
	path := filepath.Join(dir, "queue.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("triage: write queue: %w", err)
	}
	return path, nil
}

// Prompt is what the agent is asked to do. It names the one file it may write
// and the one thing it may never change.
func Prompt(queuePath string) string {
	return fmt.Sprintf(`You are triaging a task queue. The queue is in %s.

Read it, then propose changes. Write exactly one file in this directory,
proposal.json, with this shape:

{
  "tasks": [
    {"title": "...", "body": "...", "workspace": "...", "priority": 0,
     "max_attempts": 2, "scopes": ["repo:x"], "deps": [12], "after": ["another title in this file"]}
  ],
  "reprioritise": [{"id": 4, "priority": 9}],
  "notes": ["anything a human must read, one per string"]
}

What to do:
- Split a task whose body describes more than one deliverable. Propose the
  parts as new tasks with "after" naming each other, and leave the original
  alone: a person decides what to do with it.
- Raise the priority of work that unblocks other work. Use "deps" to see what
  waits on what.
- Use "notes" for what needs a human: an ambiguous task, a blocked task that
  looks abandoned, a task with no workspace.
- Repeating an existing task is noise. If the queue is fine, propose nothing.

What not to do:
- Never rewrite an existing task's body or title. You cannot change them; a
  proposal that tries is discarded.
- Do not apply anything. A person applies this file.
- Do not use bash. Write the one file and stop.

Finish with a short summary of what you proposed and why.`, queuePath)
}

// Read loads and validates the proposal the agent wrote. An invalid proposal is
// an error, not something to apply partially.
func Read(dir string) (taskspec.Document, error) {
	path := filepath.Join(dir, "proposal.json")
	file, err := os.Open(path) //nolint:gosec // G304: dir is the scratch directory this run created
	if err != nil {
		return taskspec.Document{}, fmt.Errorf("triage: no proposal was written (%w)", err)
	}
	defer func() { _ = file.Close() }()

	doc, err := taskspec.Parse(file)
	if err != nil {
		return taskspec.Document{}, err
	}
	if _, err := taskspec.Validate(doc); err != nil {
		return taskspec.Document{}, fmt.Errorf("triage: the proposal is invalid: %w", err)
	}
	return doc, nil
}

// Save copies a validated proposal to the proposals directory and returns its
// path, so a person can read it later and apply it deliberately.
func Save(dir string, doc taskspec.Document, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("triage: proposals dir: %w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("triage: encode proposal: %w", err)
	}
	path := filepath.Join(dir, now.UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("triage: write proposal: %w", err)
	}
	return path, nil
}

// Build turns the store into the agent's context.
func Build(now time.Time, tasks []store.Task, deps map[int64][]store.Dep) Queue {
	q := Queue{Now: now.UTC()}
	for _, task := range tasks {
		item := Item{
			ID: task.ID, Title: task.Title, Body: task.Body, Workspace: task.Workspace,
			Status: string(task.Status), Priority: task.Priority,
			Attempts: task.Attempts, MaxAttempts: task.MaxAttempts,
			Scopes: task.Scopes, Result: task.Result,
		}
		for _, dep := range deps[task.ID] {
			item.Deps = append(item.Deps, Dep{ID: dep.ID, Status: string(dep.Status)})
		}
		q.Tasks = append(q.Tasks, item)
	}
	return q
}

// PlanPrompt is what a planner agent is asked. Unlike triage, there is no queue
// to read: the goal is the input and the plan is the whole output.
func PlanPrompt(goal string, stages []string) string {
	var stageLine string
	if len(stages) > 0 {
		stageLine = fmt.Sprintf("\nThe workflow's stages are: %s. Give each task the role of the stage it belongs to.\n",
			strings.Join(stages, ", "))
	}
	return fmt.Sprintf(`You are planning work toward a goal.

The goal:
%s
%s
Your output is a dependency graph, not prose. Do not describe the plan in
sentences; write it as tasks and edges. Write exactly one file in this
directory, plan.json, with this shape:

{
  "phases": [
    {"name": "phase name", "after": "another phase name", "tasks": [
      {"title": "...", "body": "what done looks like", "workspace": "label",
       "role": "implement", "priority": 0, "max_attempts": 2,
       "scopes": ["repo:x"], "after": ["another task title in this file"]}
    ]}
  ]
}

How the shape is read:
- Phases run in the order they appear. Every task in a phase waits for every
  task in the previous phase: a phase is a barrier. Tasks inside one phase run
  in parallel, so do not add dependencies between them.
- "after" on a phase names an earlier phase that this one must also wait for.
  Use it when the phase order alone is not the dependency you mean.
- "after" inside a task names another task in this file, by title.
- Every task needs a title, a body that says what done looks like, and a
  workspace label. Keep the plan inside the goal; do not invent work.
- Do not repeat the previous phase's edges as task dependencies. The compiler
  already adds the barrier.

What not to do:
- Do not use bash. Write the one file and stop.
- Do not apply anything. A person imports the plan.

Finish with a short summary of the phases and why each is one phase.`, goal, stageLine)
}

// ReadPlan loads and validates the plan the planner wrote. An invalid plan is
// an error, not something to apply partially.
func ReadPlan(dir string) (taskspec.Document, error) {
	path := filepath.Join(dir, "plan.json")
	file, err := os.Open(path) //nolint:gosec // G304: dir is the scratch directory this run created
	if err != nil {
		return taskspec.Document{}, fmt.Errorf("plan: no plan was written (%w)", err)
	}
	defer func() { _ = file.Close() }()

	doc, err := taskspec.Parse(file)
	if err != nil {
		return taskspec.Document{}, err
	}
	if _, err := taskspec.Validate(doc); err != nil {
		return taskspec.Document{}, fmt.Errorf("plan: the plan is invalid: %w", err)
	}
	if len(doc.Phases) == 0 {
		return taskspec.Document{}, errors.New("plan: the plan has no phases, so it is a proposal rather than a plan")
	}
	return doc, nil
}

// SavePlan keeps a validated plan with the proposals. The name says it is a
// plan, so a person can tell it from a triage proposal in the same directory.
func SavePlan(dir string, doc taskspec.Document, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("plan: proposals dir: %w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("plan: encode plan: %w", err)
	}
	path := filepath.Join(dir, "plan-"+now.UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("plan: write plan: %w", err)
	}
	return path, nil
}
