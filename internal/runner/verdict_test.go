package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loa/internal/runner"
	"loa/internal/store"
	"loa/internal/taskspec"
)

// committedDependency finishes an implementation task, so a review task can
// depend on a branch that actually carries a change.
func committedDependency(ctx context.Context, t *testing.T, s *store.Store, b *fakeBackend) *store.Task {
	t.Helper()
	dep := addTask(t, s, store.NewTask{Title: "implementation"})
	b.agent = committer(t)
	if _, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx); err != nil {
		t.Fatalf("run dependency: %v", err)
	}
	got, err := s.Get(ctx, dep.ID)
	if err != nil {
		t.Fatalf("get dependency: %v", err)
	}
	if got.Status != store.Done || got.Branch == "" {
		t.Fatalf("dependency = %+v, want a done task with a branch", got)
	}
	return got
}

func verdictAgent(document string) func(context.Context, string) (string, error) {
	return func(_ context.Context, dir string) (string, error) {
		path := filepath.Join(dir, ".loa")
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(path, "verdict.json"), []byte(document), 0o600); err != nil {
			return "", err
		}
		return "reviewed the change", nil
	}
}

func addReview(t *testing.T, s *store.Store, dep *store.Task) *store.Task {
	t.Helper()
	return addTask(t, s, store.NewTask{
		Title: "review the implementation",
		Body:  "check the change against the task",
		Role:  "review",
		Deps:  []int64{dep.ID},
	})
}

func TestReviewApproveFinishesWithoutACommit(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, "demo")
	s := newStore(t)
	b := newFakeBackend(t, repo)
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	b.agent = verdictAgent(`{"verdict":"approve","notes":[{"file":"work-1.1.txt","issue":"nothing missing","suggestion":""}]}`)
	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Completed != 1 {
		t.Fatalf("stats = %+v, want the review done", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status != store.Done || got.Verdict != string(taskspec.Approve) {
		t.Errorf("review = %+v, want done with an approve verdict", got)
	}
	if !strings.Contains(got.VerdictNotes, "nothing missing") {
		t.Errorf("stored notes = %q", got.VerdictNotes)
	}
	if !b.wasClosed("w2") {
		t.Error("the review workspace was left open")
	}
	// The review wrote no commit, so its branch is not kept as a deliverable.
	if out, _ := gitE(repo, "branch", "--list", "loa/2.1"); strings.TrimSpace(out) != "" {
		t.Errorf("review branch left behind: %s", out)
	}

	// The dependency's change, not just its branch name, reached the reviewer.
	prompt := b.promptText("w2")
	for _, want := range []string{"## The change under review", "work-1.1.txt", dep.Branch} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

func TestReviewChangesWritesAProposalAndDoesNotApplyIt(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, "demo")
	s := newStore(t)
	b := newFakeBackend(t, repo)
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	proposals := t.TempDir()
	b.agent = verdictAgent(`{"verdict":"changes","notes":[{"file":"work-1.1.txt","issue":"wrong shape","suggestion":"reshape it"}],"tasks":[{"title":"follow-up from review","body":"do the reshape","workspace":"loa"}]}`)

	stats, err := newRunner(t, s, b, func(c *runner.Config) {
		c.MaxTasks = 1
		c.ProposalsDir = proposals
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Completed != 1 {
		t.Fatalf("stats = %+v, want the review done", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status != store.Done || got.Verdict != string(taskspec.Changes) {
		t.Errorf("review = %+v, want done with a changes verdict", got)
	}

	entries, err := os.ReadDir(proposals)
	if err != nil {
		t.Fatalf("read proposals: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("proposal files = %d, want exactly one", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(proposals, entries[0].Name())) //nolint:gosec // G304: the path is the proposal file this test just wrote
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	doc, err := taskspec.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse proposal: %v", err)
	}
	if len(doc.Tasks) != 1 || doc.Tasks[0].Title != "follow-up from review" {
		t.Errorf("proposal = %s", raw)
	}

	// Writing the proposal is not applying it: no task with that title exists.
	tasks, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, task := range tasks {
		if task.Title == "follow-up from review" {
			t.Errorf("the changes verdict applied its own proposal: %+v", task)
		}
	}
}

func TestReviewBlockedLeavesThePaneOpen(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	b.agent = verdictAgent(`{"verdict":"blocked","notes":[{"file":"README.md","issue":"the task body conflicts with itself"}]}`)
	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failure", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status == store.Done || got.Verdict != string(taskspec.Blocked) {
		t.Errorf("review = %+v, want a blocked verdict and no completion", got)
	}
	if !strings.Contains(got.Result, "w2") || !strings.Contains(got.Result, "w2:p1") {
		t.Errorf("result does not name the pane: %q", got.Result)
	}
	if b.wasClosed("w2") {
		t.Error("a blocked reviewer's workspace was closed; the human needs it")
	}
	if b.wasInterrupted("w2") {
		t.Error("a blocked reviewer was interrupted instead of left waiting")
	}
}

func TestMalformedVerdictFailsTheReview(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	b.agent = verdictAgent(`{"verdict":"approve","confidence":"high"}`)
	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failure", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status == store.Done || got.Verdict != "" {
		t.Errorf("review = %+v, want a failure with no verdict", got)
	}
	if !strings.Contains(got.Result, "unknown field") {
		t.Errorf("result = %q, want the parser's reason", got.Result)
	}
	if !b.wasClosed("w2") {
		t.Error("a failed review's workspace was left open")
	}
}

func TestCommitWithoutVerdictCannotFinishAReview(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	b.agent = committer(t)
	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failure", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status == store.Done {
		t.Errorf("a commit alone finished a review: %+v", got)
	}
	if !strings.Contains(got.Result, ".loa/verdict.json") {
		t.Errorf("result = %q, want the missing deliverable named", got.Result)
	}
}

func TestReviewWithNothingToReviewFailsBeforeOpening(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	review := addTask(t, s, store.NewTask{Title: "review nothing", Role: "review"})

	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failure", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status == store.Done || !strings.Contains(got.Result, "nothing to review") {
		t.Errorf("review = %+v, want a failure naming the empty subject", got)
	}
	if len(b.opens) != 0 {
		t.Errorf("a review with no subject opened a workspace: %+v", b.opens)
	}
}

// A dependency that is done without a branch has no change to read, so the
// review fails rather than approving an empty diff.
func TestReviewWithNoCommittedDependencyFails(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	dep := addTask(t, s, store.NewTask{Title: "settled by hand"})
	if err := s.Resolve(ctx, dep.ID, true, "approved without a branch"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	review := addReview(t, s, dep)

	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want one failure", stats)
	}
	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status == store.Done || !strings.Contains(got.Result, "nothing to review") {
		t.Errorf("review = %+v, want a failure naming the empty subject", got)
	}
}
