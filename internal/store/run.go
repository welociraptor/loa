package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors a caller has to distinguish: a run may be started only when no other
// run is active, and a finished run cannot be finished again.
var (
	ErrRunActive   = errors.New("store: a run is already active")
	ErrRunFinished = errors.New("store: run is already finished")
)

// Run is one coordinator session. EndedAt is empty while the run is active.
// The counts are what that run settled, fixed when it finished.
type Run struct {
	ID        int64
	Owner     string
	StartedAt string
	EndedAt   string
	Workers   int
	MaxTasks  int
	Done      int
	Failed    int
}

// Active reports whether the run has not finished yet.
func (r Run) Active() bool { return r.EndedAt == "" }

const runColumns = `id, owner, started_at, ended_at, workers, max_tasks, done, failed`

func scanRun(sc scanner) (*Run, error) {
	r := Run{}
	err := sc.Scan(&r.ID, &r.Owner, &r.StartedAt, &r.EndedAt,
		&r.Workers, &r.MaxTasks, &r.Done, &r.Failed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan run: %w", err)
	}
	return &r, nil
}

// StartRun begins a run, refusing while another one is active. The refusal is
// the schema's unique index, not a check-then-insert in Go, so it holds across
// processes sharing the database.
func (s *Store) StartRun(ctx context.Context, owner string, workers, maxTasks int) (Run, error) {
	if strings.TrimSpace(owner) == "" {
		return Run{}, errors.New("store: run owner is required")
	}
	if workers <= 0 {
		return Run{}, errors.New("store: run workers must be positive")
	}
	if maxTasks <= 0 {
		return Run{}, errors.New("store: run max tasks must be positive")
	}

	now := formatTime(time.Now())
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (owner, started_at, workers, max_tasks)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT DO NOTHING`,
		owner, now, workers, maxTasks)
	if err != nil {
		return Run{}, fmt.Errorf("store: start run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, fmt.Errorf("store: start run: %w", err)
	}
	if n == 0 {
		return Run{}, ErrRunActive
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Run{}, fmt.Errorf("store: start run: %w", err)
	}

	return Run{
		ID:        id,
		Owner:     owner,
		StartedAt: now,
		Workers:   workers,
		MaxTasks:  maxTasks,
	}, nil
}

// FinishRun ends an active run and records what it settled. The counts are
// written only on the transition out of active, so a repeated call cannot
// overwrite them.
func (s *Store) FinishRun(ctx context.Context, id int64, done, failed int) error {
	ended := formatTime(time.Now())
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET ended_at = ?, done = ?, failed = ?
		 WHERE id = ? AND ended_at = ''`,
		ended, done, failed, id)
	if err != nil {
		return fmt.Errorf("store: finish run %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: finish run %d: %w", id, err)
	}
	if n == 0 {
		return s.explainUnfinished(ctx, id)
	}
	return nil
}

// SettleRun closes an active run whose worker is gone, so a dead run cannot
// keep the slot forever. It records only the moment the run stopped: the
// counts stay as that run left them, because making them up would misreport
// what it settled. FinishRun remains the only writer of ended_at.
func (s *Store) SettleRun(ctx context.Context, id int64) error {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if !run.Active() {
		return fmt.Errorf("store: settle run %d: %w", id, ErrRunFinished)
	}
	return s.FinishRun(ctx, id, run.Done, run.Failed)
}

// GetRun returns one run by id.
func (s *Store) GetRun(ctx context.Context, id int64) (*Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// explainUnfinished tells a caller whether the run never existed or had
// already ended. It runs only on the error path.
func (s *Store) explainUnfinished(ctx context.Context, id int64) error {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: finish run %d: %w", id, err)
	}
	return fmt.Errorf("store: finish run %d: %w", id, ErrRunFinished)
}

// ActiveRun returns the run in progress, or nil when none is.
func (s *Store) ActiveRun(ctx context.Context) (*Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE ended_at = ''`))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Runs returns the newest runs first.
func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 {
		return nil, errors.New("store: runs limit must be positive")
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: runs: %w", err)
	}
	defer rows.Close()

	runs := make([]Run, 0, limit)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: runs: %w", err)
	}
	return runs, nil
}
