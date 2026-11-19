package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"loa/internal/store"
	"loa/internal/taskspec"
)

// defaultProposalsDir is where a changes verdict is kept. It is the same place
// triage proposals go; a person applies either with `loa import`.
func defaultProposalsDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "loa", "proposals")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "loa", "proposals")
	}
	return filepath.Join(home, ".local", "state", "loa", "proposals")
}

// reviewDiffs returns the change each dependency carries against the base it
// branched from. The dependency set is the review subject; no separate column
// is needed to name it.
func (r *Runner) reviewDiffs(ctx context.Context, task *store.Task, handle Handle) ([]promptDiff, error) {
	deps, err := r.cfg.Store.Deps(ctx, task.ID)
	if err != nil {
		return nil, err
	}

	diffs := make([]promptDiff, 0, len(deps))
	for _, dep := range deps {
		got, err := r.cfg.Store.Get(ctx, dep.ID)
		if err != nil {
			return nil, fmt.Errorf("read dependency #%d: %w", dep.ID, err)
		}
		if got.Branch == "" {
			continue
		}

		base, err := r.cfg.Store.DependencyBase(ctx, dep.ID)
		if err != nil {
			return nil, err
		}
		if base == "" {
			base = r.cfg.Base
		}
		diff, err := diffBetween(ctx, handle.Repo, base, got.Branch)
		if err != nil {
			return nil, fmt.Errorf("diff #%d %s against %s: %w", got.ID, got.Branch, base, err)
		}
		if strings.TrimSpace(diff) == "" {
			continue
		}
		diffs = append(diffs, promptDiff{
			ID: got.ID, Title: got.Title, Branch: got.Branch, Base: base, Diff: diff,
		})
	}
	return diffs, nil
}

// finishReview is the reviewer's judgement: a verdict document, not a commit.
// A blocked verdict shares the blocked-agent path and leaves the pane open; the
// other two finish the task, with changes also writing but never applying its
// proposal.
func (r *Runner) finishReview(ctx context.Context, task *store.Task, handle Handle, commits int, summary string, logf func(string, ...any)) outcome {
	review, err := readReview(handle.Worktree)
	if err != nil {
		r.stopAndClose(handle, logf)
		r.discardEmptyBranch(ctx, handle, commits, logf)
		return r.fail(ctx, task, err.Error()+"\n"+summary)
	}

	notes, err := notesJSON(review.Notes)
	if err != nil {
		r.stopAndClose(handle, logf)
		r.discardEmptyBranch(ctx, handle, commits, logf)
		return r.fail(ctx, task, err.Error())
	}

	switch review.Verdict {
	case taskspec.Blocked:
		if err := r.cfg.Store.RecordVerdict(ctx, task.ID, string(review.Verdict), notes); err != nil {
			return r.record(task, err)
		}
		logf("blocked: leaving workspace %s open for a human", handle.WorkspaceID)
		return r.fail(ctx, task, fmt.Sprintf("reviewer's verdict is blocked (workspace %s, pane %s)\n%s",
			handle.WorkspaceID, handle.PaneID, summary))

	case taskspec.Changes:
		path, err := taskspec.SaveProposal(r.cfg.ProposalsDir, review.Tasks, time.Now())
		if err != nil {
			r.stopAndClose(handle, logf)
			r.discardEmptyBranch(ctx, handle, commits, logf)
			return r.fail(ctx, task, err.Error()+"\n"+summary)
		}
		r.stopAndClose(handle, logf)
		r.discardEmptyBranch(ctx, handle, commits, logf)
		if err := r.cfg.Store.CompleteReview(ctx, task.ID, string(review.Verdict), notes,
			fmt.Sprintf("verdict: changes; proposal: %s\n%s", path, summary)); err != nil {
			return r.record(task, err)
		}
		logf("done: verdict changes; proposal %s", path)
		return outcome{ok: true}

	case taskspec.Approve:
		verdict := review.Verdict
		result := "verdict: approve\n" + summary

		if r.cfg.Check != "" {
			passed, output := runCheck(ctx, handle.Worktree, r.cfg.Check, r.cfg.TaskTimeout)
			if err := r.cfg.Store.RecordCheck(ctx, task.ID, r.cfg.Check, passed, output); err != nil {
				r.stopAndClose(handle, logf)
				r.discardEmptyBranch(ctx, handle, commits, logf)
				return r.record(task, err)
			}
			if !passed {
				// A green check is what makes an approval a merge candidate. The
				// opinion is downgraded rather than trusted for its own sake.
				verdict = taskspec.Changes
				result = fmt.Sprintf("verdict: changes; check failed: %s\n%s\n%s",
					r.cfg.Check, output, summary)
				r.stopAndClose(handle, logf)
				r.discardEmptyBranch(ctx, handle, commits, logf)
				if err := r.cfg.Store.CompleteReview(ctx, task.ID, string(verdict), notes, result); err != nil {
					return r.record(task, err)
				}
				logf("done: verdict changes; check failed: %s", r.cfg.Check)
				return outcome{ok: true}
			}
			result = fmt.Sprintf("verdict: approve; check passed: %s\n%s", r.cfg.Check, summary)
		}

		r.stopAndClose(handle, logf)
		r.discardEmptyBranch(ctx, handle, commits, logf)
		if err := r.cfg.Store.CompleteReview(ctx, task.ID, string(verdict), notes, result); err != nil {
			return r.record(task, err)
		}
		logf("done: verdict approve")
		return outcome{ok: true}
	}

	r.stopAndClose(handle, logf)
	r.discardEmptyBranch(ctx, handle, commits, logf)
	return r.fail(ctx, task, fmt.Sprintf("unknown verdict %q", review.Verdict))
}

// runCheck runs the workflow's check in the review worktree. Only the exit
// status is a pass: text on either stream is kept for the row, never read as a
// result. The command is bounded by the task's own timeout.
func runCheck(ctx context.Context, worktree, command string, timeout time.Duration) (bool, string) {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, "sh", "-c", command)
	cmd.Dir = worktree
	// Killing the shell is not enough if a child inherited the pipe: WaitDelay
	// closes it, so a check cannot outlive its timeout through a grandchild.
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, tail(string(output), 4000)
	}
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return false, tail("timed out after "+timeout.String()+"\n"+string(output), 4000)
	}
	return false, tail(err.Error()+"\n"+string(output), 4000)
}

// readReview loads the one file a reviewer must write. Its errors name the
// file when it is missing, because that is the deliverable the agent skipped.
func readReview(worktree string) (taskspec.Review, error) {
	name := filepath.Join(".loa", "verdict.json")
	path := filepath.Join(worktree, name)
	file, err := os.Open(path) //nolint:gosec // G304: worktree is the backend's own scratch directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return taskspec.Review{}, fmt.Errorf("review finished without writing %s", name)
		}
		return taskspec.Review{}, fmt.Errorf("read %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()

	return taskspec.ParseVerdict(file)
}

func notesJSON(notes []taskspec.Note) (string, error) {
	if len(notes) == 0 {
		return "[]", nil
	}
	raw, err := json.Marshal(notes)
	if err != nil {
		return "", fmt.Errorf("encode verdict notes: %w", err)
	}
	return string(raw), nil
}

// discardEmptyBranch removes a review's branch when it carries no commit. A
// review's branch is not the deliverable; one that committed is kept as
// evidence.
func (r *Runner) discardEmptyBranch(ctx context.Context, handle Handle, commits int, logf func(string, ...any)) {
	if commits != 0 {
		return
	}
	if err := deleteBranch(ctx, handle.Repo, handle.Branch); err != nil {
		logf("branch %s kept: %v", handle.Branch, err)
	}
}
