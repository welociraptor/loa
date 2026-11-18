package live_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"loa/internal/herdr"
	"loa/internal/live"
	"loa/internal/store"
)

type fakeSession struct {
	workspaces []herdr.Workspace
	agents     []herdr.Agent
	err        error
}

func (f fakeSession) Workspaces(context.Context) ([]herdr.Workspace, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.workspaces, nil
}

func (f fakeSession) Agents(context.Context) ([]herdr.Agent, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.agents, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addTask(t *testing.T, s *store.Store, task store.NewTask) int64 {
	t.Helper()
	task.Workspace = "nixos"
	id, err := s.Add(context.Background(), task)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return id
}

// leaseAndRecord takes the task and records the workspace, as dispatch does.
func leaseAndRecord(t *testing.T, s *store.Store, taskID int64, workspace string) {
	t.Helper()
	got, err := s.Lease(context.Background(), "test", time.Hour, time.Now())
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if got == nil || got.ID != taskID {
		t.Fatalf("leased %v, want %d", got, taskID)
	}
	if err := s.Attach(context.Background(), taskID, workspace, workspace+":p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
}

func rowFor(t *testing.T, snap live.Snapshot, taskID int64) live.Row {
	t.Helper()
	for _, row := range snap.Rows {
		if row.Task.ID == taskID {
			return row
		}
	}
	t.Fatalf("no row for task %d", taskID)
	return live.Row{}
}

func TestReadJoinsTasksWithTheirSession(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	blocked := addTask(t, s, store.NewTask{Title: "needs a decision", MaxTries: 1})
	gone := addTask(t, s, store.NewTask{Title: "agent died", MaxTries: 1})
	closed := addTask(t, s, store.NewTask{Title: "cleaned up", MaxTries: 1})
	waiting := addTask(t, s, store.NewTask{Title: "not started"})

	leaseAndRecord(t, s, blocked, "w1")
	leaseAndRecord(t, s, gone, "w2")
	leaseAndRecord(t, s, closed, "w3")
	if err := s.Complete(ctx, closed, "b/x", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	session := fakeSession{
		workspaces: []herdr.Workspace{
			{ID: "w1", Label: herdr.TaskLabel(blocked)},
			{ID: "w2", Label: herdr.TaskLabel(gone)},
		},
		agents: []herdr.Agent{
			{Name: "loa-1-1", Status: "blocked", WorkspaceID: "w1"},
		},
	}

	snap, err := live.Read(ctx, s, session)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if snap.Unavailable != "" {
		t.Fatalf("unavailable: %s", snap.Unavailable)
	}

	if got := rowFor(t, snap, blocked); got.AgentName != "loa-1-1" || got.AgentState != "blocked" ||
		got.WorkspaceState != live.WorkspaceLive {
		t.Errorf("blocked row = %+v", got)
	}
	// The workspace outlives its agent, and that is the state worth showing.
	if got := rowFor(t, snap, gone); got.WorkspaceID != "w2" || got.AgentState != "" ||
		got.WorkspaceState != live.WorkspaceLive {
		t.Errorf("no-agent row = %+v", got)
	}
	if got := rowFor(t, snap, closed); got.WorkspaceState != live.WorkspaceClosed {
		t.Errorf("closed row = %+v", got)
	}
	if got := rowFor(t, snap, waiting); got.WorkspaceState != live.WorkspaceNone {
		t.Errorf("untouched row = %+v", got)
	}
}

func TestReadFindsAWorkspaceByLabelWhenNothingIsRecorded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	task := addTask(t, s, store.NewTask{Title: "crashed mid-dispatch"})
	// Leased but never recorded: the window reconciliation and `ps` both care about.
	if _, err := s.Lease(ctx, "dead-runner", 2*time.Hour, time.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}

	session := fakeSession{
		workspaces: []herdr.Workspace{{ID: "w9", Label: herdr.TaskLabel(task)}},
	}
	snap, err := live.Read(ctx, s, session)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	row := rowFor(t, snap, task)
	if row.WorkspaceID != "w9" || row.WorkspaceState != live.WorkspaceUntracked {
		t.Errorf("row = %+v", row)
	}
	if len(snap.Orphans) != 0 {
		t.Errorf("a labelled workspace matched to its task was also reported as an orphan: %+v", snap.Orphans)
	}
}

func TestReadReportsOrphanWorkspaces(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// A workspace labelled for a task that is not in the queue at all: nothing
	// accounts for it, so it is reported rather than attributed to anything.
	session := fakeSession{
		workspaces: []herdr.Workspace{
			{ID: "w4", Label: herdr.TaskLabel(999)},
			{ID: "w5", Label: "someone-elses"},
		},
	}
	snap, err := live.Read(ctx, s, session)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(snap.Orphans) != 1 || snap.Orphans[0].WorkspaceID != "w4" || snap.Orphans[0].TaskID != 999 {
		t.Fatalf("orphans = %+v", snap.Orphans)
	}
}

func TestReadDegradesWhenTheSessionIsUnreadable(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	task := addTask(t, s, store.NewTask{Title: "queued"})

	snap, err := live.Read(ctx, s, fakeSession{err: errors.New("no socket")})
	if err != nil {
		t.Fatalf("a missing session failed the read: %v", err)
	}
	if snap.Unavailable == "" {
		t.Error("the snapshot claims the session was read")
	}
	if len(snap.Rows) != 1 || snap.Rows[0].Task.ID != task {
		t.Errorf("the store half was lost: %+v", snap.Rows)
	}
	if snap.Rows[0].WorkspaceState != live.WorkspaceNone {
		t.Errorf("state = %q, want none", snap.Rows[0].WorkspaceState)
	}
}

func TestSortPutsLiveWorkFirst(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	done := addTask(t, s, store.NewTask{Title: "done", Priority: 9})
	leaseAndRecord(t, s, done, "wD")
	if err := s.Complete(ctx, done, "b/x", "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	blocked := addTask(t, s, store.NewTask{Title: "blocked", MaxTries: 1})
	leaseAndRecord(t, s, blocked, "wB")
	if err := s.Fail(ctx, blocked, "no"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	running := addTask(t, s, store.NewTask{Title: "running", MaxTries: 1})
	leaseAndRecord(t, s, running, "wR")

	snap, err := live.Read(ctx, s, fakeSession{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	order := []store.Status{}
	for _, row := range snap.Rows {
		order = append(order, row.Task.Status)
	}
	want := []store.Status{store.Leased, store.Blocked, store.Done}
	if len(order) != len(want) {
		t.Fatalf("rows = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
