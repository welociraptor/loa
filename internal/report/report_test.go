package report_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"context"

	"taskflow/internal/herdr"
	"taskflow/internal/live"
	"taskflow/internal/report"
	"taskflow/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type noSession struct{}

func (noSession) Workspaces(context.Context) ([]herdr.Workspace, error) { return nil, nil }
func (noSession) Agents(context.Context) ([]herdr.Agent, error)         { return nil, nil }

func addTask(t *testing.T, s *store.Store, task store.NewTask) int64 {
	t.Helper()
	if task.Workspace == "" {
		task.Workspace = "nixos"
	}
	id, err := s.Add(context.Background(), task)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return id
}

// day builds one of each outcome: a finished task, a blocked one, a queued one,
// and a task whose workspace is still open.
func day(t *testing.T) live.Snapshot {
	t.Helper()
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()

	done := addTask(t, s, store.NewTask{Title: "wire the report", Priority: 3})
	blocked := addTask(t, s, store.NewTask{Title: "the timer question", Priority: 2, MaxTries: 1})
	addTask(t, s, store.NewTask{Title: "bump the flake", Priority: 1})

	if _, err := s.Lease(ctx, "run", time.Hour, now); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.Attach(ctx, done, "w1", "w1:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// A measurable attempt, so the run time column means something.
	time.Sleep(10 * time.Millisecond)
	if err := s.Complete(ctx, done, "taskflow/1.1", "wrote the report"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := s.Lease(ctx, "run", time.Hour, now); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.Attach(ctx, blocked, "w2", "w2:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.Fail(ctx, blocked, "needs a decision:\n  which timer?"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Something is still running: its workspace shows up as open.
	open := addTask(t, s, store.NewTask{Title: "half done", Priority: 9})
	if _, err := s.Lease(ctx, "other", 4*time.Hour, now); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := s.Attach(ctx, open, "w3", "w3:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	snap, err := live.Read(ctx, s, fakeSession{open: open})
	if err != nil {
		t.Fatalf("live read: %v", err)
	}
	return snap
}

type fakeSession struct{ open int64 }

func (f fakeSession) Workspaces(context.Context) ([]herdr.Workspace, error) {
	return []herdr.Workspace{{ID: "w3", Label: herdr.TaskLabel(f.open)}}, nil
}

func (f fakeSession) Agents(context.Context) ([]herdr.Agent, error) {
	return []herdr.Agent{{Name: "tf-4-1", Status: "working", WorkspaceID: "w3"}}, nil
}

func TestBuildGroupsTheDay(t *testing.T) {
	snap := day(t)
	now := time.Now()
	r := report.Build(snap, now.Add(-24*time.Hour), now)

	if len(r.Done) != 1 || r.Done[0].Branch != "taskflow/1.1" {
		t.Fatalf("done = %+v", r.Done)
	}
	if len(r.Blocked) != 1 || !strings.Contains(r.Blocked[0].Result, "which timer") {
		t.Fatalf("blocked = %+v", r.Blocked)
	}
	if len(r.Queued) != 1 || r.Queued[0].Title != "bump the flake" {
		t.Fatalf("queued = %+v", r.Queued)
	}
	// The queued tasks were added to the queue, but only one is the day's work.
	if len(r.Open) != 1 || r.Open[0].Session != "w3 working tf-4-1" {
		t.Fatalf("open = %+v", r.Open)
	}
}

func TestTextRendersEverySectionAndTheGap(t *testing.T) {
	snap := day(t)
	// An afternoon later, so the gap has something to say.
	now := time.Now().Add(2 * time.Hour)
	text := report.Build(snap, now.Add(-24*time.Hour), now).Text()

	for _, want := range []string{
		"since ",
		"done: 1",
		"taskflow/1.1",
		"wrote the report",
		"needs a human: 1",
		"needs a decision: which timer?", // the failure reason is flattened to a line
		"queued: 1",
		"still open: 1",
		"w3 working tf-4-1",
		"nothing has moved since",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
}

func TestAnEmptyQueueStillStatesTheGap(t *testing.T) {
	snap, err := live.Read(context.Background(), newStore(t), noSession{})
	if err != nil {
		t.Fatalf("live read: %v", err)
	}
	now := time.Now()
	text := report.Build(snap, now.Add(-24*time.Hour), now).Text()

	if strings.Contains(text, "done:") || strings.Contains(text, "queued:") {
		t.Errorf("an empty queue grew a section:\n%s", text)
	}
	if !strings.Contains(text, "the queue has never been worked") {
		t.Errorf("an empty queue said nothing about itself:\n%s", text)
	}
}

func TestWindowExcludesOlderWorkButNotOpenWorkspaces(t *testing.T) {
	snap := day(t)
	now := time.Now()

	// Everything happened inside this window...
	if r := report.Build(snap, now.Add(-time.Hour), now); len(r.Done) != 1 || len(r.Blocked) != 1 {
		t.Fatalf("recent window = %+v", r)
	}
	// ...and nothing except the still-open workspace survives an empty one.
	r := report.Build(snap, now.Add(time.Hour), now)
	if len(r.Done) != 0 || len(r.Blocked) != 0 {
		t.Errorf("a future window reported work: %+v", r)
	}
	if len(r.Open) != 1 {
		t.Errorf("open workspaces were filtered by the window: %+v", r.Open)
	}
}
