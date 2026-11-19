package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"loa/internal/store"
)

// cmdMerge turns an approved, green review into one merge commit. It is the
// only place loa writes into a checkout a person also works in, so every
// refusal below is a guard rather than a retry: a merge that cannot be decided
// deterministically is left to a person.
func cmdMerge(ctx context.Context, args []string) error {
	var into string
	s, positional, err := open(ctx, "merge", args, func(fs *flag.FlagSet) {
		fs.StringVar(&into, "into", "main", "branch of the workspace checkout to merge into")
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if len(positional) > 1 {
		return fmt.Errorf("merge: at most one review or implementation id")
	}
	if len(positional) == 0 {
		return errors.New("merge: need a review or implementation id")
	}

	id, err := taskID("merge", positional)
	if err != nil {
		return err
	}

	commit, err := mergeInto(ctx, s, id, into, ".")
	if err != nil {
		return err
	}
	fmt.Println(commit)
	return nil
}

// mergeInto is the guard and the transition. dir is the workspace checkout:
// the command runs where a person would, and the checkout is what git checks
// before it is allowed to touch anything.
func mergeInto(ctx context.Context, s *store.Store, id int64, into, dir string) (string, error) {
	if strings.TrimSpace(into) == "" {
		return "", errors.New("merge: -into needs a branch")
	}

	named, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}

	review := named
	if !isReview(named) {
		// A caller may name the implementation task instead: it is the row the
		// merge is recorded on. The review it depends on is the gate.
		review, err = s.ReviewFor(ctx, named.ID)
		if err != nil {
			return "", err
		}
		if review == nil {
			return "", fmt.Errorf("merge: task #%d has no review; nothing merges without a verdict", named.ID)
		}
	}

	if review.Status != store.Done {
		return "", fmt.Errorf("merge: review #%d is %s, not done; nothing merges without a finished review", review.ID, review.Status)
	}
	if review.Verdict != "approve" {
		return "", fmt.Errorf("merge: review #%d's verdict is %q, not approve; a person merges this", review.ID, review.Verdict)
	}
	if review.CheckCommand == "" {
		return "", fmt.Errorf("merge: review #%d has no check recorded; a verdict is not a build", review.ID)
	}
	if !review.CheckPassed {
		return "", fmt.Errorf("merge: review #%d's check failed (%s): %s",
			review.ID, review.CheckCommand, oneLine(review.CheckOutput))
	}

	impl, err := implementationFor(ctx, s, named, review)
	if err != nil {
		return "", err
	}
	if impl.Status != store.Done {
		return "", fmt.Errorf("merge: task #%d is %s, not done; its branch is not a deliverable yet", impl.ID, impl.Status)
	}
	if impl.Branch == "" {
		return "", fmt.Errorf("merge: task #%d recorded no branch to merge", impl.ID)
	}
	if !impl.MergedAt.IsZero() {
		return "", fmt.Errorf("merge: task #%d was already merged at %s (%s); refusing a second merge",
			impl.ID, impl.MergedAt.Format(time.RFC3339), impl.MergeCommit)
	}

	root, err := gitRun(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("merge: %s is not a git checkout: %w", dir, err)
	}

	status, err := gitRun(ctx, root, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", fmt.Errorf("merge: checkout %s is dirty; commit or stash first:\n%s", root, status)
	}

	head, err := gitRun(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if head != into {
		return "", fmt.Errorf("merge: checkout %s is on %q, not %q; switch branches yourself", root, head, into)
	}

	if _, err := gitRun(ctx, root, "rev-parse", "--verify", impl.Branch+"^{commit}"); err != nil {
		return "", fmt.Errorf("merge: branch %s does not exist: %w", impl.Branch, err)
	}

	// One merge, no edit and no push. A conflict is aborted, not resolved: the
	// branch and the decision stay with the person.
	_, stderr, mergeErr := gitCommand(ctx, root, "merge", "--no-ff", "--no-edit", impl.Branch)
	if mergeErr != nil {
		inProgress := gitSucceeds(ctx, root, "rev-parse", "-q", "--verify", "MERGE_HEAD")
		if inProgress {
			_, _, _ = gitCommand(ctx, root, "merge", "--abort")
		}
		if inProgress || strings.Contains(strings.ToUpper(stderr), "CONFLICT") {
			return "", fmt.Errorf("merge: conflict merging %s into %s; aborted and left branch %s for a person: %s",
				impl.Branch, into, impl.Branch, stderr)
		}
		return "", fmt.Errorf("merge: merging %s into %s failed; branch %s kept: %s",
			impl.Branch, into, impl.Branch, stderr)
	}

	commit, err := gitRun(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if err := s.MarkMerged(ctx, impl.ID, time.Now(), commit); err != nil {
		return "", fmt.Errorf("merge: merged %s into %s as %s but could not record it: %w",
			impl.Branch, into, commit, err)
	}
	return commit, nil
}

// isReview reports whether the id already names the row holding the verdict.
func isReview(task *store.Task) bool {
	return task.Role == "review" || task.Role == "reviewer" || task.Verdict != ""
}

// implementationFor finds the branch to merge. When the caller named the
// implementation task, that row is the answer; when it named the review, the
// review must have exactly one implementation to merge.
func implementationFor(ctx context.Context, s *store.Store, named, review *store.Task) (*store.Task, error) {
	deps, err := s.Deps(ctx, review.ID)
	if err != nil {
		return nil, err
	}

	var impls []*store.Task
	for _, dep := range deps {
		got, err := s.Get(ctx, dep.ID)
		if err != nil {
			return nil, err
		}
		if got.Branch != "" {
			impls = append(impls, got)
		}
	}

	if named.ID != review.ID {
		for _, impl := range impls {
			if impl.ID == named.ID {
				return impl, nil
			}
		}
		return nil, fmt.Errorf("merge: review #%d does not depend on task #%d", review.ID, named.ID)
	}

	switch len(impls) {
	case 0:
		return nil, fmt.Errorf("merge: review #%d has no implementation branch to merge", review.ID)
	case 1:
		return impls[0], nil
	default:
		return nil, fmt.Errorf("merge: review #%d covers %d branches; name the implementation task whose branch to merge", review.ID, len(impls))
	}
}

// gitRun is one git command whose failure is an error with its stderr.
func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	out, stderr, err := gitCommand(ctx, dir, args...)
	if err != nil {
		if stderr != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// gitCommand returns stdout and stderr separately, because a failed merge's
// stderr is where the conflict is named.
func gitCommand(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func gitSucceeds(ctx context.Context, dir string, args ...string) bool {
	_, _, err := gitCommand(ctx, dir, args...)
	return err == nil
}

// oneLine flattens a check's output for a refusal message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
