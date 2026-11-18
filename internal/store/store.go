// Package store is the task queue for the runner. The SQLite database is the
// only shared state between the runner, the agents, and the human: agents are
// handed one task and never write to it.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql
)

// RetryDelay is how long a failed or abandoned task stays out of the queue.
const RetryDelay = 30 * time.Minute

// Status is the lifecycle position of a task.
type Status string

// The four lifecycle states a task moves between. Transitions happen only in
// this package.
const (
	New     Status = "new"     // ready to be leased once its deps and scopes allow
	Leased  Status = "leased"  // owned by one worker until the lease expires
	Done    Status = "done"    // finished, dependent tasks may now run
	Blocked Status = "blocked" // needs a human: failed out, or reported a blocker
)

// Errors a caller has to distinguish: a missing task, and a write refused
// because the caller no longer holds the lease.
var (
	ErrNotFound  = errors.New("store: task not found")
	ErrNotLeased = errors.New("store: task is not leased")
)

// Task is one row of the queue. Scopes are coarse mutual-exclusion domains:
// two leased tasks never share one.
type Task struct {
	ID          int64
	Title       string
	Body        string
	Workspace   string // Herdr workspace label; resolved to an id at dispatch
	Status      Status
	Priority    int
	Attempts    int
	MaxAttempts int
	Branch      string
	Result      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LeaseOwner  string
	LeaseExpiry time.Time
	NotBefore   time.Time // claimable again after a failure; zero means now
	Scopes      []string

	// What the current attempt produced. Empty before the first dispatch.
	WorkspaceID string
	PaneID      string

	// RunMS is how long the last attempt held the task, in milliseconds.
	RunMS int64
}

// NewTask is the caller-supplied part of a task.
type NewTask struct {
	Title     string
	Body      string
	Workspace string
	Priority  int
	MaxTries  int
	Scopes    []string
	Deps      []int64
}

// Store is the queue. It owns one SQLite connection so that writers in this
// process serialise, and relies on busy_timeout for other processes.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the task database at path and applies the
// schema. Transactions start as write transactions so that contended writers
// block on busy_timeout instead of failing with SQLITE_BUSY_SNAPSHOT.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}

	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()

	// The database holds task bodies and agent output; SQLite's own default
	// follows the umask, which can leave them readable by everyone.
	if err := createPrivate(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// One connection serialises writers in-process; busy_timeout covers other
	// processes sharing the file, such as a CLI invocation next to the runner.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := migrate(ctx, db, migrationsFS); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// createPrivate creates the database file with owner-only permissions. An
// existing file is left exactly as it is: narrowing a mode someone chose
// deliberately is not this function's business.
func createPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the caller chose this path
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	return f.Close()
}

// Close releases the database handle. The database is a file; nothing is lost
// if the process exits without calling it.
func (s *Store) Close() error { return s.db.Close() }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// Add inserts a task and returns its id. Dependencies must already exist.
func (s *Store) Add(ctx context.Context, t NewTask) (int64, error) {
	if strings.TrimSpace(t.Title) == "" {
		return 0, errors.New("store: task title is required")
	}
	if strings.TrimSpace(t.Workspace) == "" {
		return 0, errors.New("store: task workspace is required")
	}

	now := formatTime(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: add: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (title, body, workspace, priority, max_attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Title, t.Body, t.Workspace, t.Priority, maxTries(t.MaxTries), now, now)
	if err != nil {
		return 0, fmt.Errorf("store: add: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: add: %w", err)
	}

	for _, scope := range t.Scopes {
		if strings.TrimSpace(scope) == "" {
			return 0, errors.New("store: empty scope")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_scopes (task_id, scope) VALUES (?, ?)`, id, scope); err != nil {
			return 0, fmt.Errorf("store: add scope %q: %w", scope, err)
		}
	}

	for _, dep := range t.Deps {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_deps (task_id, depends_on) VALUES (?, ?)`, id, dep); err != nil {
			return 0, fmt.Errorf("store: add dep %d: %w", dep, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: add: %w", err)
	}
	return id, nil
}

func maxTries(n int) int {
	if n <= 0 {
		return 2
	}
	return n
}

const taskColumns = `id, title, body, workspace, status, priority, attempts, max_attempts,
	coalesce(branch, ''), coalesce(result, ''),
	created_at, updated_at,
	coalesce(lease_owner, ''), coalesce(lease_expires_at, ''),
	coalesce(not_before, ''), workspace_id, pane_id, run_ms`

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(sc scanner) (*Task, error) {
	t := Task{}
	var status, created, updated, expiry, notBefore string
	err := sc.Scan(&t.ID, &t.Title, &t.Body, &t.Workspace, &status, &t.Priority, &t.Attempts, &t.MaxAttempts,
		&t.Branch, &t.Result, &created, &updated, &t.LeaseOwner, &expiry, &notBefore,
		&t.WorkspaceID, &t.PaneID, &t.RunMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan task: %w", err)
	}
	t.Status = Status(status)
	if t.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("store: created_at: %w", err)
	}
	if t.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("store: updated_at: %w", err)
	}
	if t.LeaseExpiry, err = parseTime(expiry); err != nil {
		return nil, fmt.Errorf("store: lease_expires_at: %w", err)
	}
	if t.NotBefore, err = parseTime(notBefore); err != nil {
		return nil, fmt.Errorf("store: not_before: %w", err)
	}
	return &t, nil
}

func loadScopes(ctx context.Context, q querier, t *Task) error {
	rows, err := q.QueryContext(ctx, `SELECT scope FROM task_scopes WHERE task_id = ? ORDER BY scope`, t.ID)
	if err != nil {
		return fmt.Errorf("store: scopes of %d: %w", t.ID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return fmt.Errorf("store: scopes of %d: %w", t.ID, err)
		}
		t.Scopes = append(t.Scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: scopes of %d: %w", t.ID, err)
	}
	return nil
}

// Get returns a single task.
func (s *Store) Get(ctx context.Context, id int64) (*Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := loadScopes(ctx, s.db, t); err != nil {
		return nil, err
	}
	return t, nil
}

// List returns tasks in queue order, optionally filtered by status.
func (s *Store) List(ctx context.Context, statuses ...Status) ([]Task, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks`
	args := make([]any, 0, len(statuses))
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		// Only placeholders are concatenated; the statuses are bound as args.
		query += ` WHERE status IN (` + strings.Join(placeholders, ", ") + `)` //nolint:gosec // G202: no values in the SQL text
	}
	query += ` ORDER BY priority DESC, id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}

	tasks := make([]Task, 0, 16)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list: %w", err)
		}
		tasks = append(tasks, *t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}

	for i := range tasks {
		if err := loadScopes(ctx, s.db, &tasks[i]); err != nil {
			return nil, err
		}
	}
	return tasks, nil
}

// Lease claims the highest-priority task that is runnable and not contending
// for a scope with another leased task. It returns nil when nothing is
// runnable. A caller that dies leaves a lease that expires; the next Lease
// reclaims any lapsed lease, retrying the task until max_attempts is spent.
func (s *Store) Lease(ctx context.Context, owner string, ttl time.Duration, now time.Time) (*Task, error) {
	if owner == "" {
		return nil, errors.New("store: lease owner is required")
	}
	if ttl <= 0 {
		return nil, errors.New("store: lease ttl must be positive")
	}

	nowS := formatTime(now)
	expiryS := formatTime(now.Add(ttl))
	retryS := formatTime(now.Add(RetryDelay))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: lease: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks
		 SET status = CASE WHEN attempts >= max_attempts THEN 'blocked' ELSE 'new' END,
		     lease_owner = NULL,
		     lease_expires_at = NULL,
		     not_before = CASE WHEN attempts >= max_attempts THEN NULL ELSE ? END,
		     updated_at = ?
		 WHERE status = 'leased' AND lease_expires_at <= ?`, retryS, nowS, nowS); err != nil {
		return nil, fmt.Errorf("store: lease: reap: %w", err)
	}

	var id int64
	err = tx.QueryRowContext(ctx,
		`UPDATE tasks
		 SET status = 'leased',
		     lease_owner = ?,
		     lease_expires_at = ?,
		     attempts = attempts + 1,
		     updated_at = ?
		 WHERE id = (
		     SELECT t.id FROM tasks t
		     WHERE t.status = 'new'
		       AND (t.not_before IS NULL OR t.not_before <= ?)
		       AND NOT EXISTS (
		           SELECT 1 FROM task_deps d
		           JOIN tasks dep ON dep.id = d.depends_on
		           WHERE d.task_id = t.id AND dep.status <> 'done')
		       AND NOT EXISTS (
		           SELECT 1 FROM task_scopes mine
		           JOIN task_scopes busy ON busy.scope = mine.scope
		           JOIN tasks other ON other.id = busy.task_id
		           WHERE mine.task_id = t.id AND other.status = 'leased')
		     ORDER BY t.priority DESC, t.id
		     LIMIT 1)
		 RETURNING id`, owner, expiryS, nowS, nowS).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: lease: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: lease: %w", err)
	}

	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("store: lease: %w", err)
	}
	if err := loadScopes(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: lease: %w", err)
	}
	return task, nil
}

// Complete marks a leased task done.
func (s *Store) Complete(ctx context.Context, id int64, branch, result string) error {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		 SET status = 'done', branch = ?, result = ?, lease_owner = NULL,
		     lease_expires_at = NULL, run_ms = `+runTime+`, updated_at = ?
		 WHERE id = ? AND status = 'leased'`,
		branch, result, formatTime(now), formatTime(now), id)
	return finished(res, err, id)
}

// Fail returns a leased task to the queue, or blocks it once it has spent its
// attempts.
func (s *Store) Fail(ctx context.Context, id int64, result string) error {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		 SET status = CASE WHEN attempts >= max_attempts THEN 'blocked' ELSE 'new' END,
		     result = ?, lease_owner = NULL, lease_expires_at = NULL,
		     not_before = CASE WHEN attempts >= max_attempts THEN NULL ELSE ? END,
		     run_ms = `+runTime+`, updated_at = ?
		 WHERE id = ? AND status = 'leased'`,
		result, formatTime(now.Add(RetryDelay)), formatTime(now), formatTime(now), id)
	return finished(res, err, id)
}

// Attach records the workspace and pane an attempt owns. It is refused once the
// task is no longer leased: the ids belong to whoever holds the task now.
func (s *Store) Attach(ctx context.Context, id int64, workspaceID, paneID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET workspace_id = ?, pane_id = ?, updated_at = ?
		 WHERE id = ? AND status = 'leased'`,
		workspaceID, paneID, formatTime(time.Now()), id)
	return finished(res, err, id)
}

// runTime measures how long the attempt held the task. SQLite evaluates the
// right-hand side against the row's pre-update values, so updated_at here is
// still the moment the attempt started. Resolution is one second, because that
// is what the stored timestamps carry — plenty for "what did a day cost".
// The extra parameter is the same timestamp written to updated_at.
const runTime = `CAST((julianday(?) - julianday(updated_at)) * 86400000 AS INTEGER)`

// Unblock puts a blocked task back in the queue, keeping its attempt count. A
// human decided it deserves another go, so it gets exactly one: the next lease
// increments the count, and a second failure blocks it again.
func (s *Store) Unblock(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		 SET status = 'new', not_before = NULL, lease_owner = NULL,
		     lease_expires_at = NULL, updated_at = ?
		 WHERE id = ? AND status = 'blocked'`,
		formatTime(time.Now()), id)
	return finished(res, err, id)
}

// Resolve is the human's answer to a task the runner cannot settle: an approved
// dialog that finished, or a decision that it did not work. Unlike Complete and
// Fail it does not require the lease, because the human may be answering hours
// after the run let go.
func (s *Store) Resolve(ctx context.Context, id int64, done bool, result string) error {
	status := Blocked
	if done {
		status = Done
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		 SET status = ?, result = ?, lease_owner = NULL, lease_expires_at = NULL,
		     not_before = NULL, updated_at = ?
		 WHERE id = ? AND status <> 'leased'`,
		string(status), result, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: resolve %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: resolve %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotLeased
	}
	return nil
}

// Claim is the workspace one task's current attempt owns.
type Claim struct {
	TaskID      int64
	WorkspaceID string
}

// Claims returns every recorded workspace id, which is what reconciliation
// treats as proof that a workspace was created by the runner.
func (s *Store) Claims(ctx context.Context) ([]Claim, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id FROM tasks WHERE workspace_id <> ''`)
	if err != nil {
		return nil, fmt.Errorf("store: claims: %w", err)
	}
	defer rows.Close()

	var claims []Claim
	for rows.Next() {
		var c Claim
		if err := rows.Scan(&c.TaskID, &c.WorkspaceID); err != nil {
			return nil, fmt.Errorf("store: claims: %w", err)
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claims: %w", err)
	}
	return claims, nil
}

func finished(res sql.Result, err error, id int64) error {
	if err != nil {
		return fmt.Errorf("store: finish %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish %d: %w", id, err)
	}
	if n == 0 {
		return ErrNotLeased
	}
	return nil
}
