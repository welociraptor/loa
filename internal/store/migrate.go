package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Migrations are embedded in order and applied by version. A file is never
// edited once shipped, because databases in the field have already run it.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// migrate brings the database up to the newest embedded version, one
// transaction per step, so a failure leaves the previous version intact.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	migrations, err := loadMigrations(fsys)
	if err != nil {
		return err
	}

	latest := migrations[len(migrations)-1].version

	// This read is a fast path for a database that is already current: it
	// avoids opening a write transaction when there is nothing to do. It is not
	// authoritative, because another Open may commit after it; each step below
	// re-reads the version under the write lock.
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if current > latest {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d)", current, latest)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if current, err = applyMigration(ctx, db, m, latest); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration applies one step in an IMMEDIATE transaction and returns the
// version the transaction saw. The DSN sets _txlock=immediate, so the write
// lock is held before user_version is read: a concurrent Open that committed
// while this one waited has already applied the step, and re-running it would
// fail its first CREATE TABLE.
func applyMigration(ctx context.Context, db *sql.DB, m migration, latest int) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: migration %d: %w", m.version, err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return 0, fmt.Errorf("store: migration %d: read schema version: %w", m.version, err)
	}
	// A newer binary may have migrated the file between the fast-path read and
	// this transaction. Refuse from inside the lock rather than write past it.
	if current > latest {
		return current, fmt.Errorf("store: database schema v%d is newer than this binary (v%d)", current, latest)
	}
	if current >= m.version {
		return current, nil
	}

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return current, fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	// PRAGMA does not take parameters; m.version is an int from the filename.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return current, fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return current, fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	return m.version, nil
}

// loadMigrations reads NNNN_name.sql files and returns them oldest first.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}

	var migrations []migration
	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base, ok := strings.CutSuffix(entry.Name(), ".sql")
		if !ok {
			return nil, fmt.Errorf("store: migration %q is not a .sql file", entry.Name())
		}
		prefix, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("store: migration %q is not named NNNN_name.sql", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("store: migration %q has no positive numeric prefix", entry.Name())
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", other, entry.Name(), version)
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(fsys, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: entry.Name(), sql: string(body)})
	}

	if len(migrations) == 0 {
		return nil, errors.New("store: no migrations found")
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}
