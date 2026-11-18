package runner_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"loa/internal/runner"
	"loa/internal/store"
)

// gitRun is for setup on the test goroutine.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitE(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// gitE is safe to call from the backend, which runs on worker goroutines.
func gitE(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// newRepo returns a repository called name with one commit on main.
func newRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)

	gitRun(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	gitRun(t, repo, "config", "user.name", "loa test")
	gitRun(t, repo, "config", "user.email", "test@example.invalid")
	gitRun(t, repo, "config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addTask(t *testing.T, s *store.Store, task store.NewTask) *store.Task {
	t.Helper()
	if task.Workspace == "" {
		task.Workspace = "loa"
	}
	id, err := s.Add(context.Background(), task)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

// fakeBackend is a session stand-in: it makes real git worktrees so commit
// judgement is real, and lets each test decide what the agent does.
type fakeBackend struct {
	mu        sync.Mutex
	repo      string
	worktrees string

	// openErr makes Open fail, as an unresolvable workspace label does.
	openErr error
	// agent runs in the worktree; it returns the text the agent finished with.
	agent func(ctx context.Context, dir string) (string, error)
	// orphans are workspaces the backend reports as existing for tasks.
	orphans []runner.TaskWorkspace
	// onOpen runs at the start of Open, so a test can disturb the store between
	// the lease and the workspace being recorded.
	onOpen func()

	gitMu       sync.Mutex        // serialises worktree creation in the shared repo
	opened      map[string]string // workspace id → worktree path
	closed      map[string]bool
	interrupted []string
	opens       []runner.OpenRequest
	output      map[string]string
}

func newFakeBackend(t *testing.T, repo string) *fakeBackend {
	t.Helper()
	return &fakeBackend{
		repo:      repo,
		worktrees: filepath.Join(t.TempDir(), "worktrees"),
		agent: func(context.Context, string) (string, error) {
			return "", nil
		},
		opened: map[string]string{},
		closed: map[string]bool{},
		output: map[string]string{},
	}
}

func (f *fakeBackend) Label(taskID int64) string { return fmt.Sprintf("loa #%d", taskID) }

func (f *fakeBackend) Open(_ context.Context, req runner.OpenRequest) (runner.Handle, error) {
	f.mu.Lock()
	f.opens = append(f.opens, req)
	err := f.openErr
	hook := f.onOpen
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return runner.Handle{}, err
	}

	base, err := gitE(f.repo, "rev-parse", "--verify", req.Base+"^{commit}")
	if err != nil {
		return runner.Handle{}, err
	}
	path := filepath.Join(f.worktrees, fmt.Sprintf("%d.%d", req.TaskID, req.Attempt))

	// git worktree add writes shared metadata in the repo; two concurrent adds
	// race there. Herdr serialises this for the real backend.
	f.gitMu.Lock()
	_, err = gitE(f.repo, "worktree", "add", "-b", req.Branch, path, base)
	f.gitMu.Unlock()
	if err != nil {
		return runner.Handle{}, err
	}

	workspace := fmt.Sprintf("w%d", req.TaskID)
	f.mu.Lock()
	f.opened[workspace] = path
	f.mu.Unlock()

	return runner.Handle{
		TaskID:      req.TaskID,
		AgentName:   req.AgentName,
		WorkspaceID: workspace,
		PaneID:      workspace + ":p1",
		Worktree:    path,
		Repo:        f.repo,
		Base:        base,
		Branch:      req.Branch,
	}, nil
}

func (f *fakeBackend) Prompt(ctx context.Context, h runner.Handle, _ string, _ time.Duration) error {
	f.mu.Lock()
	agent := f.agent
	f.mu.Unlock()

	out, err := agent(ctx, h.Worktree)
	f.mu.Lock()
	f.output[h.WorkspaceID] = out
	f.mu.Unlock()
	return err
}

func (f *fakeBackend) Read(_ context.Context, h runner.Handle, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.output[h.WorkspaceID], nil
}

func (f *fakeBackend) Interrupt(_ context.Context, h runner.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupted = append(f.interrupted, h.WorkspaceID)
	return nil
}

func (f *fakeBackend) Close(_ context.Context, h runner.Handle) error {
	f.mu.Lock()
	path, ok := f.opened[h.WorkspaceID]
	f.closed[h.WorkspaceID] = true
	f.mu.Unlock()
	if !ok {
		return nil
	}
	_, err := gitE(f.repo, "worktree", "remove", "--force", path)
	return err
}

func (f *fakeBackend) TaskWorkspaces(context.Context) ([]runner.TaskWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orphans, nil
}

func (f *fakeBackend) wasClosed(workspace string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[workspace]
}

func (f *fakeBackend) wasInterrupted(workspace string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.interrupted {
		if id == workspace {
			return true
		}
	}
	return false
}

func newRunner(t *testing.T, s *store.Store, b runner.Backend, mutate func(*runner.Config)) *runner.Runner {
	t.Helper()
	cfg := runner.Config{
		Store:       s,
		Backend:     b,
		Workers:     2,
		MaxTasks:    10,
		LeaseTTL:    time.Hour,
		TaskTimeout: time.Minute,
		Logger:      log.New(io.Discard, "", 0),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := runner.New(cfg)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return r
}

// committer writes a file and commits it, which is what a successful worker
// leaves behind.
func committer(_ *testing.T) func(context.Context, string) (string, error) {
	return func(_ context.Context, dir string) (string, error) {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o600); err != nil {
			return "", err
		}
		if _, err := gitE(dir, "add", "-A"); err != nil {
			return "", err
		}
		if _, err := gitE(dir, "commit", "-q", "-m", "work"); err != nil {
			return "", err
		}
		return "did the thing", nil
	}
}

func TestRunCompletesTaskThatCommits(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = committer(t)
	task := addTask(t, s, store.NewTask{Title: "add a file", Body: "make it"})

	stats, err := newRunner(t, s, b, nil).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats != (runner.Stats{Tasks: 1, Completed: 1}) {
		t.Fatalf("stats = %+v", stats)
	}

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.Done || got.Branch != "loa/1.1" {
		t.Fatalf("task = %+v", got)
	}
	if !strings.Contains(got.Result, "did the thing") {
		t.Errorf("result = %q", got.Result)
	}
	if !b.wasClosed("w1") {
		t.Error("the workspace was left open after a successful task")
	}
	// The branch outlives the workspace; that is the deliverable.
	gitRun(t, b.repo, "rev-parse", "--verify", "loa/1.1")
}

func TestRunFailsWhenNothingIsCommitted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = func(context.Context, string) (string, error) { return "looked around", nil }
	task := addTask(t, s, store.NewTask{Title: "explore"})

	stats, err := newRunner(t, s, b, nil).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.New || !strings.Contains(got.Result, "without committing") {
		t.Errorf("task = %+v", got)
	}
	if !b.wasClosed("w1") {
		t.Error("the workspace was left open after a failed task")
	}
	// An attempt that produced nothing leaves no branch behind.
	if out, _ := gitE(b.repo, "branch", "--list", "loa/*"); strings.TrimSpace(out) != "" {
		t.Errorf("branches left behind: %s", out)
	}
}

func TestUnknownWorkspaceFailsTheTask(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.openErr = errors.New(`no Herdr workspace is labelled "loa"`)
	task := addTask(t, s, store.NewTask{Title: "nowhere", Workspace: "nope"})

	if _, err := newRunner(t, s, b, nil).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(got.Result, "no Herdr workspace is labelled") {
		t.Errorf("result = %q", got.Result)
	}
}

func TestBlockedAgentIsLeftForTheHuman(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = func(context.Context, string) (string, error) {
		return "waiting for approval", runner.ErrBlocked
	}
	task := addTask(t, s, store.NewTask{Title: "needs a decision"})

	stats, err := newRunner(t, s, b, nil).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(got.Result, "w1") || !strings.Contains(got.Result, "w1:p1") {
		t.Errorf("result does not name the pane: %q", got.Result)
	}
	if b.wasClosed("w1") {
		t.Error("a blocked task's workspace was torn down; the human needs it")
	}
	if b.wasInterrupted("w1") {
		t.Error("a blocked agent was interrupted instead of left waiting")
	}
}

func TestPromptFailureStopsAndCloses(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = func(context.Context, string) (string, error) {
		return "", errors.New("timeout after 45000ms")
	}
	task := addTask(t, s, store.NewTask{Title: "slow"})

	if _, err := newRunner(t, s, b, nil).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(got.Result, "timeout") {
		t.Errorf("result = %q", got.Result)
	}
	if !b.wasInterrupted("w1") {
		t.Error("a failed agent was left running")
	}
	if !b.wasClosed("w1") {
		t.Error("a failed workspace was left open")
	}
}

func TestSharedScopeSerialisesExecution(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))

	commit := committer(t)
	var mu sync.Mutex
	active, peak := 0, 0
	b.agent = func(ctx context.Context, dir string) (string, error) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		time.Sleep(20 * time.Millisecond)
		return commit(ctx, dir)
	}

	scope := []string{"repo:demo"}
	addTask(t, s, store.NewTask{Title: "one", Scopes: scope})
	addTask(t, s, store.NewTask{Title: "two", Scopes: scope})

	r := newRunner(t, s, b, func(c *runner.Config) { c.Workers = 4 })
	stats, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Completed != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if peak != 1 {
		t.Errorf("%d tasks sharing a scope ran at once", peak)
	}
}

func TestRoundsPickUpTasksFreedBySiblings(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = committer(t)

	first := addTask(t, s, store.NewTask{Title: "first", Scopes: []string{"repo:demo"}})
	addTask(t, s, store.NewTask{Title: "sibling", Scopes: []string{"repo:demo"}})
	addTask(t, s, store.NewTask{Title: "dependent", Scopes: []string{"repo:demo"}, Deps: []int64{first.ID}})

	r := newRunner(t, s, b, func(c *runner.Config) { c.Workers = 1 })
	stats, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Completed != 3 {
		t.Fatalf("stats = %+v, want all three done", stats)
	}
}

func TestMaxTasksIsABudget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = committer(t)

	for i := 0; i < 5; i++ {
		addTask(t, s, store.NewTask{Title: "task"})
	}

	r := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 2 })
	stats, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Tasks != 2 {
		t.Errorf("ran %d tasks, want 2", stats.Tasks)
	}

	remaining, err := s.List(ctx, store.New)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 3 {
		var detail []string
		for _, task := range remaining {
			detail = append(detail, fmt.Sprintf("#%d %s: %s", task.ID, task.Status, task.Result))
		}
		t.Errorf("%d tasks left, want 3 (completed %d): %s", len(remaining), stats.Completed, strings.Join(detail, "; "))
	}
}

func TestLeaseMustOutlastTimeout(t *testing.T) {
	_, err := runner.New(runner.Config{
		Store:       newStore(t),
		Backend:     newFakeBackend(t, newRepo(t, "demo")),
		LeaseTTL:    time.Minute,
		TaskTimeout: time.Hour,
	})
	if err == nil {
		t.Fatal("a lease shorter than the task timeout was accepted")
	}
}

func TestCancelledRunClosesTheWorkspaceAndLeavesTheLease(t *testing.T) {
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))

	started := make(chan struct{})
	b.agent = func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	task := addTask(t, s, store.NewTask{Title: "interrupted"})

	r := newRunner(t, s, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := r.Run(ctx); err == nil {
		t.Fatal("a cancelled run reported success")
	}

	if !b.wasClosed("w1") {
		t.Error("the workspace outlived the run")
	}
	got, err := s.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.Leased {
		t.Errorf("status = %s, want leased", got.Status)
	}
}

func TestLostLeaseDoesNotClobberTheNewOwner(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	task := addTask(t, s, store.NewTask{Title: "contested"})

	commit := committer(t)
	b.agent = func(ctx context.Context, dir string) (string, error) {
		if err := s.Fail(ctx, task.ID, "reclaimed by a newer run"); err != nil {
			return "", err
		}
		return commit(ctx, dir)
	}

	stats, err := newRunner(t, s, b, nil).Run(ctx)
	if err != nil {
		t.Fatalf("a lost lease failed the whole run: %v", err)
	}
	if stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Result != "reclaimed by a newer run" {
		t.Errorf("the runner overwrote the new owner's result: %+v", got)
	}
}

func TestDispatchRecordsTheWorkspaceItOwns(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = committer(t)
	task := addTask(t, s, store.NewTask{Title: "add a file"})

	if _, err := newRunner(t, s, b, nil).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Recorded at dispatch and kept after finishing: it is where the attempt ran.
	if got.WorkspaceID != "w1" || got.PaneID != "w1:p1" {
		t.Errorf("ids = %q/%q, want w1/w1:p1", got.WorkspaceID, got.PaneID)
	}
}

func TestUnrecordableWorkspaceIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	task := addTask(t, s, store.NewTask{Title: "contested"})

	// Someone else takes the task between the lease and the recording, so the
	// write is refused and the workspace belongs to nobody this run can name.
	b.onOpen = func() {
		if err := s.Fail(ctx, task.ID, "taken by a newer run"); err != nil {
			t.Errorf("fail: %v", err)
		}
	}
	b.agent = committer(t)

	stats, err := newRunner(t, s, b, nil).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Failed != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if b.wasClosed("w1") {
		t.Error("a workspace whose id could not be recorded was closed; it is the only evidence left")
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorkspaceID != "" {
		t.Errorf("workspace id = %q, want it unrecorded", got.WorkspaceID)
	}
}

func TestReconcileClosesOnlyWhatItOwns(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	type setup func(t *testing.T, s *store.Store, b *fakeBackend)
	cases := map[string]struct {
		setup  setup
		closed bool
	}{
		"recorded and finished": {
			closed: true,
			setup: func(t *testing.T, s *store.Store, b *fakeBackend) {
				id := addTask(t, s, store.NewTask{Title: "done"}).ID
				if _, err := s.Lease(ctx, "other", time.Hour, now); err != nil {
					t.Fatalf("lease: %v", err)
				}
				if err := s.Attach(ctx, id, "wOwned", "wOwned:p1"); err != nil {
					t.Fatalf("attach: %v", err)
				}
				if err := s.Complete(ctx, id, "b/x", "done"); err != nil {
					t.Fatalf("complete: %v", err)
				}
				b.orphans = []runner.TaskWorkspace{{TaskID: id, WorkspaceID: "wOwned"}}
			},
		},
		"recorded and held": {
			closed: false,
			setup: func(t *testing.T, s *store.Store, b *fakeBackend) {
				id := addTask(t, s, store.NewTask{Title: "running"}).ID
				if _, err := s.Lease(ctx, "other", time.Hour, now); err != nil {
					t.Fatalf("lease: %v", err)
				}
				if err := s.Attach(ctx, id, "wLive", "wLive:p1"); err != nil {
					t.Fatalf("attach: %v", err)
				}
				b.orphans = []runner.TaskWorkspace{{TaskID: id, WorkspaceID: "wLive"}}
			},
		},
		"never recorded, task idle": {
			closed: true,
			setup: func(t *testing.T, s *store.Store, b *fakeBackend) {
				id := addTask(t, s, store.NewTask{Title: "orphan"}).ID
				b.orphans = []runner.TaskWorkspace{{TaskID: id, WorkspaceID: "wLost"}}
			},
		},
		"never recorded, task held": {
			closed: false,
			setup: func(t *testing.T, s *store.Store, b *fakeBackend) {
				id := addTask(t, s, store.NewTask{Title: "starting"}).ID
				if _, err := s.Lease(ctx, "other", time.Hour, now); err != nil {
					t.Fatalf("lease: %v", err)
				}
				// The other runner created the workspace and has not recorded it yet.
				b.orphans = []runner.TaskWorkspace{{TaskID: id, WorkspaceID: "wFresh"}}
			},
		},
		"recorded against a different task": {
			closed: false,
			setup: func(t *testing.T, s *store.Store, b *fakeBackend) {
				mine := addTask(t, s, store.NewTask{Title: "mine"}).ID
				theirs := addTask(t, s, store.NewTask{Title: "theirs"}).ID
				if _, err := s.Lease(ctx, "other", time.Hour, now); err != nil {
					t.Fatalf("lease: %v", err)
				}
				if err := s.Attach(ctx, mine, "wMine", "wMine:p1"); err != nil {
					t.Fatalf("attach: %v", err)
				}
				// A workspace labelled for another task carries my id: not mine to remove.
				b.orphans = []runner.TaskWorkspace{{TaskID: theirs, WorkspaceID: "wMine"}}
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			b := newFakeBackend(t, newRepo(t, "demo"))
			tc.setup(t, s, b)

			// One task at most is claimable here; the point is reconciliation, which
			// runs before any lease.
			r := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 })
			if _, err := r.Run(ctx); err != nil {
				t.Fatalf("run: %v", err)
			}
			closed := false
			for _, space := range b.orphans {
				if b.wasClosed(space.WorkspaceID) {
					closed = true
				}
			}
			if closed != tc.closed {
				t.Errorf("closed = %v, want %v", closed, tc.closed)
			}
		})
	}
}
