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
	"sort"
	"strings"
	"sync"
	"time"

	"loa/internal/store"
)

// Config is one run's worth of settings. Zero values are replaced by defaults
// in New; the only combination that is rejected is a lease that cannot outlive
// a task.
type Config struct {
	Store        *store.Store
	Backend      Backend
	Base         string // ref each worktree branches from
	BranchPrefix string
	PromptsDir   string // where role prompt files live; default found from the cwd
	ProposalsDir string // where a changes verdict writes its proposal
	Workers      int
	MaxTasks     int           // tasks per run, i.e. the budget for one run
	LeaseTTL     time.Duration // must exceed TaskTimeout
	TaskTimeout  time.Duration
	Logger       *log.Logger
}

// TaskOutcome is what became of one dispatched task, as recorded in the store.
// The ids and branch are read back from the task row rather than taken from the
// backend's handle: the row is what the dashboard, the report, and the next run
// see, so it is the only source of truth here.
type TaskOutcome struct {
	ID          int64
	Done        bool
	Blocked     bool
	Branch      string
	WorkspaceID string
	PaneID      string
}

// Stats is what one run did. Run is the run row this run closed.
type Stats struct {
	Run       store.Run
	Tasks     int
	Completed int
	Failed    int
	Outcomes  []TaskOutcome
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
		cfg.BranchPrefix = "loa/"
	}
	if cfg.PromptsDir == "" {
		cfg.PromptsDir = defaultPromptsDir()
	}
	if cfg.ProposalsDir == "" {
		cfg.ProposalsDir = defaultProposalsDir()
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
func (r *Runner) Run(ctx context.Context) (stats Stats, runErr error) {
	// Whatever a previous, dead run left open is closed before new work starts.
	r.reconcile(ctx, time.Now())

	run, err := r.startRun(ctx)
	if err != nil {
		return stats, err
	}
	stats.Run = run

	// The row is closed on every way out of this function, including a cancelled
	// run: an open row would keep the next run from starting until a person
	// noticed it. Cleanup gets its own context for the same reason.
	defer func() {
		sort.Slice(stats.Outcomes, func(i, j int) bool { return stats.Outcomes[i].ID < stats.Outcomes[j].ID })
		stats.Run.Done = stats.Completed
		stats.Run.Failed = stats.Failed

		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.cfg.Store.FinishRun(closeCtx, run.ID, stats.Completed, stats.Failed); err != nil {
			r.log.Printf("run %d: close: %v", run.ID, err)
			if runErr == nil {
				runErr = fmt.Errorf("runner: close run %d: %w", run.ID, err)
			}
			return
		}
		stats.Run.EndedAt = time.Now().UTC().Format(time.RFC3339)
	}()

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

		stats.Outcomes = append(stats.Outcomes, r.taskOutcomes(batch)...)
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

// startRun opens this run's row. A refusal names the run that already owns the
// slot, because "a run is active" alone does not tell a person which pane to
// look at.
func (r *Runner) startRun(ctx context.Context) (store.Run, error) {
	run, err := r.cfg.Store.StartRun(ctx, r.owner, r.cfg.Workers, r.cfg.MaxTasks)
	if errors.Is(err, store.ErrRunActive) {
		active, lookupErr := r.cfg.Store.ActiveRun(ctx)
		if lookupErr == nil && active != nil {
			return store.Run{}, fmt.Errorf("runner: run %d is active (owner %s); if it is dead, close it with `loa runs -close %d`: %w",
				active.ID, active.Owner, active.ID, err)
		}
		return store.Run{}, fmt.Errorf("runner: a run is active (owner unknown); if it is dead, find it with `loa runs` and close it with `loa runs -close ID`: %w", err)
	}
	if err != nil {
		return store.Run{}, fmt.Errorf("runner: start run: %w", err)
	}
	return run, nil
}

// taskOutcomes reads each task's row after dispatch, so the reported branch,
// workspace and pane are the recorded ones rather than a handle that may never
// have been attachable. A read failure still reports the task, as failed.
func (r *Runner) taskOutcomes(tasks []*store.Task) []TaskOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out := make([]TaskOutcome, 0, len(tasks))
	for _, task := range tasks {
		got, err := r.cfg.Store.Get(ctx, task.ID)
		if err != nil {
			r.log.Printf("task %d: read outcome: %v", task.ID, err)
			out = append(out, TaskOutcome{ID: task.ID})
			continue
		}
		out = append(out, TaskOutcome{
			ID:          got.ID,
			Done:        got.Status == store.Done,
			Blocked:     got.Status == store.Blocked,
			Branch:      got.Branch,
			WorkspaceID: got.WorkspaceID,
			PaneID:      got.PaneID,
		})
	}
	return out
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

	// The role decides the prompt and the tools before anything is opened: an
	// unknown role or a missing file must fail without leaving a workspace.
	spec, err := specForRole(task.Role)
	if err != nil {
		return r.fail(ctx, task, err.Error())
	}
	source, err := readPrompt(r.cfg.PromptsDir, spec)
	if err != nil {
		return r.fail(ctx, task, err.Error())
	}

	review := spec.deliverable == deliverableVerdict
	if review {
		deps, err := r.cfg.Store.Deps(ctx, task.ID)
		if err != nil {
			return r.record(task, err)
		}
		if len(deps) == 0 {
			return r.fail(ctx, task, "review has nothing to review: it depends on no task")
		}
	}

	// A done dependency's branch already carries what that task committed, so a
	// dependent task starts there instead of at the configured base. The store
	// picks the dependency that finished last; an empty answer means none
	// qualifies and the base stands.
	base := r.cfg.Base
	depBase, err := r.cfg.Store.DependencyBase(ctx, task.ID)
	if err != nil {
		return r.record(task, err)
	}
	if depBase != "" {
		base = depBase
		logf("branching from done dependency %s", depBase)
	}

	handle, err := r.cfg.Backend.Open(ctx, OpenRequest{
		TaskID:         task.ID,
		Attempt:        task.Attempts,
		WorkspaceLabel: task.Workspace,
		Branch:         r.cfg.BranchPrefix + fmt.Sprintf("%d.%d", task.ID, task.Attempts),
		Base:           base,
		AgentName:      fmt.Sprintf("loa-%d-%d", task.ID, task.Attempts),
		Tools:          spec.tools,
	})
	if err != nil {
		return r.fail(ctx, task, err.Error())
	}
	logf("working in %s on %s (workspace %s, agent %s)",
		handle.Worktree, handle.Branch, handle.WorkspaceID, handle.AgentName)

	// The workspace exists from here on. Recording it is what lets the dashboard
	// address it and reconciliation know it is ours; if the write fails, the
	// workspace is left alone rather than destroyed — it is the only evidence of
	// what the attempt was doing, and the next run's label fallback finds it.
	if err := r.cfg.Store.Attach(ctx, task.ID, handle.WorkspaceID, handle.PaneID); err != nil {
		logf("cannot record workspace %s: %v", handle.WorkspaceID, err)
		return r.record(task, err)
	}

	// The prompt file is rendered with the attempt's real worktree, then the
	// contract is appended. The name and hash go on the row before the prompt is
	// sent, so everything this attempt produces is attributable to those bytes.
	fields := fieldsFor(task, handle.Worktree, handle.Repo, handle.Branch)
	if review {
		diffs, err := r.reviewDiffs(ctx, task, handle)
		if err != nil {
			r.stopAndClose(handle, logf)
			if commits, _ := commitsAhead(ctx, handle.Worktree, handle.Base); commits == 0 {
				_ = deleteBranch(ctx, handle.Repo, handle.Branch)
			}
			return r.fail(ctx, task, err.Error())
		}
		if len(diffs) == 0 {
			r.stopAndClose(handle, logf)
			_ = deleteBranch(ctx, handle.Repo, handle.Branch)
			return r.fail(ctx, task, "review has nothing to review: no dependency has a committed change")
		}
		fields.Diffs = diffs
	}

	text, err := taskPromptFor(task, spec, spec.prompt, source, fields)
	if err != nil {
		r.stopAndClose(handle, logf)
		return r.fail(ctx, task, err.Error())
	}
	if err := r.cfg.Store.RecordPrompt(ctx, task.ID, spec.prompt, promptHash(source)); err != nil {
		return r.record(task, err)
	}

	// The prompt is bounded twice: the backend's own wait, and this context, so a
	// backend that ignores its timeout still cannot hold the run.
	taskCtx, cancel := context.WithTimeout(ctx, r.cfg.TaskTimeout)
	promptErr := r.cfg.Backend.Prompt(taskCtx, handle, text, r.cfg.TaskTimeout)
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
	if review {
		return r.finishReview(ctx, task, handle, commits, summary, logf)
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

// reconcile closes workspaces the runner owns but nobody is holding, plus the
// ones a dead run created before it could record them. Ownership is the stored
// id; the `loa #N` label is only trusted for the window where a workspace
// exists and no row claims it, and never while that task is being worked on.
func (r *Runner) reconcile(ctx context.Context, now time.Time) {
	spaces, err := r.cfg.Backend.TaskWorkspaces(ctx)
	if err != nil {
		r.log.Printf("reconcile: %v", err)
		return
	}
	claims, err := r.cfg.Store.Claims(ctx)
	if err != nil {
		r.log.Printf("reconcile: %v", err)
		return
	}

	owner := make(map[string]int64, len(claims))
	for _, c := range claims {
		owner[c.WorkspaceID] = c.TaskID
	}
	held := make(map[int64]bool)

	for _, space := range spaces {
		claimedBy, claimed := owner[space.WorkspaceID]
		switch {
		case claimed && claimedBy != space.TaskID:
			// Recorded against one task, labelled for another: not made by this
			// runner, so it is not this runner's to remove.
			r.log.Printf("reconcile: workspace %s is labelled for task %d but recorded for task %d; leaving it alone",
				space.WorkspaceID, space.TaskID, claimedBy)
		case claimed:
			if r.isHeld(ctx, claimedBy, now, held) {
				continue
			}
			r.closeWorkspace(ctx, space, fmt.Sprintf("task %d is no longer held", claimedBy))
		default:
			// Nothing claims this id: the run that created it died before it could
			// record it. The label is the only evidence, so it is trusted only while
			// nobody is working on that task — a live runner may be in exactly this
			// window right now.
			if r.isHeld(ctx, space.TaskID, now, held) {
				continue
			}
			r.closeWorkspace(ctx, space, fmt.Sprintf("task %d has no record of it", space.TaskID))
		}
	}
}

func (r *Runner) closeWorkspace(ctx context.Context, space TaskWorkspace, why string) {
	if err := r.cfg.Backend.Close(ctx, Handle{TaskID: space.TaskID, WorkspaceID: space.WorkspaceID}); err != nil {
		r.log.Printf("reconcile: close %s: %v", space.WorkspaceID, err)
		return
	}
	r.log.Printf("reconcile: closed workspace %s: %s", space.WorkspaceID, why)
}

// isHeld reports whether a task's lease is still good, remembering answers for
// the length of one reconciliation. When in doubt it answers yes: closing a live
// worker's pane is worse than leaving a tab behind.
func (r *Runner) isHeld(ctx context.Context, taskID int64, now time.Time, cache map[int64]bool) bool {
	if answer, ok := cache[taskID]; ok {
		return answer
	}

	task, err := r.cfg.Store.Get(ctx, taskID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		cache[taskID] = false
	case err != nil:
		r.log.Printf("reconcile: task %d: %v", taskID, err)
		cache[taskID] = true
	default:
		cache[taskID] = task.Status == store.Leased && task.LeaseExpiry.After(now)
	}
	return cache[taskID]
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
