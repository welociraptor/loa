package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func startRun(t *testing.T, s *Store, owner string) Run {
	t.Helper()
	r, err := s.StartRun(context.Background(), owner, 2, 3)
	if err != nil {
		t.Fatalf("start run as %q: %v", owner, err)
	}
	return r
}

// TestStartRunRefusesWhileAnotherIsActive proves the single-active-run rule
// holds across handles: two Stores on one file are what two processes look
// like, and the second insert must lose to the schema rather than to call
// ordering.
func TestStartRunRefusesWhileAnotherIsActive(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	first := openAt(t, path)
	second := openAt(t, path)

	run := startRun(t, first, "owner-a")
	if !run.Active() {
		t.Errorf("run %+v is not active after starting", run)
	}

	if _, err := second.StartRun(ctx, "owner-b", 1, 1); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second start: %v, want ErrRunActive", err)
	}

	active, err := second.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active == nil || active.ID != run.ID || active.Owner != "owner-a" {
		t.Fatalf("active run = %+v, want the run owner-a started", active)
	}
}

// TestSchemaRefusesASecondActiveRun writes around StartRun to prove the
// invariant is the unique index, not Go code.
func TestSchemaRefusesASecondActiveRun(t *testing.T) {
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "tasks.db"))
	startRun(t, s, "owner")

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (owner, started_at) VALUES ('intruder', ?)`,
		formatTime(time.Now()))
	if err == nil {
		t.Fatal("the database accepted two active runs")
	}

	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE ended_at = ''`).
		Scan(&active); err != nil {
		t.Fatalf("count active runs: %v", err)
	}
	if active != 1 {
		t.Errorf("active runs = %d, want 1", active)
	}
}

func TestFinishRunFreesTheSlot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	first := openAt(t, path)
	second := openAt(t, path)

	run := startRun(t, first, "owner-a")
	if err := second.FinishRun(ctx, run.ID, 4, 1); err != nil {
		t.Fatalf("finish: %v", err)
	}

	next := startRun(t, second, "owner-b")
	if next.ID == run.ID {
		t.Errorf("new run reuses id %d", run.ID)
	}

	active, err := first.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active == nil || active.ID != next.ID {
		t.Fatalf("active run = %+v, want %+v", active, next)
	}
}

func TestFinishedRunIsNotActive(t *testing.T) {
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "tasks.db"))
	run := startRun(t, s, "owner")

	if err := s.FinishRun(ctx, run.ID, 2, 3); err != nil {
		t.Fatalf("finish: %v", err)
	}

	active, err := s.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active != nil {
		t.Errorf("active run = %+v, want none", active)
	}

	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want the finished run", runs)
	}
	if runs[0].Active() {
		t.Errorf("run %+v is still active after finishing", runs[0])
	}
	if runs[0].Done != 2 || runs[0].Failed != 3 {
		t.Errorf("counts = (%d, %d), want (2, 3)", runs[0].Done, runs[0].Failed)
	}
}

func TestFinishRunTwiceErrors(t *testing.T) {
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "tasks.db"))
	run := startRun(t, s, "owner")

	if err := s.FinishRun(ctx, run.ID, 1, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := s.FinishRun(ctx, run.ID, 9, 9); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("second finish: %v, want ErrRunFinished", err)
	}

	if err := s.FinishRun(ctx, run.ID+999, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("finish unknown run: %v, want ErrNotFound", err)
	}

	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if runs[0].Done != 1 || runs[0].Failed != 0 {
		t.Errorf("counts = (%d, %d), want the first finish's (1, 0)", runs[0].Done, runs[0].Failed)
	}
}

func TestSettleRunFreesTheSlotAndKeepsTheCounts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	first := openAt(t, path)
	second := openAt(t, path)

	run := startRun(t, first, "owner-a")
	// Counts can only be written on the way out of active, so set them behind
	// the API to show that settling carries them rather than zeroing them.
	if _, err := first.db.ExecContext(ctx,
		`UPDATE runs SET done = 5, failed = 2 WHERE id = ?`, run.ID); err != nil {
		t.Fatalf("seed counts: %v", err)
	}

	if err := second.SettleRun(ctx, run.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}

	active, err := first.ActiveRun(ctx)
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active != nil {
		t.Errorf("active run = %+v, want none", active)
	}

	settled, err := first.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if settled.Active() || settled.EndedAt == "" {
		t.Errorf("run = %+v, want it closed", settled)
	}
	if settled.Done != 5 || settled.Failed != 2 {
		t.Errorf("counts = (%d, %d), want the recorded (5, 2)", settled.Done, settled.Failed)
	}

	next := startRun(t, second, "owner-b")
	if next.ID == run.ID {
		t.Errorf("new run reuses id %d", run.ID)
	}
}

func TestSettleRunRefusesAFinishedRun(t *testing.T) {
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "tasks.db"))
	run := startRun(t, s, "owner")
	if err := s.FinishRun(ctx, run.ID, 1, 0); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if err := s.SettleRun(ctx, run.ID); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("settle: %v, want ErrRunFinished", err)
	}
	if err := s.SettleRun(ctx, run.ID+999); !errors.Is(err, ErrNotFound) {
		t.Errorf("settle unknown run: %v, want ErrNotFound", err)
	}
}

func TestRunTimestampsAreRFC3339UTC(t *testing.T) {
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "tasks.db"))

	startBefore := time.Now().UTC().Truncate(time.Second)
	run := startRun(t, s, "owner")
	startAfter := time.Now().UTC().Truncate(time.Second)

	for _, stamp := range []string{run.StartedAt, mustActive(t, s).StartedAt} {
		parsed, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			t.Fatalf("started_at %q is not RFC3339: %v", stamp, err)
		}
		if parsed.Location() != time.UTC {
			t.Errorf("started_at %q is not UTC", stamp)
		}
		if parsed.Before(startBefore) || parsed.After(startAfter) {
			t.Errorf("started_at %q is outside [%s, %s]", stamp, startBefore, startAfter)
		}
	}

	finishBefore := time.Now().UTC().Truncate(time.Second)
	if err := s.FinishRun(ctx, run.ID, 1, 1); err != nil {
		t.Fatalf("finish: %v", err)
	}
	finishAfter := time.Now().UTC().Truncate(time.Second)
	finished, err := s.Runs(ctx, 1)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(finished) != 1 {
		t.Fatalf("runs = %+v, want one", finished)
	}
	ended, err := time.Parse(time.RFC3339, finished[0].EndedAt)
	if err != nil {
		t.Fatalf("ended_at %q is not RFC3339: %v", finished[0].EndedAt, err)
	}
	if ended.Location() != time.UTC {
		t.Errorf("ended_at %q is not UTC", finished[0].EndedAt)
	}
	if ended.Before(finishBefore) || ended.After(finishAfter) {
		t.Errorf("ended_at %q is outside [%s, %s]", finished[0].EndedAt, finishBefore, finishAfter)
	}
}

func mustActive(t *testing.T, s *Store) Run {
	t.Helper()
	active, err := s.ActiveRun(context.Background())
	if err != nil {
		t.Fatalf("active run: %v", err)
	}
	if active == nil {
		t.Fatal("no active run")
	}
	return *active
}

func TestRunsAreNewestFirstAndBounded(t *testing.T) {
	ctx := context.Background()
	s := openAt(t, filepath.Join(t.TempDir(), "tasks.db"))

	first := startRun(t, s, "owner-a")
	if err := s.FinishRun(ctx, first.ID, 1, 0); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	second := startRun(t, s, "owner-b")
	if err := s.FinishRun(ctx, second.ID, 0, 1); err != nil {
		t.Fatalf("finish second: %v", err)
	}

	runs, err := s.Runs(ctx, 1)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != second.ID {
		t.Fatalf("runs = %+v, want only the newest", runs)
	}

	if _, err := s.Runs(ctx, 0); err == nil {
		t.Error("limit 0 accepted")
	}
	if _, err := s.StartRun(ctx, "", 1, 1); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := s.StartRun(ctx, "owner", 0, 1); err == nil {
		t.Error("zero workers accepted")
	}
}

// TestRunMigrationAppliesToV4DatabaseWithData opens a database at the version
// before runs existed, fills it, then lets the current binary migrate it.
func TestRunMigrationAppliesToV4DatabaseWithData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	const created = "2024-01-02T03:04:05Z"

	db := rawDBAt(t, path)
	applyMigrationsUpTo(t, db, 4)
	res, err := db.ExecContext(ctx,
		`INSERT INTO tasks (title, body, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"survivor", "written before runs existed", created, created)
	if err != nil {
		t.Fatalf("insert v4 task: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("task id: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v4 database: %v", err)
	}

	s := openAt(t, path)
	if got := version(t, s.db); got != 7 {
		t.Fatalf("user_version = %d, want 7", got)
	}

	task, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("read task after migration: %v", err)
	}
	if task.Title != "survivor" || task.Body != "written before runs existed" {
		t.Errorf("task = %+v, want the row written at v4", task)
	}

	run := startRun(t, s, "owner")
	if got, err := s.ActiveRun(ctx); err != nil || got == nil || got.ID != run.ID {
		t.Errorf("active run after migrating = %+v, %v", got, err)
	}
}

func TestRunDatabaseNewerThanBinaryIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA user_version = 8"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	_, err = Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("open: %v, want a newer-than-binary refusal", err)
	}
}
