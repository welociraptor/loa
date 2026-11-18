// Package runner turns the task store into work: lease a task, give it a
// worktree and an agent, judge the result from the repository rather than from
// what the agent claims, and record the outcome.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskflow/internal/store"
)

// Config is one run's worth of settings. Zero values are replaced by defaults
// in New; the only combination that is rejected is a lease that cannot outlive
// a task.
type Config struct {
	Store        *store.Store
	Agent        Agent
	ReposDir     string // directory holding the checkouts a repo: scope names
	WorktreeDir  string // where per-task worktrees are created
	Base         string // commit or ref each worktree branches from
	BranchPrefix string
	Workers      int
	MaxTasks     int           // tasks per run, i.e. the daily budget when run from a timer
	LeaseTTL     time.Duration // must exceed TaskTimeout
	TaskTimeout  time.Duration
	Logger       *log.Logger
}

// Stats is what one run did.
type Stats struct {
	Tasks     int
	Completed int
	Failed    int
}

// Runner works the queue. It is safe to run two of them against one database:
// leases and scopes live in SQL, not in this struct.
type Runner struct {
	cfg   Config
	log   *log.Logger
	owner string
}

// New validates cfg, fills in defaults, and prepares the worktree directory.
func New(cfg Config) (*Runner, error) {
	if cfg.Store == nil {
		return nil, errors.New("runner: no store")
	}
	if cfg.Agent == nil {
		return nil, errors.New("runner: no agent")
	}

	var err error
	if cfg.ReposDir, err = filepath.Abs(cfg.ReposDir); err != nil {
		return nil, fmt.Errorf("runner: repos dir: %w", err)
	}
	if cfg.WorktreeDir == "" {
		cfg.WorktreeDir = defaultWorktreeDir()
	}
	if cfg.WorktreeDir, err = filepath.Abs(cfg.WorktreeDir); err != nil {
		return nil, fmt.Errorf("runner: worktree dir: %w", err)
	}
	if cfg.Base == "" {
		cfg.Base = "HEAD"
	}
	if cfg.BranchPrefix == "" {
		cfg.BranchPrefix = "taskflow/"
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.MaxTasks <= 0 {
		cfg.MaxTasks = 1
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 2 * time.Hour
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 45 * time.Minute
	}
	// A lease that lapses mid-run lets a second worker pick up the same task,
	// so the timeout has to fit inside it with room to spare.
	if cfg.LeaseTTL <= cfg.TaskTimeout {
		return nil, fmt.Errorf("runner: lease ttl %s must exceed task timeout %s", cfg.LeaseTTL, cfg.TaskTimeout)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "runner: ", log.LstdFlags)
	}

	if err := os.MkdirAll(cfg.WorktreeDir, 0o700); err != nil {
		return nil, fmt.Errorf("runner: worktree dir: %w", err)
	}

	host, err := os.Hostname()
	if err != nil {
		host = "runner"
	}

	return &Runner{
		cfg:   cfg,
		log:   cfg.Logger,
		owner: fmt.Sprintf("%s/%d", host, os.Getpid()),
	}, nil
}

func defaultWorktreeDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "taskflow", "worktrees")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "taskflow", "worktrees")
	}
	return filepath.Join(home, ".local", "state", "taskflow", "worktrees")
}

// Run works the queue until it is empty or MaxTasks is spent. Each round leases
// as many runnable tasks as it has workers and runs them concurrently, so a
// task freed by a finishing sibling is picked up by the next round instead of
// being missed by a worker that already gave up.
func (r *Runner) Run(ctx context.Context) (Stats, error) {
	var stats Stats

	for stats.Tasks < r.cfg.MaxTasks {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		slots := min(r.cfg.Workers, r.cfg.MaxTasks-stats.Tasks)
		batch := make([]*store.Task, 0, slots)
		for len(batch) < slots {
			task, err := r.cfg.Store.Lease(ctx, r.owner, r.cfg.LeaseTTL, time.Now())
			if err != nil {
				return stats, err
			}
			if task == nil {
				break
			}
			batch = append(batch, task)
		}
		if len(batch) == 0 {
			return stats, nil
		}

		r.log.Printf("leased %d task(s) as %s", len(batch), r.owner)

		outcomes := make([]outcome, len(batch))
		var wg sync.WaitGroup
		for i, task := range batch {
			wg.Add(1)
			go func(i int, task *store.Task) {
				defer wg.Done()
				outcomes[i] = r.dispatch(ctx, task)
			}(i, task)
		}
		wg.Wait()

		for _, o := range outcomes {
			if o.err != nil {
				return stats, o.err
			}
			stats.Tasks++
			if o.ok {
				stats.Completed++
			} else {
				stats.Failed++
			}
		}
	}
	return stats, nil
}

// outcome is one task's fate. err is reserved for failures of the runner
// itself, which end the run rather than being charged to the task.
type outcome struct {
	ok  bool
	err error
}

func (r *Runner) dispatch(ctx context.Context, task *store.Task) outcome {
	logf := func(format string, args ...any) {
		r.log.Printf("task %d: "+format, append([]any{task.ID}, args...)...)
	}

	repo, err := repoFor(task)
	if err != nil {
		return r.fail(ctx, task, err.Error())
	}
	repoPath := filepath.Join(r.cfg.ReposDir, repo)

	base, err := gitOutput(ctx, repoPath, "rev-parse", "--verify", r.cfg.Base+"^{commit}")
	if err != nil {
		return r.fail(ctx, task, "cannot resolve base: "+err.Error())
	}

	worktree, branch, err := r.addWorktree(ctx, repoPath, task)
	if err != nil {
		return r.fail(ctx, task, "cannot create worktree: "+err.Error())
	}
	logf("working in %s on %s", worktree, branch)

	taskCtx, cancel := context.WithTimeout(ctx, r.cfg.TaskTimeout)
	defer cancel()

	output, runErr := r.cfg.Agent.Run(taskCtx, worktree, prompt(task, worktree, repo, branch))

	// The agent's own summary cannot tell success from failure; the branch can.
	// It is read even after a failed run, because a worker that crashed after
	// committing has left work worth keeping.
	commits, gitErr := commitsAhead(ctx, worktree, base)

	// Clean up before judging the task: the checkout is scratch space, but the
	// branch is the output. An attempt that produced no commit leaves no branch
	// behind, so a queue that fails all day does not litter the repository.
	r.removeWorktree(repoPath, worktree)
	if gitErr == nil && commits == 0 {
		r.deleteBranch(repoPath, branch)
	}

	if runErr != nil {
		return r.fail(ctx, task, runErr.Error())
	}
	if gitErr != nil {
		return r.fail(ctx, task, "cannot inspect the worktree: "+gitErr.Error())
	}
	if commits == 0 {
		return r.fail(ctx, task, "finished without committing: "+Tail(strings.TrimSpace(output), 800))
	}

	if err := r.cfg.Store.Complete(ctx, task.ID, branch, Tail(strings.TrimSpace(output), 4000)); err != nil {
		return r.record(task, err)
	}
	logf("done: %s (%d commit(s))", branch, commits)
	return outcome{ok: true}
}

// fail charges a failure to the task: the store decides whether it is retried
// or blocked.
func (r *Runner) fail(ctx context.Context, task *store.Task, reason string) outcome {
	r.log.Printf("task %d: failed: %s", task.ID, Tail(reason, 500))
	if err := r.cfg.Store.Fail(ctx, task.ID, Tail(reason, 4000)); err != nil {
		return r.record(task, err)
	}
	return outcome{}
}

// record decides whether a store write failure is the runner's problem. A lost
// lease is not: the task belongs to someone else now, or is already requeued.
func (r *Runner) record(task *store.Task, err error) outcome {
	if errors.Is(err, store.ErrNotLeased) {
		r.log.Printf("task %d: lease was lost, leaving it to whoever holds it now", task.ID)
		return outcome{}
	}
	return outcome{err: fmt.Errorf("runner: task %d: %w", task.ID, err)}
}

func repoFor(task *store.Task) (string, error) {
	var repos []string
	for _, scope := range task.Scopes {
		name, ok := strings.CutPrefix(scope, "repo:")
		if !ok {
			continue
		}
		if err := checkRepoName(name); err != nil {
			return "", err
		}
		repos = append(repos, name)
	}

	switch len(repos) {
	case 0:
		return "", errors.New("no repo: scope, so there is nothing to work on")
	case 1:
		return repos[0], nil
	default:
		return "", fmt.Errorf("several repo: scopes (%s), expected exactly one", strings.Join(repos, ", "))
	}
}

// checkRepoName keeps a scope from naming a path outside ReposDir. Task text is
// often machine-authored, so it is treated as untrusted input.
func checkRepoName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("repo scope %q is not a plain repository name", name)
	}
	return nil
}

func (r *Runner) addWorktree(ctx context.Context, repoPath string, task *store.Task) (path, branch string, err error) {
	attempt := fmt.Sprintf("%d.%d", task.ID, task.Attempts)
	branch = r.cfg.BranchPrefix + attempt
	path = filepath.Join(r.cfg.WorktreeDir, filepath.Base(repoPath)+"-"+attempt)

	// A crash can leave the directory and its registration behind; git refuses
	// to reuse the path until the registration is pruned.
	if err := os.RemoveAll(path); err != nil {
		return "", "", err
	}
	if _, err := gitOutput(ctx, repoPath, "worktree", "prune"); err != nil {
		return "", "", err
	}
	if _, err := gitOutput(ctx, repoPath, "worktree", "add", "-b", branch, path, r.cfg.Base); err != nil {
		// A cancelled or killed git leaves the directory and, depending on how
		// far it got, a registration behind. Neither is useful to anyone.
		r.removeWorktree(repoPath, path)
		return "", "", err
	}
	return path, branch, nil
}

// removeWorktree deletes the checkout and, when git cannot do that alone, the
// directory and the registration separately. It is called on the happy path and
// after failures, including a worktree that was never fully created.
func (r *Runner) removeWorktree(repoPath, path string) {
	// Cleanup must happen even when the run was cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := gitOutput(ctx, repoPath, "worktree", "remove", "--force", path)
	if err == nil {
		return
	}
	r.log.Printf("worktree %s: git could not remove it (%v), cleaning up by hand", path, err)

	if err := os.RemoveAll(path); err != nil {
		r.log.Printf("worktree %s kept: %v", path, err)
	}
	if _, err := gitOutput(ctx, repoPath, "worktree", "prune"); err != nil {
		r.log.Printf("worktree %s: prune failed: %v", path, err)
	}
}

func (r *Runner) deleteBranch(repoPath, branch string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, err := gitOutput(ctx, repoPath, "branch", "--delete", "--force", branch); err != nil {
		r.log.Printf("branch %s kept: %v", branch, err)
	}
}

func commitsAhead(ctx context.Context, worktree, base string) (int, error) {
	out, err := gitOutput(ctx, worktree, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}
