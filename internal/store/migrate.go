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

	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	latest := migrations[len(migrations)-1].version
	if current > latest {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d)", current, latest)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %d: %w", m.version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	// PRAGMA does not take parameters; m.version is an int from the filename.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %s: %w", m.name, err)
	}
	return nil
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
