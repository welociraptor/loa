// Package runner turns the task store into work: lease a task, hand it to a
// backend that opens a worktree workspace with an agent in it, judge the result
// from the repository rather than from what the agent claims, and record the
// outcome.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
	Backend      Backend
	Base         string // ref each worktree branches from
	BranchPrefix string
	Workers      int
	MaxTasks     int           // tasks per run, i.e. the budget for one run
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
	if cfg.Backend == nil {
		return nil, errors.New("runner: no backend")
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

// Run works the queue until it is empty or MaxTasks is spent. Each round leases
// as many runnable tasks as it has workers and runs them concurrently, so a
// task freed by a finishing sibling is picked up by the next round instead of
// being missed by a worker that already gave up.
func (r *Runner) Run(ctx context.Context) (Stats, error) {
	var stats Stats

	// Whatever a previous, dead run left open is closed before new work starts.
	r.reconcile(ctx, time.Now())

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

	if strings.TrimSpace(task.Workspace) == "" {
		return r.fail(ctx, task, "no workspace: a task needs -workspace LABEL to be dispatched")
	}

	handle, err := r.cfg.Backend.Open(ctx, OpenRequest{
		TaskID:         task.ID,
		Attempt:        task.Attempts,
		WorkspaceLabel: task.Workspace,
		Branch:         r.cfg.BranchPrefix + fmt.Sprintf("%d.%d", task.ID, task.Attempts),
		Base:           r.cfg.Base,
		AgentName:      fmt.Sprintf("tf-%d-%d", task.ID, task.Attempts),
	})
	if err != nil {
		return r.fail(ctx, task, err.Error())
	}
	logf("working in %s on %s (workspace %s, agent %s)",
		handle.Worktree, handle.Branch, handle.WorkspaceID, handle.AgentName)

	// The prompt is bounded twice: the backend's own wait, and this context, so a
	// backend that ignores its timeout still cannot hold the run.
	taskCtx, cancel := context.WithTimeout(ctx, r.cfg.TaskTimeout)
	promptErr := r.cfg.Backend.Prompt(taskCtx, handle, prompt(task, handle.Worktree, handle.Repo, handle.Branch), r.cfg.TaskTimeout)
	cancel()

	// A blocked agent is waiting for a person. Leave the pane alone; closing it
	// would take away the thing they are being asked about.
	blocked := errors.Is(promptErr, ErrBlocked)
	if blocked {
		logf("blocked: leaving workspace %s open for a human", handle.WorkspaceID)
		return r.fail(ctx, task, fmt.Sprintf("%v (workspace %s, pane %s)", promptErr, handle.WorkspaceID, handle.PaneID))
	}

	// The agent's own summary cannot tell success from failure; the branch can.
	// It is read even after a failed run, because a worker that crashed after
	// committing has left work worth keeping.
	commits, gitErr := commitsAhead(ctx, handle.Worktree, handle.Base)
	output, _ := r.cfg.Backend.Read(ctx, handle, 100)
	summary := SummaryOf(output, 12)

	if promptErr != nil {
		r.stopAndClose(handle, logf)
		return r.fail(ctx, task, promptErr.Error()+"\n"+summary)
	}
	if gitErr != nil {
		r.stopAndClose(handle, logf)
		return r.fail(ctx, task, "cannot inspect the worktree: "+gitErr.Error())
	}

	// Clean up before judging: the checkout is scratch space, the branch is the
	// output. An attempt that produced no commit leaves no branch behind, so a
	// queue that fails all day does not litter the repository.
	r.stopAndClose(handle, logf)
	if commits == 0 {
		if err := deleteBranch(ctx, handle.Repo, handle.Branch); err != nil {
			logf("branch %s kept: %v", handle.Branch, err)
		}
		return r.fail(ctx, task, "finished without committing: "+summary)
	}

	if err := r.cfg.Store.Complete(ctx, task.ID, handle.Branch, summary); err != nil {
		return r.record(task, err)
	}
	logf("done: %s (%d commit(s))", handle.Branch, commits)
	return outcome{ok: true}
}

// stopAndClose interrupts the agent and removes its workspace. Cleanup uses its
// own context so a cancelled run still tidies up.
func (r *Runner) stopAndClose(handle Handle, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := r.cfg.Backend.Interrupt(ctx, handle); err != nil {
		logf("interrupt %s: %v", handle.AgentName, err)
	}
	if err := r.cfg.Backend.Close(ctx, handle); err != nil {
		logf("close workspace %s: %v", handle.WorkspaceID, err)
	}
}

// reconcile closes task workspaces nobody is holding any more. It replaces the
// stale-worktree pruning the subprocess runner needed: the residue is visible in
// the session, so it can be tidied up on the way in instead of guessed at.
func (r *Runner) reconcile(ctx context.Context, now time.Time) {
	spaces, err := r.cfg.Backend.TaskWorkspaces(ctx)
	if err != nil {
		r.log.Printf("reconcile: %v", err)
		return
	}
	for _, space := range spaces {
		if r.held(ctx, space.TaskID, now) {
			continue
		}
		if err := r.cfg.Backend.Close(ctx, Handle{TaskID: space.TaskID, WorkspaceID: space.WorkspaceID}); err != nil {
			r.log.Printf("reconcile: close %s: %v", space.WorkspaceID, err)
			continue
		}
		r.log.Printf("reconcile: closed workspace %s for task %d", space.WorkspaceID, space.TaskID)
	}
}

// held reports whether a task's lease is still good. When in doubt it answers
// yes: closing a live worker's pane is worse than leaving a tab behind.
func (r *Runner) held(ctx context.Context, taskID int64, now time.Time) bool {
	task, err := r.cfg.Store.Get(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		r.log.Printf("reconcile: task %d: %v", taskID, err)
		return true
	}
	return task.Status == store.Leased && task.LeaseExpiry.After(now)
}

// fail charges a failure to the task: the store decides whether it is retried
// or blocked.
func (r *Runner) fail(ctx context.Context, task *store.Task, reason string) outcome {
	r.log.Printf("task %d: failed: %s", task.ID, tail(reason, 500))
	if err := r.cfg.Store.Fail(ctx, task.ID, tail(reason, 4000)); err != nil {
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
