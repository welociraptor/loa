package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"loa/internal/store"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String())
}

func newMergeRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	gitIn(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	gitIn(t, repo, "config", "user.name", "loa test")
	gitIn(t, repo, "config", "user.email", "test@example.invalid")
	gitIn(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func commitImplBranch(t *testing.T, repo, branch string) {
	t.Helper()
	gitIn(t, repo, "checkout", "-q", "-b", branch, "main")
	name := strings.ReplaceAll(branch, "/", "-") + ".txt"
	if err := os.WriteFile(filepath.Join(repo, name), []byte("change\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "change")
	gitIn(t, repo, "checkout", "-q", "main")
}

// recordImplementation puts a done implementation task with a branch in the
// queue. The branch itself is made by the caller, because a conflict test needs
// a history a helper cannot guess.
func recordImplementation(t *testing.T, s *store.Store, branch string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := s.Add(ctx, store.NewTask{Title: "implementation", Workspace: "loa"})
	if err != nil {
		t.Fatalf("add implementation: %v", err)
	}
	if _, err := s.Lease(ctx, "test", time.Hour, time.Now()); err != nil {
		t.Fatalf("lease implementation: %v", err)
	}
	if err := s.Complete(ctx, id, branch, "done"); err != nil {
		t.Fatalf("complete implementation: %v", err)
	}
	return id
}

// recordReview finishes a review task with the verdict and check a merge reads.
func recordReview(t *testing.T, s *store.Store, impl int64, check string, passed bool, verdict string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := s.Add(ctx, store.NewTask{
		Title: "review", Workspace: "loa", Role: "review", Deps: []int64{impl},
	})
	if err != nil {
		t.Fatalf("add review: %v", err)
	}
	if _, err := s.Lease(ctx, "test", time.Hour, time.Now()); err != nil {
		t.Fatalf("lease review: %v", err)
	}
	if check != "" {
		if err := s.RecordCheck(ctx, id, check, passed, "check output"); err != nil {
			t.Fatalf("record check: %v", err)
		}
	}
	if err := s.CompleteReview(ctx, id, verdict, "[]", "verdict: "+verdict); err != nil {
		t.Fatalf("complete review: %v", err)
	}
	return id
}

func mergeOpen(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMergeApprovedGreenRecordsTheCommit(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "true", true, "approve")

	before := gitIn(t, repo, "rev-parse", "HEAD")
	commit, err := mergeInto(ctx, s, review, "main", repo)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if commit == before {
		t.Fatalf("merge commit = the pre-merge head %s", before)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != commit {
		t.Errorf("checkout HEAD = %s, want the recorded merge commit %s", head, commit)
	}
	if parents := strings.Fields(gitIn(t, repo, "rev-list", "--parents", "-n", "1", "HEAD")); len(parents) != 3 {
		t.Errorf("merge commit parents = %v, want one --no-ff commit with two parents", parents)
	}

	got, err := s.Get(ctx, impl)
	if err != nil {
		t.Fatalf("get implementation: %v", err)
	}
	if got.MergedAt.IsZero() || got.MergeCommit != commit {
		t.Errorf("implementation = %+v, want merged_at and merge_commit %s", got, commit)
	}
}

func TestMergeRefusesARedCheck(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "false", false, "approve")

	before := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := mergeInto(ctx, s, review, "main", repo); err == nil || !strings.Contains(err.Error(), "check failed") {
		t.Fatalf("merge = %v, want a refusal naming the failed check", err)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != before {
		t.Errorf("a refused merge moved HEAD to %s", head)
	}
	got, err := s.Get(ctx, impl)
	if err != nil {
		t.Fatalf("get implementation: %v", err)
	}
	if !got.MergedAt.IsZero() || got.MergeCommit != "" {
		t.Errorf("implementation = %+v, want no merge record", got)
	}
}

func TestMergeRefusesADirtyCheckout(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "true", true, "approve")

	if err := os.WriteFile(filepath.Join(repo, "uncommitted.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := mergeInto(ctx, s, review, "main", repo); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("merge = %v, want a refusal naming the dirty checkout", err)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != before {
		t.Errorf("a refused merge moved HEAD to %s", head)
	}
}

func TestMergeRefusesAWrongBranch(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "true", true, "approve")

	gitIn(t, repo, "checkout", "-q", "-b", "elsewhere")
	before := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := mergeInto(ctx, s, review, "main", repo); err == nil || !strings.Contains(err.Error(), "not \"main\"") {
		t.Fatalf("merge = %v, want a refusal naming the wrong branch", err)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != before {
		t.Errorf("a refused merge moved HEAD to %s", head)
	}
}

func TestMergeConflictRefusesAndLeavesTheBranch(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)

	// Both sides change the same line, so the merge cannot be decided here.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")

	branch := "loa/1.1"
	gitIn(t, repo, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("implementation\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitIn(t, repo, "commit", "-q", "-am", "implementation side")

	gitIn(t, repo, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitIn(t, repo, "commit", "-q", "-am", "main side")

	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "true", true, "approve")
	before := gitIn(t, repo, "rev-parse", "HEAD")

	if _, err := mergeInto(ctx, s, review, "main", repo); err == nil || !strings.Contains(strings.ToLower(err.Error()), "conflict") {
		t.Fatalf("merge = %v, want a refusal naming the conflict", err)
	}
	// The branch is the person's to merge; the target checkout is not left
	// half-merged.
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != before {
		t.Errorf("a conflicted merge moved HEAD to %s", head)
	}
	if status := gitIn(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("checkout left dirty after a conflict: %q", status)
	}
	gitIn(t, repo, "rev-parse", "--verify", branch+"^{commit}")
	got, err := s.Get(ctx, impl)
	if err != nil {
		t.Fatalf("get implementation: %v", err)
	}
	if !got.MergedAt.IsZero() {
		t.Errorf("a conflicted merge was recorded: %+v", got)
	}
}

func TestMergeRefusesASecondMerge(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "true", true, "approve")

	first, err := mergeInto(ctx, s, review, "main", repo)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if _, err := mergeInto(ctx, s, review, "main", repo); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Fatalf("second merge = %v, want an already-merged refusal", err)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != first {
		t.Errorf("a second merge moved HEAD to %s, want %s", head, first)
	}
}

func TestMergeRefusesAReviewWithNoCheckRecorded(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "", false, "approve")

	before := gitIn(t, repo, "rev-parse", "HEAD")
	if _, err := mergeInto(ctx, s, review, "main", repo); err == nil || !strings.Contains(err.Error(), "no check recorded") {
		t.Fatalf("merge = %v, want a refusal naming the missing check", err)
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != before {
		t.Errorf("a refused merge moved HEAD to %s", head)
	}
}

func TestMergeCanNameEitherTheReviewOrItsImplementation(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	recordReview(t, s, impl, "true", true, "approve")

	if _, err := mergeInto(ctx, s, impl, "main", repo); err != nil {
		t.Fatalf("merge by implementation id: %v", err)
	}
	got, err := s.Get(ctx, impl)
	if err != nil {
		t.Fatalf("get implementation: %v", err)
	}
	if got.MergedAt.IsZero() {
		t.Errorf("implementation = %+v, want the merge recorded", got)
	}
}

func TestMergeRefusesAnImplementationWithNoReview(t *testing.T) {
	ctx := context.Background()
	repo := newMergeRepo(t)
	s := mergeOpen(t)
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)

	if _, err := mergeInto(ctx, s, impl, "main", repo); err == nil || !strings.Contains(err.Error(), "no review") {
		t.Fatalf("merge = %v, want a refusal naming the missing review", err)
	}
	if commit := gitIn(t, repo, "rev-parse", "HEAD"); commit == "" {
		t.Fatal("repo state unreadable")
	}
}

func TestMergeCommandUsesTheCheckoutItRunsIn(t *testing.T) {
	repo := newMergeRepo(t)
	db := filepath.Join(t.TempDir(), "tasks.db")
	s, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	branch := "loa/1.1"
	commitImplBranch(t, repo, branch)
	impl := recordImplementation(t, s, branch)
	review := recordReview(t, s, impl, "true", true, "approve")
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cmd := exec.Command(binary, "merge", strconv.FormatInt(review, 10))
	cmd.Dir = repo
	env := []string{"LOA_DB=" + db}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LOA_DB=") || strings.HasPrefix(kv, "HERDR_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("loa merge: %v: %s", err, strings.TrimSpace(stderr.String()))
	}

	commit := strings.TrimSpace(stdout.String())
	if commit != gitIn(t, repo, "rev-parse", "HEAD") {
		t.Errorf("printed commit = %q, want the checkout HEAD", commit)
	}

	s, err = store.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), impl)
	if err != nil {
		t.Fatalf("get implementation: %v", err)
	}
	if got.MergeCommit != commit || got.MergedAt.IsZero() {
		t.Errorf("implementation = %+v, want the merge recorded through the command", got)
	}
}
