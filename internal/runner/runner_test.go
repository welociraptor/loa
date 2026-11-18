package runner_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"taskflow/internal/runner"
	"taskflow/internal/store"
)

type fakeAgent struct {
	fn func(ctx context.Context, dir, prompt string) (string, error)
}

func (f fakeAgent) Run(ctx context.Context, dir, prompt string) (string, error) {
	return f.fn(ctx, dir, prompt)
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// newRepo returns a parent directory holding a repository called name with one
// commit on main.
func newRepo(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, name)

	runCmd(t, root, "git", "init", "-q", "-b", "main", repo)
	runCmd(t, repo, "git", "config", "user.name", "taskflow test")
	runCmd(t, repo, "git", "config", "user.email", "test@example.invalid")
	runCmd(t, repo, "git", "config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	runCmd(t, repo, "git", "add", "-A")
	runCmd(t, repo, "git", "commit", "-q", "-m", "init")
	return root
}

func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	runCmd(t, dir, "git", "add", "-A")
	runCmd(t, dir, "git", "commit", "-q", "-m", message)
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

func newRunner(t *testing.T, s *store.Store, reposDir string, agent runner.Agent, mutate func(*runner.Config)) *runner.Runner {
	t.Helper()
	cfg := runner.Config{
		Store:       s,
		Agent:       agent,
		ReposDir:    reposDir,
		WorktreeDir: filepath.Join(t.TempDir(), "worktrees"),
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

func addTask(t *testing.T, s *store.Store, task store.NewTask) *store.Task {
	t.Helper()
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

func TestRunCompletesTaskThatCommits(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)

	var sawDir, sawPrompt string
	agent := fakeAgent{fn: func(_ context.Context, dir, p string) (string, error) {
		sawDir, sawPrompt = dir, p
		if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("done\n"), 0o600); err != nil {
			return "", err
		}
		commitAll(t, dir, "add out.txt")
		return "wrote out.txt", nil
	}}

	task := addTask(t, s, store.NewTask{Title: "add out.txt", Body: "make the file", Scopes: []string{"repo:demo"}})
	r := newRunner(t, s, repos, agent, nil)

	stats, err := r.Run(ctx)
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
	if got.Status != store.Done {
		t.Fatalf("status = %s, want done", got.Status)
	}
	if got.Branch != "taskflow/1.1" {
		t.Errorf("branch = %q, want taskflow/1.1", got.Branch)
	}
	if !strings.Contains(got.Result, "wrote out.txt") {
		t.Errorf("result = %q", got.Result)
	}

	if !strings.Contains(sawPrompt, "add out.txt") || !strings.Contains(sawPrompt, sawDir) {
		t.Errorf("prompt did not describe the task and its worktree: %q", sawPrompt)
	}
	// The agent works in its own checkout, and the branch outlives it.
	if filepath.Dir(sawDir) == repos {
		t.Errorf("agent ran in the main checkout: %s", sawDir)
	}
	if _, err := os.Stat(sawDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree %s was not removed", sawDir)
	}
	runCmd(t, filepath.Join(repos, "demo"), "git", "rev-parse", "--verify", "taskflow/1.1")
}

func TestRunFailsWhenNothingIsCommitted(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)

	agent := fakeAgent{fn: func(context.Context, string, string) (string, error) {
		return "I had a look around and changed nothing", nil
	}}
	task := addTask(t, s, store.NewTask{Title: "explore", Scopes: []string{"repo:demo"}})

	if _, err := newRunner(t, s, repos, agent, nil).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.New {
		t.Errorf("status = %s, want new (retryable)", got.Status)
	}
	if !strings.Contains(got.Result, "without committing") {
		t.Errorf("result = %q", got.Result)
	}
	// The abandoned attempt leaves no branch behind.
	if out, err := exec.Command("git", "-C", filepath.Join(repos, "demo"), "branch", "--list", "taskflow/*").Output(); err != nil {
		t.Fatalf("list branches: %v", err)
	} else if len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("branches left behind: %s", out)
	}
}

func TestRunRejectsUnusableRepoNames(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)

	agent := fakeAgent{fn: func(context.Context, string, string) (string, error) {
		t.Error("agent ran for a task it should have rejected")
		return "", nil
	}}
	r := newRunner(t, s, repos, agent, nil)

	cases := map[string]string{
		"no repo scope": "no repo: scope",
		"escaping name": "not a plain repository name",
		"several repos": "expected exactly one",
	}
	tasks := map[string]*store.Task{
		"no repo scope": addTask(t, s, store.NewTask{Title: "a", Scopes: []string{"docs"}}),
		"escaping name": addTask(t, s, store.NewTask{Title: "b", Scopes: []string{"repo:../../etc"}}),
		"several repos": addTask(t, s, store.NewTask{Title: "c", Scopes: []string{"repo:demo", "repo:other"}}),
	}

	if _, err := r.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	for name, want := range cases {
		got, err := s.Get(ctx, tasks[name].ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !strings.Contains(got.Result, want) {
			t.Errorf("%s: result = %q, want it to mention %q", name, got.Result, want)
		}
	}
}

func TestRunRecordsAgentError(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)

	agent := fakeAgent{fn: func(context.Context, string, string) (string, error) {
		return "", errors.New("model exploded")
	}}
	task := addTask(t, s, store.NewTask{Title: "doomed", Scopes: []string{"repo:demo"}})

	stats, err := newRunner(t, s, repos, agent, nil).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats != (runner.Stats{Tasks: 1, Failed: 1}) {
		t.Errorf("stats = %+v", stats)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.New || !strings.Contains(got.Result, "model exploded") {
		t.Errorf("task = %+v", got)
	}
}

func TestSharedScopeSerialisesExecution(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)

	var mu sync.Mutex
	active, peak := 0, 0
	agent := fakeAgent{fn: func(_ context.Context, dir, _ string) (string, error) {
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
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o600); err != nil {
			return "", err
		}
		commitAll(t, dir, "work")
		return "done", nil
	}}

	scope := []string{"repo:demo"}
	addTask(t, s, store.NewTask{Title: "one", Scopes: scope})
	addTask(t, s, store.NewTask{Title: "two", Scopes: scope})

	r := newRunner(t, s, repos, agent, func(c *runner.Config) { c.Workers = 4 })
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
	repos := newRepo(t, "demo")
	s := newStore(t)

	agent := fakeAgent{fn: func(_ context.Context, dir, _ string) (string, error) {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o600); err != nil {
			return "", err
		}
		commitAll(t, dir, "work")
		return "done", nil
	}}

	// Two tasks contend for the same scope, and a third waits on the first.
	first := addTask(t, s, store.NewTask{Title: "first", Scopes: []string{"repo:demo"}})
	addTask(t, s, store.NewTask{Title: "sibling", Scopes: []string{"repo:demo"}})
	addTask(t, s, store.NewTask{Title: "dependent", Scopes: []string{"repo:demo"}, Deps: []int64{first.ID}})

	// One worker at a time forces the queue to be re-examined between rounds.
	r := newRunner(t, s, repos, agent, func(c *runner.Config) { c.Workers = 1 })
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
	repos := newRepo(t, "demo")
	s := newStore(t)

	agent := fakeAgent{fn: func(_ context.Context, dir, _ string) (string, error) {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o600); err != nil {
			return "", err
		}
		commitAll(t, dir, "work")
		return "done", nil
	}}

	for i := 0; i < 5; i++ {
		addTask(t, s, store.NewTask{Title: "task", Scopes: []string{"repo:demo"}})
	}

	r := newRunner(t, s, repos, agent, func(c *runner.Config) { c.MaxTasks = 2 })
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
		Agent:       fakeAgent{fn: func(context.Context, string, string) (string, error) { return "", nil }},
		ReposDir:    t.TempDir(),
		WorktreeDir: t.TempDir(),
		LeaseTTL:    time.Minute,
		TaskTimeout: time.Hour,
	})
	if err == nil {
		t.Fatal("a lease shorter than the task timeout was accepted")
	}
}

func TestTimeoutKillsTheAgentAndItsChildren(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")

	// The worker spawns something long-lived, then hangs. Both must die at the
	// timeout, or the leftover keeps the worktree and the port.
	script := filepath.Join(t.TempDir(), "hanging-pi")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nsleep 60\n"
	// The fixture has to be executable.
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // G306: must be executable
		t.Fatalf("write agent: %v", err)
	}

	task := addTask(t, s, store.NewTask{Title: "hang", Scopes: []string{"repo:demo"}})
	r := newRunner(t, s, repos, runner.Pi{Bin: script}, func(c *runner.Config) {
		c.TaskTimeout = 300 * time.Millisecond
	})

	stats, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.New {
		t.Errorf("status = %s, want new", got.Status)
	}

	raw, err := os.ReadFile(pidFile) //nolint:gosec // G304: path built by the test
	if err != nil {
		t.Fatalf("agent never started its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("child pid %q: %v", raw, err)
	}
	if alive(pid, 2*time.Second) {
		t.Errorf("child process %d outlived the agent", pid)
	}
}

// alive reports whether pid still exists, giving the kernel a moment to reap
// a killed process.
func alive(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCancelledRunCleansUpAndLeavesTheLease(t *testing.T) {
	repos := newRepo(t, "demo")
	s := newStore(t)
	worktrees := filepath.Join(t.TempDir(), "worktrees")

	started := make(chan struct{})
	agent := fakeAgent{fn: func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	task := addTask(t, s, store.NewTask{Title: "interrupted", Scopes: []string{"repo:demo"}})

	var logged bytes.Buffer
	r := newRunner(t, s, repos, agent, func(c *runner.Config) {
		c.WorktreeDir = worktrees
		c.TaskTimeout = time.Minute
		c.Logger = log.New(&logged, "", 0)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := r.Run(ctx); err == nil {
		t.Fatal("a cancelled run reported success")
	}

	// The checkout is scratch space and must not outlive the run.
	entries, err := os.ReadDir(worktrees)
	if err != nil {
		t.Fatalf("read worktrees: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("worktrees left behind: %v (runner log: %s)", entries, logged.String())
	}

	// Nothing was written, so the task stays leased and expires on its own.
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
	repos := newRepo(t, "demo")
	s := newStore(t)
	task := addTask(t, s, store.NewTask{Title: "contested", Scopes: []string{"repo:demo"}})

	// Something else takes the task away while the worker is busy.
	agent := fakeAgent{fn: func(_ context.Context, dir, _ string) (string, error) {
		if err := s.Fail(ctx, task.ID, "reclaimed by a newer run"); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o600); err != nil {
			return "", err
		}
		commitAll(t, dir, "work")
		return "done, but no longer mine", nil
	}}

	r := newRunner(t, s, repos, agent, nil)
	stats, err := r.Run(ctx)
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

func TestStaleWorktreeRegistrationIsRecovered(t *testing.T) {
	ctx := context.Background()
	repos := newRepo(t, "demo")
	s := newStore(t)
	worktrees := filepath.Join(t.TempDir(), "worktrees")

	task := addTask(t, s, store.NewTask{Title: "after a crash", Scopes: []string{"repo:demo"}})
	repo := filepath.Join(repos, "demo")

	// A previous run registered this path and died; git refuses to reuse it
	// until the registration is pruned.
	stale := filepath.Join(worktrees, "demo-1.1")
	runCmd(t, repo, "git", "worktree", "add", "-q", "-b", "stale", stale, "HEAD")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatalf("remove stale worktree: %v", err)
	}

	agent := fakeAgent{fn: func(_ context.Context, dir, _ string) (string, error) {
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("x\n"), 0o600); err != nil {
			return "", err
		}
		commitAll(t, dir, "work")
		return "done", nil
	}}
	r := newRunner(t, s, repos, agent, func(c *runner.Config) { c.WorktreeDir = worktrees })

	stats, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Completed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.Done {
		t.Errorf("status = %s, want done", got.Status)
	}
}
