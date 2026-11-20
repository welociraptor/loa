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

// captureReviewDiff saves the review subject while the worktree still exists:
// the runner removes the worktree when the review finishes.
func captureReviewDiff(agent func(context.Context, string) (string, error), sink *string) func(context.Context, string) (string, error) {
	return func(ctx context.Context, dir string) (string, error) {
		raw, err := os.ReadFile(filepath.Join(dir, ".loa", "review.diff")) //nolint:gosec // G304: dir is the test's own worktree
		if err != nil {
			return "", err
		}
		*sink = string(raw)
		return agent(ctx, dir)
	}
}

// largeCommitter writes a change bigger than one argv entry may be.
func largeCommitter(size int) func(context.Context, string) (string, error) {
	return func(_ context.Context, dir string) (string, error) {
		name := "work-" + filepath.Base(dir) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("x", size)+"\n"), 0o600); err != nil {
			return "", err
		}
		if _, err := gitE(dir, "add", "-A"); err != nil {
			return "", err
		}
		if _, err := gitE(dir, "commit", "-q", "-m", "work"); err != nil {
			return "", err
		}
		return "wrote a large change", nil
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

	var diff string
	b.agent = captureReviewDiff(verdictAgent(`{"verdict":"approve","notes":[{"file":"work-1.1.txt","issue":"nothing missing","suggestion":""}]}`), &diff)
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

	// The dependency's change reaches the reviewer through the worktree file,
	// named by the prompt, rather than being embedded in the argument.
	prompt := b.promptText("w2")
	for _, want := range []string{"## The change under review", ".loa/review.diff", dep.Branch} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt does not contain %q:\n%s", want, prompt)
		}
	}
	for _, want := range []string{"work-1.1.txt", dep.Branch} {
		if !strings.Contains(diff, want) {
			t.Errorf("review.diff does not contain %q:\n%s", want, diff)
		}
	}
}

func TestReviewApproveWithGreenCheckRecordsIt(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, "demo")
	s := newStore(t)
	b := newFakeBackend(t, repo)
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	const check = `test -f work-1.1.txt && echo green`
	b.agent = verdictAgent(`{"verdict":"approve","notes":[]}`)
	stats, err := newRunner(t, s, b, func(c *runner.Config) {
		c.MaxTasks = 1
		c.Check = check
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
	if got.Status != store.Done || got.Verdict != string(taskspec.Approve) {
		t.Errorf("review = %+v, want done with an approve verdict", got)
	}
	if got.CheckCommand != check || !got.CheckPassed {
		t.Errorf("check = (%q, %v), want the command and a pass", got.CheckCommand, got.CheckPassed)
	}
	if !strings.Contains(got.CheckOutput, "green") {
		t.Errorf("check output = %q, want what the command printed", got.CheckOutput)
	}
	if !strings.Contains(got.Result, "check passed") {
		t.Errorf("result = %q, want the check recorded", got.Result)
	}
}

func TestReviewApproveWithRedCheckBecomesChanges(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	b := newFakeBackend(t, newRepo(t, "demo"))
	dep := committedDependency(ctx, t, s, b)
	review := addReview(t, s, dep)

	const check = `echo red >&2; exit 1`
	b.agent = verdictAgent(`{"verdict":"approve","notes":[]}`)
	stats, err := newRunner(t, s, b, func(c *runner.Config) {
		c.MaxTasks = 1
		c.Check = check
	}).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Completed != 1 {
		t.Fatalf("stats = %+v, want the review to finish as changes", stats)
	}

	got, err := s.Get(ctx, review.ID)
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if got.Status != store.Done || got.Verdict != string(taskspec.Changes) {
		t.Errorf("review = %+v, want done with a changes verdict, not approve", got)
	}
	if got.CheckCommand != check || got.CheckPassed {
		t.Errorf("check = (%q, %v), want the command and a failure", got.CheckCommand, got.CheckPassed)
	}
	if !strings.Contains(got.CheckOutput, "red") || !strings.Contains(got.Result, "check failed") {
		t.Errorf("output/result = (%q, %q), want the failure named", got.CheckOutput, got.Result)
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

// A chain's second task records its dependency's branch as the base it
// branched from. When that branch is tidied away after a merge, the review
// still diffs the dependency against the configured base, as the worktree did.
func TestReviewFallsBackWhenDependencyBaseIsGone(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, "demo")
	s := newStore(t)
	b := newFakeBackend(t, repo)
	b.agent = committer(t)

	first := addTask(t, s, store.NewTask{Title: "first"})
	second := addTask(t, s, store.NewTask{Title: "second", Deps: []int64{first.ID}})
	if _, err := newRunner(t, s, b, func(c *runner.Config) {
		c.MaxTasks = 2
		c.Workers = 1
	}).Run(ctx); err != nil {
		t.Fatalf("run chain: %v", err)
	}

	firstDone, err := s.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	secondDone, err := s.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if firstDone.Status != store.Done || firstDone.Branch == "" || secondDone.Status != store.Done {
		t.Fatalf("chain = (%+v, %+v), want both done with branches", firstDone, secondDone)
	}

	// The first dependency was merged and its branch tidied away, so the second
	// task's recorded base no longer resolves.
	gitRun(t, repo, "branch", "--delete", "--force", firstDone.Branch)

	review := addReview(t, s, secondDone)
	var diff string
	b.agent = captureReviewDiff(verdictAgent(`{"verdict":"approve","notes":[]}`), &diff)
	stats, err := newRunner(t, s, b, func(c *runner.Config) {
		c.MaxTasks = 1
		c.Base = "main"
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
	if got.Status != store.Done {
		t.Errorf("review = %+v, want it done", got)
	}

	// The dependency is diffed against the configured base it was opened from,
	// and its own change is still there to review.
	prompt := b.promptText("w3")
	if want := "Branch `" + secondDone.Branch + "` from `main`"; !strings.Contains(prompt, want) {
		t.Errorf("review prompt does not contain %q:\n%s", want, prompt)
	}
	for _, want := range []string{"work-1.1.txt", "work-2.1.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("review.diff does not contain %q:\n%s", want, diff)
		}
	}
}

// The subject itself missing is not a cue to fall back: a review with no branch
// to read still fails.
func TestReviewWithAMissingDependencyBranchFails(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, "demo")
	s := newStore(t)
	b := newFakeBackend(t, repo)
	dep := committedDependency(ctx, t, s, b)
	gitRun(t, repo, "branch", "--delete", "--force", dep.Branch)

	review := addReview(t, s, dep)
	b.agent = verdictAgent(`{"verdict":"approve","notes":[]}`)
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
	if got.Status == store.Done || !strings.Contains(got.Result, dep.Branch) {
		t.Errorf("review = %+v, want a failure naming the missing branch %q", got, dep.Branch)
	}
	if prompt := b.promptText("w2"); prompt != "" {
		t.Errorf("a review with no branch to read was still prompted:\n%s", prompt)
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

// A review's subject can be larger than one argv entry may be, so it is written
// into the review worktree and the prompt only names it. This is the regression
// that failed a 135 KiB review before the backend saw the prompt.
func TestReviewKeepsALargeChangeOutOfThePrompt(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, "demo")
	s := newStore(t)
	b := newFakeBackend(t, repo)

	big := addTask(t, s, store.NewTask{Title: "large implementation"})
	b.agent = largeCommitter(140 * 1024)
	if _, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx); err != nil {
		t.Fatalf("run large dependency: %v", err)
	}
	bigDone, err := s.Get(ctx, big.ID)
	if err != nil {
		t.Fatalf("get large dependency: %v", err)
	}

	small := addTask(t, s, store.NewTask{Title: "small implementation"})
	b.agent = committer(t)
	if _, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx); err != nil {
		t.Fatalf("run small dependency: %v", err)
	}
	smallDone, err := s.Get(ctx, small.ID)
	if err != nil {
		t.Fatalf("get small dependency: %v", err)
	}
	if bigDone.Branch == "" || smallDone.Branch == "" {
		t.Fatalf("dependencies = (%+v, %+v), want both with branches", bigDone, smallDone)
	}

	review := addTask(t, s, store.NewTask{
		Title: "review both",
		Role:  "review",
		Deps:  []int64{big.ID, small.ID},
	})

	var diff string
	b.agent = captureReviewDiff(verdictAgent(`{"verdict":"approve","notes":[]}`), &diff)
	stats, err := newRunner(t, s, b, func(c *runner.Config) { c.MaxTasks = 1 }).Run(ctx)
	if err != nil {
		t.Fatalf("run review: %v", err)
	}
	if stats.Completed != 1 {
		t.Fatalf("stats = %+v, want the review done", stats)
	}
	if got, err := s.Get(ctx, review.ID); err != nil || got.Status != store.Done {
		t.Fatalf("review = %+v, err = %v, want it done", got, err)
	}

	const argMax = 128 * 1024
	prompt := b.promptText("w3")
	if len(prompt) >= argMax/4 {
		t.Errorf("review prompt = %d bytes, want far below the %d-byte argument limit", len(prompt), argMax)
	}
	for _, want := range []string{"## The change under review", ".loa/review.diff", bigDone.Branch, smallDone.Branch} {
		if !strings.Contains(prompt, want) {
			t.Errorf("review prompt does not contain %q:\n%s", want, prompt)
		}
	}

	if len(diff) <= argMax {
		t.Fatalf("review.diff = %d bytes, want more than the argument limit", len(diff))
	}
	for _, want := range []string{bigDone.Branch, smallDone.Branch, "work-1.1.txt", "work-2.1.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("review.diff does not contain %q:\n%s", want, diff)
		}
	}
}
