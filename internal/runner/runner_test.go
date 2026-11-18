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

	"taskflow/internal/runner"
	"taskflow/internal/store"
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
	gitRun(t, repo, "config", "user.name", "taskflow test")
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
		task.Workspace = "taskflow"
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
		worktrees: filepath.Join(t.TempDir(), "unused"),
		agent: func(context.Context, string) (string, error) {
			return "", nil
		},
		opened: map[string]string{},
		closed: map[string]bool{},
		output: map[string]string{},
	}
}

func (f *fakeBackend) Label(taskID int64) string { return fmt.Sprintf("taskflow #%d", taskID) }

func (f *fakeBackend) Open(_ context.Context, req runner.OpenRequest) (runner.Handle, error) {
	f.mu.Lock()
	f.opens = append(f.opens, req)
	err := f.openErr
	f.mu.Unlock()
	if err != nil {
		return runner.Handle{}, err
	}

	base, err := gitE(f.repo, "rev-parse", "--verify", req.Base+"^{commit}")
	if err != nil {
		return runner.Handle{}, err
	}
	path := req.WorktreePath
	if _, err := gitE(f.repo, "worktree", "add", "-b", req.Branch, path, base); err != nil {
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
		Worktrees:   filepath.Join(t.TempDir(), "worktrees"),
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
	if got.Status != store.Done || got.Branch != "taskflow/1.1" {
		t.Fatalf("task = %+v", got)
	}
	if !strings.Contains(got.Result, "did the thing") {
		t.Errorf("result = %q", got.Result)
	}
	if !b.wasClosed("w1") {
		t.Error("the workspace was left open after a successful task")
	}
	// The branch outlives the workspace; that is the deliverable.
	gitRun(t, b.repo, "rev-parse", "--verify", "taskflow/1.1")
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
	if out, _ := gitE(b.repo, "branch", "--list", "taskflow/*"); strings.TrimSpace(out) != "" {
		t.Errorf("branches left behind: %s", out)
	}
}

func TestUnknownWorkspaceFailsTheTask(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.openErr = errors.New(`no Herdr workspace is labelled "taskflow"`)
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
		t.Errorf("%d tasks left, want 3", len(remaining))
	}
}

func TestLeaseMustOutlastTimeout(t *testing.T) {
	_, err := runner.New(runner.Config{
		Store:       newStore(t),
		Backend:     newFakeBackend(t, newRepo(t, "demo")),
		Worktrees:   t.TempDir(),
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

func TestReconcileClosesOrphanWorkspacesOnly(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	b.agent = committer(t)

	live := addTask(t, s, store.NewTask{Title: "live", Scopes: []string{"repo:a"}})
	// One workspace for a task that is gone, one for a task that is currently
	// leased to the run about to start.
	b.orphans = []runner.TaskWorkspace{
		{TaskID: 999, WorkspaceID: "wDead"},
		{TaskID: live.ID, WorkspaceID: "wLive"},
	}
	b.opened["wDead"] = filepath.Join(b.worktrees, "dead")

	// The live task's workspace has to look leased before the run starts.
	if _, err := s.Lease(ctx, "someone", time.Hour, time.Now()); err != nil {
		t.Fatalf("lease: %v", err)
	}

	r := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1; c.Workers = 1 })
	if _, err := r.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !b.wasClosed("wDead") {
		t.Error("an orphaned workspace was left open")
	}
	if b.wasClosed("wLive") {
		t.Error("a workspace for a leased task was closed")
	}
}

// A pane is a screen, not a transcript: the stored result has to be the agent's
// own words, not the TUI around them.
func TestSummaryKeepsTheAgentsWords(t *testing.T) {
	screen := strings.Join([]string{
		"─────────────────────────────────────────",
		"[Skills]",
		"  herdr",
		"",
		" Task #2: add note.txt",
		"",
		" Created note.txt and committed it.",
		" Verify: git show 745c4b1",
		"",
		"─────────────────────────────────────────",
		"↑6.4k ↓326 $0.001 deepseek/deepseek-v4.1-flash • medium",
	}, "\n")

	got := runner.SummaryOf(screen, 4)
	if strings.Contains(got, "───") {
		t.Errorf("summary kept box drawing: %q", got)
	}
	if !strings.Contains(got, "Verify: git show") {
		t.Errorf("summary = %q, want the tail of the transcript", got)
	}
}
