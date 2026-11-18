package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskflow/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func add(t *testing.T, s *store.Store, task store.NewTask) int64 {
	t.Helper()
	id, err := s.Add(context.Background(), task)
	if err != nil {
		t.Fatalf("add %q: %v", task.Title, err)
	}
	return id
}

func lease(t *testing.T, s *store.Store, now time.Time) *store.Task {
	t.Helper()
	task, err := s.Lease(context.Background(), "test", time.Hour, now)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return task
}

func TestAddRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{
		Title:    "fix the flaky test",
		Body:     "reproduce it first",
		Priority: 7,
		Scopes:   []string{"repo:taskflow", "repo:taskflow/ci"},
	})

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Title != "fix the flaky test" || task.Body != "reproduce it first" {
		t.Errorf("text not round-tripped: %+v", task)
	}
	if task.Status != store.New || task.Priority != 7 || task.Attempts != 0 || task.MaxAttempts != 2 {
		t.Errorf("unexpected initial state: %+v", task)
	}
	if len(task.Scopes) != 2 {
		t.Errorf("scopes = %v, want 2", task.Scopes)
	}
	if _, err := s.Get(ctx, id+999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get missing task: %v, want ErrNotFound", err)
	}
}

func TestAddRejectsBlankFields(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	if _, err := s.Add(ctx, store.NewTask{Title: "  "}); err == nil {
		t.Error("blank title accepted")
	}
	if _, err := s.Add(ctx, store.NewTask{Title: "ok", Scopes: []string{""}}); err == nil {
		t.Error("blank scope accepted")
	}
}

func TestLeaseFollowsPriorityThenID(t *testing.T) {
	s := open(t)

	low := add(t, s, store.NewTask{Title: "low", Priority: 1})
	high := add(t, s, store.NewTask{Title: "high", Priority: 9})
	alsoLow := add(t, s, store.NewTask{Title: "also low", Priority: 1})

	now := time.Now()
	for _, want := range []int64{high, low, alsoLow} {
		got := lease(t, s, now)
		if got == nil || got.ID != want {
			t.Fatalf("leased %v, want id %d", got, want)
		}
		if got.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", got.Attempts)
		}
	}
	if got := lease(t, s, now); got != nil {
		t.Fatalf("leased %v from an exhausted queue", got)
	}
}

func TestLeaseExcludesSharedScopes(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	slow := add(t, s, store.NewTask{Title: "edit the parser", Priority: 10, Scopes: []string{"repo:taskflow"}})
	contending := add(t, s, store.NewTask{Title: "edit the lexer", Priority: 9, Scopes: []string{"repo:taskflow"}})
	unrelated := add(t, s, store.NewTask{Title: "bump the flake", Priority: 1, Scopes: []string{"repo:nixos"}})

	now := time.Now()
	if got := lease(t, s, now); got == nil || got.ID != slow {
		t.Fatalf("first lease = %v, want %d", got, slow)
	}
	// contending shares a scope with the leased task, so the unrelated one wins
	// despite its lower priority.
	if got := lease(t, s, now); got == nil || got.ID != unrelated {
		t.Fatalf("second lease = %v, want %d", got, unrelated)
	}
	if err := s.Complete(ctx, slow, "b/fix-parser", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := lease(t, s, now); got == nil || got.ID != contending {
		t.Fatalf("third lease = %v, want %d", got, contending)
	}
}

func TestLeaseWaitsForDependencies(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	first := add(t, s, store.NewTask{Title: "schema"})
	second := add(t, s, store.NewTask{Title: "writer", Deps: []int64{first}})

	now := time.Now()
	if got := lease(t, s, now); got == nil || got.ID != first {
		t.Fatalf("first lease = %v, want %d", got, first)
	}
	if got := lease(t, s, now); got != nil {
		t.Fatalf("leased %d while its dependency was unfinished", got.ID)
	}
	if err := s.Complete(ctx, first, "b/schema", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := lease(t, s, now); got == nil || got.ID != second {
		t.Fatalf("lease after dependency = %v, want %d", got, second)
	}
}

func TestExpiredLeaseRetriesThenBlocks(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Title: "long job", MaxTries: 2})
	start := time.Now()

	if got := lease(t, s, start); got == nil || got.ID != id || got.Attempts != 1 {
		t.Fatalf("first lease = %v", got)
	}
	// The owner dies; the lease lapses, the task waits out the retry delay, and
	// then runs again.
	if got := lease(t, s, start.Add(2*time.Hour)); got != nil {
		t.Fatalf("leased %v before the retry delay elapsed", got)
	}
	if got := lease(t, s, start.Add(2*time.Hour+store.RetryDelay)); got == nil || got.ID != id || got.Attempts != 2 {
		t.Fatalf("retry lease = %v", got)
	}
	// Out of attempts: the next reap blocks it instead of retrying.
	if got := lease(t, s, start.Add(4*time.Hour+2*store.RetryDelay)); got != nil {
		t.Fatalf("leased %v after attempts were spent", got)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Blocked {
		t.Errorf("status = %s, want blocked", task.Status)
	}
	if !task.LeaseExpiry.IsZero() || task.LeaseOwner != "" {
		t.Errorf("blocked task still holds a lease: %+v", task)
	}
}

func TestFailRequeuesUntilAttemptsAreSpent(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Title: "flaky", MaxTries: 2})
	now := time.Now()

	lease(t, s, now)
	if err := s.Fail(ctx, id, "compiler blew up"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.New || task.Result != "compiler blew up" {
		t.Fatalf("after first failure: %+v", task)
	}
	if got := lease(t, s, now); got != nil {
		t.Fatalf("task was claimable %v before its retry delay elapsed", got)
	}

	lease(t, s, now.Add(store.RetryDelay))
	if err := s.Fail(ctx, id, "again"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Blocked {
		t.Fatalf("after second failure: %+v", task)
	}
	if !task.NotBefore.IsZero() {
		t.Errorf("blocked task still carries a retry time: %+v", task.NotBefore)
	}
}

func TestFinishRequiresLease(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Title: "unclaimed"})
	if err := s.Complete(ctx, id, "b/x", "done"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("complete unleased: %v, want ErrNotLeased", err)
	}
	if err := s.Fail(ctx, id, "nope"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("fail unleased: %v, want ErrNotLeased", err)
	}
}

func TestConcurrentLeasesNeverDuplicate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	// Two handles, two connections: the same file contention the runner and a
	// CLI invocation would produce.
	a, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close()
	b, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	const tasks = 12
	for i := 0; i < tasks; i++ {
		add(t, a, store.NewTask{Title: fmt.Sprintf("task %d", i)})
	}

	var mu sync.Mutex
	leased := map[int64]int{}
	var wg sync.WaitGroup

	for _, s := range []*store.Store{a, b} {
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(s *store.Store, worker int) {
				defer wg.Done()
				for {
					task, err := s.Lease(ctx, fmt.Sprintf("w%d", worker), time.Minute, time.Now())
					if err != nil {
						t.Errorf("lease: %v", err)
						return
					}
					if task == nil {
						return
					}
					mu.Lock()
					leased[task.ID]++
					mu.Unlock()
				}
			}(s, w)
		}
	}
	wg.Wait()

	if len(leased) != tasks {
		t.Fatalf("%d distinct tasks leased, want %d (map: %v)", len(leased), tasks, leased)
	}
	for id, n := range leased {
		if n != 1 {
			t.Errorf("task %d was leased %d times", id, n)
		}
	}
}

func TestReopenIsStable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	id := add(t, first, store.NewTask{Title: "survives"})
	first.Close()

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	tasks, err := second.List(ctx, store.New)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("reopened queue = %+v", tasks)
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	a := add(t, s, store.NewTask{Title: "a", Priority: 1})
	b := add(t, s, store.NewTask{Title: "b", Priority: 5})
	done := add(t, s, store.NewTask{Title: "c", Priority: 1})
	lease(t, s, time.Now())
	if err := s.Complete(ctx, b, "b/done", "shipped"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 || all[0].ID != b {
		t.Fatalf("unfiltered list = %+v", all)
	}

	fresh, err := s.List(ctx, store.New)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(fresh) != 2 || fresh[0].ID != a || fresh[1].ID != done {
		t.Fatalf("new tasks = %+v", fresh)
	}

	if _, err := s.Get(ctx, done); err != nil {
		t.Fatalf("get: %v", err)
	}
}

func TestDatabaseIsCreatedOwnerOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("new database mode = %o, want 600", perm)
	}
}

func TestExistingDatabaseModeIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	// Somebody chose to share this database; opening it must not narrow that
	// silently, and must not fail either.
	// Deliberately world-readable: the test proves Open does not narrow it.
	if err := os.WriteFile(path, nil, 0o644); err != nil { //nolint:gosec // G306: see above
		t.Fatalf("create: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: see above
		t.Fatalf("chmod: %v", err)
	}

	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644 kept", perm)
	}
}

// TestBlockersAgreeWithLease pins the explanation to the claim predicate. If
// they disagree, "show" lies about why a task is not running.
func TestBlockersAgreeWithLease(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	type setup func(t *testing.T, s *store.Store) int64
	cases := map[string]setup{
		"runnable": func(t *testing.T, s *store.Store) int64 {
			return add(t, s, store.NewTask{Title: "free"})
		},
		"unfinished dependency": func(t *testing.T, s *store.Store) int64 {
			dep := add(t, s, store.NewTask{Title: "dep"})
			task := add(t, s, store.NewTask{Title: "blocked", Deps: []int64{dep}})
			lease(t, s, now) // takes the dep, the only other claimable task
			return task
		},
		"retry delay": func(t *testing.T, s *store.Store) int64 {
			task := add(t, s, store.NewTask{Title: "flaky"})
			lease(t, s, now)
			if err := s.Fail(ctx, task, "boom"); err != nil {
				t.Fatalf("fail: %v", err)
			}
			return task
		},
		"scope contention": func(t *testing.T, s *store.Store) int64 {
			holder := add(t, s, store.NewTask{Title: "holder", Scopes: []string{"repo:a"}})
			task := add(t, s, store.NewTask{Title: "contender", Scopes: []string{"repo:a"}})
			if got := lease(t, s, now); got == nil || got.ID != holder {
				t.Fatalf("holder was not leased: %v", got)
			}
			return task
		},
		"blocked": func(t *testing.T, s *store.Store) int64 {
			task := add(t, s, store.NewTask{Title: "dead", MaxTries: 1})
			lease(t, s, now)
			if err := s.Fail(ctx, task, "out of attempts"); err != nil {
				t.Fatalf("fail: %v", err)
			}
			return task
		},
		"done": func(t *testing.T, s *store.Store) int64 {
			task := add(t, s, store.NewTask{Title: "finished"})
			lease(t, s, now)
			if err := s.Complete(ctx, task, "b/done", "ok"); err != nil {
				t.Fatalf("complete: %v", err)
			}
			return task
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			id := build(t, s)

			blockers, err := s.Blockers(ctx, id, now)
			if err != nil {
				t.Fatalf("blockers: %v", err)
			}
			claimed := lease(t, s, now)

			runnable := len(blockers) == 0
			isNext := claimed != nil && claimed.ID == id
			if runnable != isNext {
				t.Fatalf("blockers %v but next lease = %v", blockers, claimed)
			}
		})
	}
}

func TestBlockersNameTheReason(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()

	dep := add(t, s, store.NewTask{Title: "schema"})
	child := add(t, s, store.NewTask{Title: "writer", Scopes: []string{"repo:a"}, Deps: []int64{dep}})
	lease(t, s, now)

	blockers, err := s.Blockers(ctx, child, now)
	if err != nil {
		t.Fatalf("blockers: %v", err)
	}
	want := "waiting on #" + strconv.FormatInt(dep, 10)
	if len(blockers) != 1 || !strings.Contains(blockers[0], want) {
		t.Fatalf("blockers = %v, want it to mention %q", blockers, want)
	}
}
