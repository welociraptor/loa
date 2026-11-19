package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"loa/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "tasks.db"))
}

func openAt(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// backdate sets a task's updated_at directly: timestamps have one-second
// resolution, so this is the only way to make "finished last" deterministic
// without sleeping between completions.
func backdate(t *testing.T, path string, id int64, when time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE tasks SET updated_at = ? WHERE id = ?`, when.UTC().Format(time.RFC3339), id); err != nil {
		t.Fatalf("backdate %d: %v", id, err)
	}
}

func add(t *testing.T, s *store.Store, task store.NewTask) int64 {
	t.Helper()
	id, err := s.Add(context.Background(), task)
	if err != nil {
		t.Fatalf("add %q: %v", task.Title, err)
	}
	return id
}

func leaseFor(t *testing.T, s *store.Store, owner string, ttl time.Duration, now time.Time) *store.Task {
	t.Helper()
	task, err := s.Lease(context.Background(), owner, ttl, now)
	if err != nil {
		t.Fatalf("lease as %q: %v", owner, err)
	}
	return task
}

func lease(t *testing.T, s *store.Store, now time.Time) *store.Task {
	t.Helper()
	return leaseFor(t, s, "test", time.Hour, now)
}

// taskTitles lists the tasks currently in the queue, in queue order. It is how
// the rollback tests prove a failed insert left no row behind.
func taskTitles(t *testing.T, s *store.Store) []string {
	t.Helper()
	tasks, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	titles := make([]string, len(tasks))
	for i := range tasks {
		titles[i] = tasks[i].Title
	}
	return titles
}

func blockersAt(t *testing.T, s *store.Store, id int64, now time.Time) []string {
	t.Helper()
	blockers, err := s.Blockers(context.Background(), id, now)
	if err != nil {
		t.Fatalf("blockers of %d: %v", id, err)
	}
	return blockers
}

func TestAddRoundTrip(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{
		Title:     "fix the flaky test",
		Body:      "reproduce it first",
		Workspace: "loa",
		Priority:  7,
		Scopes:    []string{"repo:loa", "repo:loa/ci"},
	})

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Title != "fix the flaky test" || task.Body != "reproduce it first" {
		t.Errorf("text not round-tripped: %+v", task)
	}
	if task.Workspace != "loa" {
		t.Errorf("workspace = %q, want loa", task.Workspace)
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

	if _, err := s.Add(ctx, store.NewTask{Workspace: "loa", Title: "  "}); err == nil {
		t.Error("blank title accepted")
	}
	if _, err := s.Add(ctx, store.NewTask{Workspace: "loa", Title: "ok", Scopes: []string{""}}); err == nil {
		t.Error("blank scope accepted")
	}
}

func TestAddDefaultsMaxAttempts(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		maxTries int
		want     int
	}{
		{"zero", 0, 2},
		{"negative", -3, 2},
		{"one", 1, 1},
		{"explicit", 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := add(t, s, store.NewTask{Workspace: "loa", Title: tc.name, MaxTries: tc.maxTries})
			task, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if task.MaxAttempts != tc.want {
				t.Errorf("max attempts = %d, want %d", task.MaxAttempts, tc.want)
			}
		})
	}
}

func TestAddRejectsMissingWorkspace(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	for _, workspace := range []string{"", "   ", "\t\n"} {
		if _, err := s.Add(ctx, store.NewTask{Title: "no workspace", Workspace: workspace}); err == nil {
			t.Errorf("workspace %q accepted", workspace)
		}
	}
	if got := taskTitles(t, s); len(got) != 0 {
		t.Errorf("rejected adds left %v behind", got)
	}
}

func TestAddRollsBackOnScopeOrDependencyFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("duplicate scope", func(t *testing.T) {
		s := open(t)
		_, err := s.Add(ctx, store.NewTask{
			Title: "duplicate scope", Workspace: "loa",
			Scopes: []string{"repo:loa", "repo:loa"},
		})
		if err == nil {
			t.Fatal("duplicate scope accepted")
		}
		if got := taskTitles(t, s); len(got) != 0 {
			t.Errorf("failed add left %v behind", got)
		}
	})

	t.Run("empty scope", func(t *testing.T) {
		s := open(t)
		_, err := s.Add(ctx, store.NewTask{
			Title: "empty scope", Workspace: "loa",
			Scopes: []string{"repo:loa", "  "},
		})
		if err == nil {
			t.Fatal("empty scope accepted")
		}
		if got := taskTitles(t, s); len(got) != 0 {
			t.Errorf("failed add left %v behind", got)
		}
	})

	t.Run("duplicate dependency", func(t *testing.T) {
		s := open(t)
		dep := add(t, s, store.NewTask{Title: "dep", Workspace: "loa"})
		_, err := s.Add(ctx, store.NewTask{
			Title: "duplicate dep", Workspace: "loa",
			Deps: []int64{dep, dep},
		})
		if err == nil {
			t.Fatal("duplicate dependency accepted")
		}
		if got := taskTitles(t, s); !slices.Equal(got, []string{"dep"}) {
			t.Errorf("failed add changed the queue to %v", got)
		}
	})

	t.Run("missing dependency", func(t *testing.T) {
		s := open(t)
		add(t, s, store.NewTask{Title: "dep", Workspace: "loa"})
		_, err := s.Add(ctx, store.NewTask{
			Title: "missing dep", Workspace: "loa", Deps: []int64{999},
		})
		if err == nil {
			t.Fatal("dependency on a missing task accepted")
		}
		if got := taskTitles(t, s); !slices.Equal(got, []string{"dep"}) {
			t.Errorf("failed add changed the queue to %v", got)
		}
	})
}

func TestLeaseFollowsPriorityThenID(t *testing.T) {
	s := open(t)

	low := add(t, s, store.NewTask{Workspace: "loa", Title: "low", Priority: 1})
	high := add(t, s, store.NewTask{Workspace: "loa", Title: "high", Priority: 9})
	alsoLow := add(t, s, store.NewTask{Workspace: "loa", Title: "also low", Priority: 1})

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

	slow := add(t, s, store.NewTask{Workspace: "loa", Title: "edit the parser", Priority: 10, Scopes: []string{"repo:loa"}})
	contending := add(t, s, store.NewTask{Workspace: "loa", Title: "edit the lexer", Priority: 9, Scopes: []string{"repo:loa"}})
	unrelated := add(t, s, store.NewTask{Workspace: "loa", Title: "bump the flake", Priority: 1, Scopes: []string{"repo:nixos"}})

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

func TestLeaseValidatesOwnerAndTTL(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := add(t, s, store.NewTask{Title: "work", Workspace: "loa"})
	now := time.Now()

	if _, err := s.Lease(ctx, "", time.Hour, now); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := s.Lease(ctx, "test", 0, now); err == nil {
		t.Error("zero ttl accepted")
	}
	if _, err := s.Lease(ctx, "test", -time.Second, now); err == nil {
		t.Error("negative ttl accepted")
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.New || task.Attempts != 0 || task.LeaseOwner != "" {
		t.Errorf("a refused lease changed the task: %+v", task)
	}
}

func TestLeaseReapsAtTheExpiryBoundary(t *testing.T) {
	const ttl = time.Hour
	start := time.Now().Truncate(time.Second)

	cases := []struct {
		name       string
		now        time.Time
		wantReaped bool
	}{
		{"at expiry", start.Add(ttl), true},
		// Stored timestamps have one-second resolution. T+1ns formats to T and is
		// therefore reaped; T-1ns formats to T-1s and is still held.
		{"within the expiry second", start.Add(ttl).Add(time.Nanosecond), true},
		{"just before expiry", start.Add(ttl).Add(-time.Nanosecond), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			ctx := context.Background()
			id := add(t, s, store.NewTask{Title: "slow", Workspace: "loa"})

			if got := leaseFor(t, s, "test", ttl, start); got == nil || got.ID != id {
				t.Fatalf("initial lease = %v, want %d", got, id)
			}
			if got := leaseFor(t, s, "test", ttl, tc.now); got != nil {
				t.Fatalf("second lease = %v, want nothing claimable", got)
			}

			task, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if tc.wantReaped {
				if task.Status != store.New {
					t.Errorf("status = %s, want new", task.Status)
				}
				if want := start.Add(ttl).Add(store.RetryDelay); !task.NotBefore.Equal(want) {
					t.Errorf("not_before = %s, want %s", task.NotBefore, want)
				}
				if task.LeaseOwner != "" || !task.LeaseExpiry.IsZero() {
					t.Errorf("reaped task still holds a lease: %+v", task)
				}
			} else {
				if task.Status != store.Leased || task.LeaseOwner != "test" {
					t.Errorf("status/owner = %s/%q, want leased/test", task.Status, task.LeaseOwner)
				}
				if want := start.Add(ttl); !task.LeaseExpiry.Equal(want) {
					t.Errorf("lease expiry = %s, want %s", task.LeaseExpiry, want)
				}
			}
		})
	}
}

func TestLeaseRetryBoundary(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Title: "flaky", Workspace: "loa"})
	lease(t, s, time.Now())
	if err := s.Fail(ctx, id, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	retryAt := task.NotBefore
	if retryAt.IsZero() {
		t.Fatal("failure did not delay the retry")
	}

	if got := lease(t, s, retryAt.Add(-time.Second)); got != nil {
		t.Fatalf("leased %v before the retry time", got)
	}
	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.New || !task.NotBefore.Equal(retryAt) {
		t.Fatalf("early lease changed the task: %+v", task)
	}

	if got := lease(t, s, retryAt); got == nil || got.ID != id || got.Attempts != 2 {
		t.Fatalf("lease at the retry time = %v, want task %d with 2 attempts", got, id)
	}
}

func TestLeaseSharedScopeSymmetry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	holder := add(t, s, store.NewTask{Title: "holder", Workspace: "loa", Priority: 50, Scopes: []string{"s1", "s2"}})
	sharesOne := add(t, s, store.NewTask{Title: "shares one", Workspace: "loa", Priority: 40, Scopes: []string{"s1"}})
	sharesTwo := add(t, s, store.NewTask{Title: "shares two", Workspace: "loa", Priority: 30, Scopes: []string{"s2"}})
	freeA := add(t, s, store.NewTask{Title: "free a", Workspace: "loa", Priority: 20})
	freeB := add(t, s, store.NewTask{Title: "free b", Workspace: "loa", Priority: 10})

	if got := lease(t, s, now); got == nil || got.ID != holder {
		t.Fatalf("first lease = %v, want holder %d", got, holder)
	}
	// A task with no scopes is never excluded by someone else's scope.
	if got := lease(t, s, now); got == nil || got.ID != freeA {
		t.Fatalf("second lease = %v, want %d", got, freeA)
	}
	// The holder is leased, so it cannot be leased a second time.
	if got := lease(t, s, now); got == nil || got.ID != freeB {
		t.Fatalf("third lease = %v, want %d", got, freeB)
	}
	if got := lease(t, s, now); got != nil {
		t.Fatalf("leased %v while both scopes were held", got)
	}

	if err := s.Complete(ctx, holder, "b/holder", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Both one-scope tasks become runnable; neither excludes the other.
	if got := lease(t, s, now); got == nil || got.ID != sharesOne {
		t.Fatalf("lease after the holder finished = %v, want %d", got, sharesOne)
	}
	if got := lease(t, s, now); got == nil || got.ID != sharesTwo {
		t.Fatalf("last lease = %v, want %d", got, sharesTwo)
	}
	if got := lease(t, s, now); got != nil {
		t.Fatalf("leased %v from an exhausted queue", got)
	}
}

func TestLeaseWaitsForDependencies(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	first := add(t, s, store.NewTask{Workspace: "loa", Title: "schema"})
	second := add(t, s, store.NewTask{Workspace: "loa", Title: "writer", Deps: []int64{first}})

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

	id := add(t, s, store.NewTask{Workspace: "loa", Title: "long job", MaxTries: 2})
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

	id := add(t, s, store.NewTask{Workspace: "loa", Title: "flaky", MaxTries: 2})
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

	// Fail stamps not_before from the real clock, so the retry lease has to be
	// measured from now, not from the test's earlier reference time.
	lease(t, s, time.Now().Add(store.RetryDelay+time.Minute))
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

func TestFailSetsRunTimeAndNotBefore(t *testing.T) {
	ctx := context.Background()

	t.Run("requeued", func(t *testing.T) {
		s := open(t)
		id := add(t, s, store.NewTask{Title: "flaky", Workspace: "loa", MaxTries: 2})
		// The attempt started two seconds ago; the lease is still valid because
		// its expiry is an hour ahead.
		leaseFor(t, s, "test", time.Hour, time.Now().Add(-2*time.Second))

		before := time.Now()
		if err := s.Fail(ctx, id, "compiler blew up"); err != nil {
			t.Fatalf("fail: %v", err)
		}
		after := time.Now()

		task, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if task.Status != store.New || task.Result != "compiler blew up" {
			t.Errorf("after failure: %+v", task)
		}
		if task.RunMS < 1900 || task.RunMS > 4000 {
			t.Errorf("run time = %dms, want about two seconds", task.RunMS)
		}
		low := before.Add(store.RetryDelay).Add(-time.Second)
		high := after.Add(store.RetryDelay)
		if task.NotBefore.Before(low) || task.NotBefore.After(high) {
			t.Errorf("not_before = %s, want about %s", task.NotBefore, before.Add(store.RetryDelay))
		}
	})

	t.Run("blocked", func(t *testing.T) {
		s := open(t)
		id := add(t, s, store.NewTask{Title: "final", Workspace: "loa", MaxTries: 1})
		leaseFor(t, s, "test", time.Hour, time.Now().Add(-2*time.Second))

		if err := s.Fail(ctx, id, "gave up"); err != nil {
			t.Fatalf("fail: %v", err)
		}
		task, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if task.Status != store.Blocked {
			t.Errorf("status = %s, want blocked", task.Status)
		}
		if !task.NotBefore.IsZero() {
			t.Errorf("blocked task still carries a retry time: %s", task.NotBefore)
		}
		if task.RunMS < 1900 || task.RunMS > 4000 {
			t.Errorf("run time = %dms, want about two seconds", task.RunMS)
		}
	})
}

func TestFinishRequiresLease(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Workspace: "loa", Title: "unclaimed"})
	if err := s.Complete(ctx, id, "b/x", "done"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("complete unleased: %v, want ErrNotLeased", err)
	}
	if err := s.Fail(ctx, id, "nope"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("fail unleased: %v, want ErrNotLeased", err)
	}
}

func TestUnblockBuysExactlyOneAttempt(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Title: "out of luck", Workspace: "loa", MaxTries: 2})
	if got := lease(t, s, time.Now()); got == nil || got.ID != id || got.Attempts != 1 {
		t.Fatalf("first lease = %v", got)
	}
	if err := s.Fail(ctx, id, "first failure"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	// Fail stamps the retry delay from the real clock, so measure from now.
	if got := lease(t, s, time.Now().Add(store.RetryDelay+time.Minute)); got == nil || got.ID != id || got.Attempts != 2 {
		t.Fatalf("second lease = %v, want task %d with 2 attempts", got, id)
	}
	if err := s.Fail(ctx, id, "second failure"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Blocked || task.Attempts != 2 {
		t.Fatalf("before unblock: %+v", task)
	}

	if err := s.Unblock(ctx, id); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.New {
		t.Errorf("status after unblock = %s, want new", task.Status)
	}
	if task.Attempts != 2 {
		t.Errorf("attempts after unblock = %d, want 2 kept", task.Attempts)
	}
	if !task.NotBefore.IsZero() || task.LeaseOwner != "" || !task.LeaseExpiry.IsZero() {
		t.Errorf("unblocked task still carries lease or retry state: %+v", task)
	}

	// The next lease spends the one attempt the human paid for.
	if got := lease(t, s, time.Now()); got == nil || got.ID != id || got.Attempts != 3 {
		t.Fatalf("post-unblock lease = %v, want task %d with 3 attempts", got, id)
	}
	if err := s.Fail(ctx, id, "third failure"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Blocked || task.Attempts != 3 {
		t.Errorf("after the bought attempt failed: %+v", task)
	}
	if !task.NotBefore.IsZero() {
		t.Errorf("blocked task still carries a retry time: %s", task.NotBefore)
	}
}

func TestUnblockRefusesNonBlocked(t *testing.T) {
	ctx := context.Background()

	cases := map[string]func(t *testing.T, s *store.Store) int64{
		"new": func(t *testing.T, s *store.Store) int64 {
			return add(t, s, store.NewTask{Title: "fresh", Workspace: "loa"})
		},
		"leased": func(t *testing.T, s *store.Store) int64 {
			id := add(t, s, store.NewTask{Title: "running", Workspace: "loa"})
			lease(t, s, time.Now())
			return id
		},
		"done": func(t *testing.T, s *store.Store) int64 {
			id := add(t, s, store.NewTask{Title: "finished", Workspace: "loa"})
			lease(t, s, time.Now())
			if err := s.Complete(ctx, id, "b/done", "ok"); err != nil {
				t.Fatalf("complete: %v", err)
			}
			return id
		},
		"missing": func(*testing.T, *store.Store) int64 {
			return 999
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			id := build(t, s)
			// A missing id reports ErrNotLeased because no row matched the
			// update; ErrNotFound would be more precise if a caller ever needs
			// to tell the two apart.
			if err := s.Unblock(ctx, id); !errors.Is(err, store.ErrNotLeased) {
				t.Errorf("unblock %s: %v, want ErrNotLeased", name, err)
			}
		})
	}
}

func TestResolveSettlesOutsideTheLease(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	doneTask := add(t, s, store.NewTask{Title: "done", Workspace: "loa", Priority: 30})
	leaseFor(t, s, "test", time.Hour, time.Now().Add(-2*time.Second))
	if err := s.Complete(ctx, doneTask, "b/done", "shipped"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	before, err := s.Get(ctx, doneTask)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.RunMS == 0 {
		t.Fatalf("test setup: run time is zero: %+v", before)
	}

	blockedTask := add(t, s, store.NewTask{Title: "blocked", Workspace: "loa", Priority: 20, MaxTries: 1})
	lease(t, s, time.Now())
	if err := s.Fail(ctx, blockedTask, "needs a human"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	newTask := add(t, s, store.NewTask{Title: "new", Workspace: "loa", Priority: 10})

	if err := s.Resolve(ctx, newTask, true, "approved by hand"); err != nil {
		t.Fatalf("resolve new: %v", err)
	}
	task, err := s.Get(ctx, newTask)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Done || task.Result != "approved by hand" || task.RunMS != 0 {
		t.Errorf("resolved new task: %+v", task)
	}

	if err := s.Resolve(ctx, blockedTask, false, "still broken"); err != nil {
		t.Fatalf("resolve blocked as blocked: %v", err)
	}
	task, err = s.Get(ctx, blockedTask)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Blocked || task.Result != "still broken" {
		t.Errorf("resolved blocked task: %+v", task)
	}

	if err := s.Resolve(ctx, blockedTask, true, "fixed by hand"); err != nil {
		t.Fatalf("resolve blocked as done: %v", err)
	}
	task, err = s.Get(ctx, blockedTask)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Done || task.Result != "fixed by hand" {
		t.Errorf("resolved blocked task as done: %+v", task)
	}

	if err := s.Resolve(ctx, doneTask, false, "reopened"); err != nil {
		t.Fatalf("resolve done as blocked: %v", err)
	}
	task, err = s.Get(ctx, doneTask)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Status != store.Blocked || task.Result != "reopened" {
		t.Errorf("resolved done task: %+v", task)
	}
	if task.RunMS != before.RunMS {
		t.Errorf("resolving changed the run time from %dms to %dms", before.RunMS, task.RunMS)
	}

	if err := s.Resolve(ctx, 999, true, "nothing"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("resolve missing: %v, want ErrNotLeased", err)
	}
}

func TestResolveRefusesALeasedTask(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id := add(t, s, store.NewTask{Title: "running", Workspace: "loa"})
	lease(t, s, time.Now())
	before, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	for _, done := range []bool{true, false} {
		if err := s.Resolve(ctx, id, done, "late answer"); !errors.Is(err, store.ErrNotLeased) {
			t.Errorf("resolve done=%t: %v, want ErrNotLeased", done, err)
		}
	}

	after, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != store.Leased || after.Result != before.Result {
		t.Errorf("a refused resolve changed status/result: %+v", after)
	}
	if after.LeaseOwner != before.LeaseOwner || !after.LeaseExpiry.Equal(before.LeaseExpiry) {
		t.Errorf("a refused resolve changed the lease: %+v", after)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a refused resolve changed updated_at: %s -> %s", before.UpdatedAt, after.UpdatedAt)
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
		add(t, a, store.NewTask{Workspace: "loa", Title: fmt.Sprintf("task %d", i)})
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
	id := add(t, first, store.NewTask{Workspace: "loa", Title: "survives"})
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

	a := add(t, s, store.NewTask{Workspace: "loa", Title: "a", Priority: 1})
	b := add(t, s, store.NewTask{Workspace: "loa", Title: "b", Priority: 5})
	done := add(t, s, store.NewTask{Workspace: "loa", Title: "c", Priority: 1})
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

func TestListMultipleStatuses(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()

	blockedTask := add(t, s, store.NewTask{Title: "blocked", Workspace: "loa", Priority: 100, MaxTries: 1})
	doneTask := add(t, s, store.NewTask{Title: "done", Workspace: "loa", Priority: 75})
	leasedTask := add(t, s, store.NewTask{Title: "leased", Workspace: "loa", Priority: 50})
	newHigh := add(t, s, store.NewTask{Title: "new high", Workspace: "loa", Priority: 10})
	newLow := add(t, s, store.NewTask{Title: "new low", Workspace: "loa", Priority: 1})

	if got := lease(t, s, now); got == nil || got.ID != blockedTask {
		t.Fatalf("first lease = %v, want %d", got, blockedTask)
	}
	if err := s.Fail(ctx, blockedTask, "out of attempts"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got := lease(t, s, now); got == nil || got.ID != doneTask {
		t.Fatalf("second lease = %v, want %d", got, doneTask)
	}
	if err := s.Complete(ctx, doneTask, "b/done", "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := lease(t, s, now); got == nil || got.ID != leasedTask {
		t.Fatalf("third lease = %v, want %d", got, leasedTask)
	}

	tasks, err := s.List(ctx, store.New, store.Blocked)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []int64
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	if want := []int64{blockedTask, newHigh, newLow}; !slices.Equal(ids, want) {
		t.Errorf("new+blocked ids = %v, want %v in priority order", ids, want)
	}

	// An unknown status is a filter that matches nothing, not an error.
	unknown, err := s.List(ctx, store.Status("weird"))
	if err != nil {
		t.Fatalf("list unknown status: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown status returned %+v", unknown)
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

func TestOpenRejectsBadPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := store.Open(ctx, ""); err == nil {
		t.Error("empty path accepted")
	}

	if _, err := store.Open(ctx, filepath.Join(dir, "missing", "tasks.db")); err == nil {
		t.Error("missing parent directory accepted")
	}

	asDir := filepath.Join(dir, "tasks.db")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := store.Open(ctx, asDir); err == nil {
		t.Error("directory accepted as a database")
	}

	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	if _, err := store.Open(ctx, corrupt); err == nil {
		t.Error("corrupt file accepted as a database")
	}

	future := filepath.Join(dir, "future.db")
	db, err := sql.Open("sqlite", future)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 11"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	db.Close()
	if _, err := store.Open(ctx, future); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Errorf("future schema: %v, want a refusal", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()

	id := add(t, first, store.NewTask{Title: "survives", Workspace: "loa"})

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer second.Close()

	tasks, err := second.List(ctx)
	if err != nil {
		t.Fatalf("list through second handle: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("second handle sees %+v", tasks)
	}

	if err := second.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if _, err := first.Get(ctx, id); err != nil {
		t.Fatalf("get through the handle still open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first handle: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first handle again: %v", err)
	}

	third, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer third.Close()
	if _, err := third.Get(ctx, id); err != nil {
		t.Fatalf("get after reopening: %v", err)
	}
}

// TestClosedStoreRefusesWork pins that every queue operation fails once the
// handle is closed, rather than silently acting on a database it no longer has.
func TestClosedStoreRefusesWork(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := add(t, s, store.NewTask{Title: "closed", Workspace: "loa"})
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ops := map[string]func() error{
		"add": func() error {
			_, err := s.Add(ctx, store.NewTask{Title: "later", Workspace: "loa"})
			return err
		},
		"apply": func() error {
			_, err := s.Apply(ctx, []store.BatchItem{{NewTask: store.NewTask{Title: "later", Workspace: "loa"}}}, nil)
			return err
		},
		"get":        func() error { _, err := s.Get(ctx, id); return err },
		"list":       func() error { _, err := s.List(ctx); return err },
		"lease":      func() error { _, err := s.Lease(ctx, "test", time.Hour, time.Now()); return err },
		"complete":   func() error { return s.Complete(ctx, id, "b/x", "ok") },
		"fail":       func() error { return s.Fail(ctx, id, "nope") },
		"attach":     func() error { return s.Attach(ctx, id, "w1", "w1:p1") },
		"unblock":    func() error { return s.Unblock(ctx, id) },
		"resolve":    func() error { return s.Resolve(ctx, id, true, "late") },
		"claims":     func() error { _, err := s.Claims(ctx); return err },
		"deps":       func() error { _, err := s.Deps(ctx, id); return err },
		"dependents": func() error { _, err := s.Dependents(ctx, id); return err },
		"blockers":   func() error { _, err := s.Blockers(ctx, id, time.Now()); return err },
	}

	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); err == nil {
				t.Errorf("%s on a closed store succeeded", name)
			}
		})
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
			return add(t, s, store.NewTask{Workspace: "loa", Title: "free"})
		},
		"unfinished dependency": func(t *testing.T, s *store.Store) int64 {
			dep := add(t, s, store.NewTask{Workspace: "loa", Title: "dep"})
			task := add(t, s, store.NewTask{Workspace: "loa", Title: "blocked", Deps: []int64{dep}})
			lease(t, s, now) // takes the dep, the only other claimable task
			return task
		},
		"retry delay": func(t *testing.T, s *store.Store) int64 {
			task := add(t, s, store.NewTask{Workspace: "loa", Title: "flaky"})
			lease(t, s, now)
			if err := s.Fail(ctx, task, "boom"); err != nil {
				t.Fatalf("fail: %v", err)
			}
			return task
		},
		"scope contention": func(t *testing.T, s *store.Store) int64 {
			holder := add(t, s, store.NewTask{Workspace: "loa", Title: "holder", Scopes: []string{"repo:a"}})
			task := add(t, s, store.NewTask{Workspace: "loa", Title: "contender", Scopes: []string{"repo:a"}})
			if got := lease(t, s, now); got == nil || got.ID != holder {
				t.Fatalf("holder was not leased: %v", got)
			}
			return task
		},
		"blocked": func(t *testing.T, s *store.Store) int64 {
			task := add(t, s, store.NewTask{Workspace: "loa", Title: "dead", MaxTries: 1})
			lease(t, s, now)
			if err := s.Fail(ctx, task, "out of attempts"); err != nil {
				t.Fatalf("fail: %v", err)
			}
			return task
		},
		"done": func(t *testing.T, s *store.Store) int64 {
			task := add(t, s, store.NewTask{Workspace: "loa", Title: "finished"})
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

	dep := add(t, s, store.NewTask{Workspace: "loa", Title: "schema"})
	child := add(t, s, store.NewTask{Workspace: "loa", Title: "writer", Scopes: []string{"repo:a"}, Deps: []int64{dep}})
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

func TestBlockersExactMessages(t *testing.T) {
	ctx := context.Background()

	t.Run("leased", func(t *testing.T) {
		s := open(t)
		id := add(t, s, store.NewTask{Title: "running", Workspace: "loa"})
		task := lease(t, s, time.Now())
		want := []string{"leased by test until " + task.LeaseExpiry.Format(time.RFC3339)}
		if got := blockersAt(t, s, id, time.Now()); !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})

	t.Run("done", func(t *testing.T) {
		s := open(t)
		id := add(t, s, store.NewTask{Title: "finished", Workspace: "loa"})
		lease(t, s, time.Now())
		if err := s.Complete(ctx, id, "b/done", "ok"); err != nil {
			t.Fatalf("complete: %v", err)
		}
		if got, want := blockersAt(t, s, id, time.Now()), []string{"already done"}; !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		s := open(t)
		id := add(t, s, store.NewTask{Title: "dead", Workspace: "loa", MaxTries: 1})
		lease(t, s, time.Now())
		if err := s.Fail(ctx, id, "out of attempts"); err != nil {
			t.Fatalf("fail: %v", err)
		}
		want := []string{"blocked, needs a human: out of attempts"}
		if got := blockersAt(t, s, id, time.Now()); !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})

	t.Run("retry", func(t *testing.T) {
		s := open(t)
		id := add(t, s, store.NewTask{Title: "flaky", Workspace: "loa"})
		lease(t, s, time.Now())
		if err := s.Fail(ctx, id, "boom"); err != nil {
			t.Fatalf("fail: %v", err)
		}
		task, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		want := []string{"retry delayed until " + task.NotBefore.Format(time.RFC3339)}
		if got := blockersAt(t, s, id, time.Now()); !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})

	t.Run("dependency", func(t *testing.T) {
		s := open(t)
		dep := add(t, s, store.NewTask{Title: "schema", Workspace: "loa"})
		id := add(t, s, store.NewTask{Title: "writer", Workspace: "loa", Deps: []int64{dep}})
		want := []string{fmt.Sprintf("waiting on #%d (new) %q", dep, "schema")}
		if got := blockersAt(t, s, id, time.Now()); !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})

	t.Run("scope contender", func(t *testing.T) {
		s := open(t)
		holder := add(t, s, store.NewTask{Title: "holder", Workspace: "loa", Priority: 10, Scopes: []string{"repo:a"}})
		id := add(t, s, store.NewTask{Title: "contender", Workspace: "loa", Priority: 5, Scopes: []string{"repo:a"}})
		if got := lease(t, s, time.Now()); got == nil || got.ID != holder {
			t.Fatalf("lease = %v, want holder %d", got, holder)
		}
		want := []string{fmt.Sprintf("scope %q is held by #%d (%s)", "repo:a", holder, "test")}
		if got := blockersAt(t, s, id, time.Now()); !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})

	t.Run("multiple reasons", func(t *testing.T) {
		s := open(t)
		dep := add(t, s, store.NewTask{Title: "schema", Workspace: "loa", Priority: 1})
		id := add(t, s, store.NewTask{
			Title: "target", Workspace: "loa", Priority: 10,
			Scopes: []string{"repo:a"}, Deps: []int64{dep},
		})

		// Finish the dependency so the target can run once; then reopen it.
		if got := lease(t, s, time.Now()); got == nil || got.ID != dep {
			t.Fatalf("dep lease = %v, want %d", got, dep)
		}
		if err := s.Complete(ctx, dep, "b/schema", "done"); err != nil {
			t.Fatalf("complete dep: %v", err)
		}
		if got := lease(t, s, time.Now()); got == nil || got.ID != id {
			t.Fatalf("target lease = %v, want %d", got, id)
		}
		if err := s.Fail(ctx, id, "boom"); err != nil {
			t.Fatalf("fail target: %v", err)
		}
		if err := s.Resolve(ctx, dep, false, "reopened"); err != nil {
			t.Fatalf("reopen dep: %v", err)
		}

		contender := add(t, s, store.NewTask{Title: "contender", Workspace: "loa", Priority: 5, Scopes: []string{"repo:a"}})
		if got := lease(t, s, time.Now().Add(store.RetryDelay+time.Minute)); got == nil || got.ID != contender {
			t.Fatalf("contender lease = %v, want %d", got, contender)
		}

		task, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		want := []string{
			"retry delayed until " + task.NotBefore.Format(time.RFC3339),
			fmt.Sprintf("waiting on #%d (blocked) %q", dep, "schema"),
			fmt.Sprintf("scope %q is held by #%d (%s)", "repo:a", contender, "test"),
		}
		if got := blockersAt(t, s, id, time.Now()); !slices.Equal(got, want) {
			t.Errorf("blockers = %v, want %v", got, want)
		}
	})
}

func TestAttachRecordsWhatTheAttemptOwns(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()

	id := add(t, s, store.NewTask{Title: "work", Workspace: "loa"})
	if err := s.Attach(ctx, id, "w1", "w1:p1"); !errors.Is(err, store.ErrNotLeased) {
		t.Fatalf("attach before leasing: %v, want ErrNotLeased", err)
	}

	lease(t, s, now)
	if err := s.Attach(ctx, id, "w2", "w2:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.WorkspaceID != "w2" || task.PaneID != "w2:p1" {
		t.Fatalf("ids = %q/%q", task.WorkspaceID, task.PaneID)
	}

	// The ids are history, not lease state: finishing keeps them.
	if err := s.Complete(ctx, id, "b/x", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.Attach(ctx, id, "w3", "w3:p1"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("attach after finishing: %v, want ErrNotLeased", err)
	}

	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.WorkspaceID != "w2" {
		t.Errorf("workspace id = %q after finishing, want w2", task.WorkspaceID)
	}
}

func TestClaimsListWhatTheStoreCanProve(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()

	claimed := add(t, s, store.NewTask{Title: "claimed", Workspace: "loa"})
	unclaimed := add(t, s, store.NewTask{Title: "never dispatched", Workspace: "loa"})
	if got := lease(t, s, now); got == nil || got.ID != claimed {
		t.Fatalf("leased %v, want task %d", got, claimed)
	}
	if err := s.Attach(ctx, claimed, "w7", "w7:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	claims, err := s.Claims(ctx)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	if len(claims) != 1 || claims[0].TaskID != claimed || claims[0].WorkspaceID != "w7" {
		t.Fatalf("claims = %+v", claims)
	}
	if _, err := s.Get(ctx, unclaimed); err != nil {
		t.Fatalf("get: %v", err)
	}
}

func TestClaimsEmptyAndMultiple(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	claims, err := s.Claims(ctx)
	if err != nil {
		t.Fatalf("claims of an empty queue: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("empty queue claimed %+v", claims)
	}

	first := add(t, s, store.NewTask{Title: "first", Workspace: "loa", Priority: 20})
	second := add(t, s, store.NewTask{Title: "second", Workspace: "loa", Priority: 10})
	now := time.Now()
	if got := lease(t, s, now); got == nil || got.ID != first {
		t.Fatalf("first lease = %v, want %d", got, first)
	}
	if err := s.Attach(ctx, first, "w1", "w1:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := lease(t, s, now); got == nil || got.ID != second {
		t.Fatalf("second lease = %v, want %d", got, second)
	}
	if err := s.Attach(ctx, second, "w2", "w2:p1"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// A leased task with no recorded workspace is not a claim.
	unclaimed := add(t, s, store.NewTask{Title: "unclaimed", Workspace: "loa", Priority: 1})
	if got := lease(t, s, now); got == nil || got.ID != unclaimed {
		t.Fatalf("third lease = %v, want %d", got, unclaimed)
	}

	claims, err = s.Claims(ctx)
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	got := make(map[int64]string, len(claims))
	for _, c := range claims {
		got[c.TaskID] = c.WorkspaceID
	}
	want := map[int64]string{first: "w1", second: "w2"}
	if len(got) != len(want) {
		t.Fatalf("claims = %+v, want %v", claims, want)
	}
	for id, workspaceID := range want {
		if got[id] != workspaceID {
			t.Errorf("claim for task %d = %q, want %q", id, got[id], workspaceID)
		}
	}
}

func TestDepsOrderAndMissingTask(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	first := add(t, s, store.NewTask{Title: "first", Workspace: "loa"})
	second := add(t, s, store.NewTask{Title: "second", Workspace: "loa"})
	child := add(t, s, store.NewTask{
		Title: "child", Workspace: "loa",
		Deps: []int64{second, first},
	})

	deps, err := s.Deps(ctx, child)
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if len(deps) != 2 || deps[0].ID != first || deps[1].ID != second {
		t.Fatalf("deps = %+v, want ids %d then %d", deps, first, second)
	}
	if deps[0].Title != "first" || deps[0].Status != store.New {
		t.Errorf("first dep = %+v", deps[0])
	}

	missing, err := s.Deps(ctx, 999)
	if err != nil {
		t.Fatalf("deps of a missing task: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing task has deps %+v", missing)
	}
}

func TestTaskCarriesDependenciesInGetAndList(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	first := add(t, s, store.NewTask{Title: "first", Workspace: "loa"})
	second := add(t, s, store.NewTask{Title: "second", Workspace: "loa"})
	child := add(t, s, store.NewTask{Title: "child", Workspace: "loa", Deps: []int64{second, first}})

	// A done edge still belongs to the task; it is not removed from the graph.
	if got := lease(t, s, time.Now()); got == nil || got.ID != first {
		t.Fatalf("lease = %v, want first dependency %d", got, first)
	}
	if err := s.Complete(ctx, first, "b/first", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	want := []store.Dep{
		{ID: first, Title: "first", Status: store.Done},
		{ID: second, Title: "second", Status: store.New},
	}
	task, err := s.Get(ctx, child)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !slices.Equal(task.Deps, want) {
		t.Errorf("get deps = %+v, want %+v", task.Deps, want)
	}

	tasks, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, listed := range tasks {
		if listed.ID == child {
			if !slices.Equal(listed.Deps, want) {
				t.Errorf("list deps = %+v, want %+v", listed.Deps, want)
			}
			return
		}
	}
	t.Fatalf("child %d missing from list %+v", child, tasks)
}

func TestDependentsListsTasksInIDOrder(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	dep := add(t, s, store.NewTask{Title: "dep", Workspace: "loa"})
	writer := add(t, s, store.NewTask{Title: "writer", Workspace: "loa", Deps: []int64{dep}})
	reader := add(t, s, store.NewTask{Title: "reader", Workspace: "loa", Deps: []int64{dep}})
	unrelated := add(t, s, store.NewTask{Title: "unrelated", Workspace: "loa"})

	got, err := s.Dependents(ctx, dep)
	if err != nil {
		t.Fatalf("dependents: %v", err)
	}
	want := []store.Dep{
		{ID: writer, Title: "writer", Status: store.New},
		{ID: reader, Title: "reader", Status: store.New},
	}
	if !slices.Equal(got, want) {
		t.Errorf("dependents = %+v, want %+v", got, want)
	}

	if none, err := s.Dependents(ctx, unrelated); err != nil {
		t.Errorf("dependents of a task nothing waits on: %v", err)
	} else if len(none) != 0 {
		t.Errorf("dependents of %d = %+v, want none", unrelated, none)
	}

	if none, err := s.Dependents(ctx, 999); err != nil {
		t.Errorf("dependents of a missing task: %v", err)
	} else if len(none) != 0 {
		t.Errorf("missing task has dependents %+v", none)
	}
}

func TestDependencyBaseIsTheNewestDoneDependency(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	s := openAt(t, path)

	first := add(t, s, store.NewTask{Workspace: "loa", Title: "first"})
	second := add(t, s, store.NewTask{Workspace: "loa", Title: "second"})
	child := add(t, s, store.NewTask{Workspace: "loa", Title: "child", Deps: []int64{first, second}})

	branches := map[int64]string{first: "b/first", second: "b/second"}
	for range branches {
		task := lease(t, s, time.Now())
		if task == nil {
			t.Fatal("nothing to lease")
		}
		if err := s.Complete(ctx, task.ID, branches[task.ID], "done"); err != nil {
			t.Fatalf("complete %d: %v", task.ID, err)
		}
	}

	// The lower id finished last, so picking by id would return the wrong branch.
	backdate(t, path, first, time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC))
	backdate(t, path, second, time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC))

	got, err := s.DependencyBase(ctx, child)
	if err != nil {
		t.Fatalf("dependency base: %v", err)
	}
	if got != "b/first" {
		t.Errorf("dependency base = %q, want b/first (the one that finished last)", got)
	}
}

func TestDependencyBaseIgnoresRequeuedAndBranchlessDependencies(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	done := add(t, s, store.NewTask{Workspace: "loa", Title: "done"})
	lease(t, s, time.Now())
	if err := s.Complete(ctx, done, "b/done", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// A failure returns the task to the queue: it is no longer done.
	requeued := add(t, s, store.NewTask{Workspace: "loa", Title: "requeued"})
	lease(t, s, time.Now())
	if err := s.Fail(ctx, requeued, "compiler blew up"); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// A human can settle a task done without a branch to point at.
	branchless := add(t, s, store.NewTask{Workspace: "loa", Title: "settled by hand"})
	if err := s.Resolve(ctx, branchless, true, "approved by hand"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	for _, tc := range []struct {
		name string
		deps []int64
		want string
	}{
		{"requeued ignored beside a done dependency", []int64{done, requeued}, "b/done"},
		{"only a requeued dependency", []int64{requeued}, ""},
		{"a done dependency without a branch", []int64{branchless}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := add(t, s, store.NewTask{Workspace: "loa", Title: tc.name, Deps: tc.deps})
			got, err := s.DependencyBase(ctx, child)
			if err != nil {
				t.Fatalf("dependency base: %v", err)
			}
			if got != tc.want {
				t.Errorf("dependency base = %q, want %q", got, tc.want)
			}
		})
	}

	got, err := s.DependencyBase(ctx, 999)
	if err != nil {
		t.Fatalf("dependency base of a missing task: %v", err)
	}
	if got != "" {
		t.Errorf("a missing task has dependency base %q", got)
	}
}

func TestCompleteSetsRunTime(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	id := add(t, s, store.NewTask{Title: "slow", Workspace: "loa"})
	// The attempt started two seconds ago; the lease is still valid because its
	// expiry is an hour ahead. Stored timestamps have one-second resolution, so
	// a shorter attempt would measure 0.
	leaseFor(t, s, "test", time.Hour, time.Now().Add(-2*time.Second))
	if err := s.Complete(ctx, id, "b/x", "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.RunMS < 1900 || task.RunMS > 4000 {
		t.Errorf("run time = %dms, want about two seconds", task.RunMS)
	}

	// A human resolving a task settles it without pretending to have timed it.
	if err := s.Resolve(ctx, id, true, "approved by hand"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.RunMS < 1900 || task.RunMS > 4000 {
		t.Errorf("resolving changed the run time to %dms", task.RunMS)
	}
}

func TestApplyCreatesTheBatchInOneTransaction(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	existing := add(t, s, store.NewTask{Title: "existing", Workspace: "loa", Priority: 1})

	ids, err := s.Apply(ctx, []store.BatchItem{
		{NewTask: store.NewTask{Title: "first", Workspace: "loa", PlanID: "plan-a", Scopes: []string{"repo:a"}}},
		{NewTask: store.NewTask{Title: "second", Workspace: "loa", PlanID: "plan-a"},
			After: []int{0}, Deps: []int64{existing}},
	}, []store.Priority{{ID: existing, Priority: 9}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("ids = %v", ids)
	}

	second, err := s.Get(ctx, ids[1])
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	deps, err := s.Deps(ctx, ids[1])
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if second.Title != "second" || second.PlanID != "plan-a" || len(deps) != 2 {
		t.Fatalf("second = %+v deps = %+v", second, deps)
	}
	first, err := s.Get(ctx, existing)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if first.Priority != 9 {
		t.Errorf("priority = %d, want 9", first.Priority)
	}
}

func TestApplyRollsBackWhenAnyPartFails(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	before, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	_, err = s.Apply(ctx,
		[]store.BatchItem{{NewTask: store.NewTask{Title: "would be created", Workspace: "loa"}}},
		[]store.Priority{{ID: 404, Priority: 3}},
	)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("apply: %v, want a refusal", err)
	}

	after, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a failed apply created %d task(s)", len(after)-len(before))
	}
}

func TestRoleDefaultsToImplementAndRoundTrips(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	old := add(t, s, store.NewTask{Title: "before roles", Workspace: "loa"})
	task, err := s.Get(ctx, old)
	if err != nil {
		t.Fatalf("get default role: %v", err)
	}
	if task.Role != "implement" {
		t.Errorf("default role = %q, want implement", task.Role)
	}

	id := add(t, s, store.NewTask{Title: "plan it", Workspace: "loa", Role: "planner"})
	task, err = s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get explicit role: %v", err)
	}
	if task.Role != "planner" {
		t.Errorf("role = %q, want planner", task.Role)
	}
	if task.Prompt != "" || task.PromptHash != "" {
		t.Errorf("prompt record = (%q, %q), want empty before dispatch", task.Prompt, task.PromptHash)
	}
}

func TestRecordPromptNeedsTheLeaseAndIsKept(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := add(t, s, store.NewTask{Title: "implement it", Workspace: "loa"})

	if err := s.RecordPrompt(ctx, id, "implementer.md", "abc"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("record on an unleased task: %v, want ErrNotLeased", err)
	}

	lease(t, s, time.Now())
	if err := s.RecordPrompt(ctx, id, "implementer.md", "abc"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Complete(ctx, id, "loa/1.1", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.Prompt != "implementer.md" || task.PromptHash != "abc" {
		t.Errorf("prompt record = (%q, %q), want the dispatch's file and hash", task.Prompt, task.PromptHash)
	}
}

func TestRecordCheckNeedsTheLeaseAndIsKept(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := add(t, s, store.NewTask{Title: "review it", Workspace: "loa", Role: "review"})

	if err := s.RecordCheck(ctx, id, "go test ./...", true, "ok"); !errors.Is(err, store.ErrNotLeased) {
		t.Errorf("record on an unleased review: %v, want ErrNotLeased", err)
	}

	lease(t, s, time.Now())
	if err := s.RecordCheck(ctx, id, "go test ./...", false, "FAIL"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.CompleteReview(ctx, id, "changes", "[]", "check failed"); err != nil {
		t.Fatalf("complete review: %v", err)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if task.CheckCommand != "go test ./..." || task.CheckPassed || task.CheckOutput != "FAIL" {
		t.Errorf("check = (%q, %v, %q), want the recorded failure",
			task.CheckCommand, task.CheckPassed, task.CheckOutput)
	}
}

func TestMarkMergedRecordsOnceAndRefusesASecond(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	id := add(t, s, store.NewTask{Title: "implement it", Workspace: "loa"})
	lease(t, s, time.Now())
	if err := s.Complete(ctx, id, "loa/1.1", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkMerged(ctx, id, at, "abc123"); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	if err := s.MarkMerged(ctx, id, at.Add(time.Minute), "def456"); !errors.Is(err, store.ErrAlreadyMerged) {
		t.Errorf("second mark: %v, want ErrAlreadyMerged", err)
	}
	if err := s.MarkMerged(ctx, 999, at, "def456"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("mark missing: %v, want ErrNotFound", err)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !task.MergedAt.Equal(at) || task.MergeCommit != "abc123" {
		t.Errorf("merge record = (%s, %q), want the first merge kept", task.MergedAt, task.MergeCommit)
	}
}
