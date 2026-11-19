package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Dep is a task that another task waits on.
type Dep struct {
	ID     int64
	Title  string
	Status Status
}

// Deps returns the tasks id waits for, in id order.
func (s *Store) Deps(ctx context.Context, id int64) ([]Dep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.title, t.status
		 FROM task_deps d
		 JOIN tasks t ON t.id = d.depends_on
		 WHERE d.task_id = ?
		 ORDER BY t.id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: deps of %d: %w", id, err)
	}
	defer rows.Close()

	var deps []Dep
	for rows.Next() {
		var d Dep
		var status string
		if err := rows.Scan(&d.ID, &d.Title, &status); err != nil {
			return nil, fmt.Errorf("store: deps of %d: %w", id, err)
		}
		d.Status = Status(status)
		deps = append(deps, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: deps of %d: %w", id, err)
	}
	return deps, nil
}

// Dependents returns the tasks that wait on id, in id order.
func (s *Store) Dependents(ctx context.Context, id int64) ([]Dep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.title, t.status
		 FROM task_deps d
		 JOIN tasks t ON t.id = d.task_id
		 WHERE d.depends_on = ?
		 ORDER BY t.id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: dependents of %d: %w", id, err)
	}
	defer rows.Close()

	var dependents []Dep
	for rows.Next() {
		var d Dep
		var status string
		if err := rows.Scan(&d.ID, &d.Title, &status); err != nil {
			return nil, fmt.Errorf("store: dependents of %d: %w", id, err)
		}
		d.Status = Status(status)
		dependents = append(dependents, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: dependents of %d: %w", id, err)
	}
	return dependents, nil
}

// DependencyBase returns the branch of the dependency that finished last among
// those that are done and recorded one. It returns "" when no dependency
// qualifies, which is what makes the configured base the fallback. A re-queued
// dependency is not done, so it never contributes.
func (s *Store) DependencyBase(ctx context.Context, id int64) (string, error) {
	var branch string
	err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(dep.branch, '')
		 FROM task_deps d
		 JOIN tasks dep ON dep.id = d.depends_on
		 WHERE d.task_id = ?
		   AND dep.status = 'done'
		   AND dep.branch IS NOT NULL AND dep.branch <> ''
		 ORDER BY dep.updated_at DESC, dep.id DESC
		 LIMIT 1`, id).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: dependency base of %d: %w", id, err)
	}
	return branch, nil
}

// ReviewFor returns the newest review task that depends on id. A merge is
// allowed to name either the review (which has the verdict and check) or the
// implementation task (which has the branch); this is how the latter finds the
// former. It returns nil when no review depends on the task.
func (s *Store) ReviewFor(ctx context.Context, id int64) (*Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+`
		 FROM tasks r
		 JOIN task_deps d ON d.task_id = r.id
		 WHERE d.depends_on = ? AND r.role IN ('review', 'reviewer')
		 ORDER BY r.id DESC
		 LIMIT 1`, id))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := loadScopes(ctx, s.db, t); err != nil {
		return nil, err
	}
	if err := loadDeps(ctx, s.db, []*Task{t}); err != nil {
		return nil, err
	}
	return t, nil
}

// Blockers explains why a task cannot be claimed at now. An empty result means
// it is runnable. The checks mirror the claim predicate in Lease; when one
// changes, the other changes with it.
func (s *Store) Blockers(ctx context.Context, id int64, now time.Time) ([]string, error) {
	task, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	switch task.Status {
	case Leased:
		return []string{fmt.Sprintf("leased by %s until %s", task.LeaseOwner, task.LeaseExpiry.Format(time.RFC3339))}, nil
	case Done:
		return []string{"already done"}, nil
	case Blocked:
		return []string{fmt.Sprintf("blocked, needs a human: %s", task.Result)}, nil
	}

	var reasons []string

	if !task.NotBefore.IsZero() && task.NotBefore.After(now) {
		reasons = append(reasons, fmt.Sprintf("retry delayed until %s", task.NotBefore.Format(time.RFC3339)))
	}

	deps, err := s.Deps(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		if d.Status != Done {
			reasons = append(reasons, fmt.Sprintf("waiting on #%d (%s) %q", d.ID, d.Status, d.Title))
		}
	}

	contenders, err := s.scopeContenders(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, c := range contenders {
		reasons = append(reasons, fmt.Sprintf("scope %q is held by #%d (%s)", c.scope, c.id, c.owner))
	}

	return reasons, nil
}

type contender struct {
	scope string
	id    int64
	owner string
}

func (s *Store) scopeContenders(ctx context.Context, id int64) ([]contender, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT mine.scope, other.id, coalesce(other.lease_owner, '')
		 FROM task_scopes mine
		 JOIN task_scopes theirs ON theirs.scope = mine.scope
		 JOIN tasks other ON other.id = theirs.task_id
		 WHERE mine.task_id = ? AND other.id <> ? AND other.status = 'leased'
		 ORDER BY mine.scope, other.id`, id, id)
	if err != nil {
		return nil, fmt.Errorf("store: scope contenders for %d: %w", id, err)
	}
	defer rows.Close()

	var contenders []contender
	for rows.Next() {
		var c contender
		if err := rows.Scan(&c.scope, &c.id, &c.owner); err != nil {
			return nil, fmt.Errorf("store: scope contenders for %d: %w", id, err)
		}
		contenders = append(contenders, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scope contenders for %d: %w", id, err)
	}
	return contenders, nil
}
