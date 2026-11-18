package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "raw.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func version(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err != nil {
		t.Fatalf("look up table %q: %v", name, err)
	}
	return found == 1
}

func TestMigrationsApplyInOrderAndOnce(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)

	fsys := fstest.MapFS{
		"migrations/0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER);")},
		"migrations/0002_two.sql": {Data: []byte("CREATE TABLE two (id INTEGER);")},
	}

	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := version(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
	if !hasTable(t, db, "one") || !hasTable(t, db, "two") {
		t.Error("not every migration was applied")
	}

	// Running again must be a no-op, not a re-execution: 0001 would fail.
	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if got := version(t, db); got != 2 {
		t.Errorf("user_version = %d after a second run, want 2", got)
	}
}

func TestMigrationOnlyAppliesWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)

	first := fstest.MapFS{
		"migrations/0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER);")},
	}
	if err := migrate(ctx, db, first); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The v1 database now meets a binary that also has v2.
	second := fstest.MapFS{
		"migrations/0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER);")},
		"migrations/0002_two.sql": {Data: []byte("CREATE TABLE two (id INTEGER);")},
	}
	if err := migrate(ctx, db, second); err != nil {
		t.Fatalf("migrate to v2: %v", err)
	}
	if got := version(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
	if !hasTable(t, db, "two") {
		t.Error("v2 table missing")
	}
}

func TestDatabaseNewerThanBinaryIsRejected(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 7"); err != nil {
		t.Fatalf("set version: %v", err)
	}

	fsys := fstest.MapFS{
		"migrations/0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER);")},
	}
	err := migrate(ctx, db, fsys)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

func TestMigrationNamesAreChecked(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"no numeric prefix": {"migrations/initial.sql": {Data: []byte("SELECT 1;")}},
		"zero version":      {"migrations/0000_x.sql": {Data: []byte("SELECT 1;")}},
		"duplicate version": {
			"migrations/0001_a.sql": {Data: []byte("SELECT 1;")},
			"migrations/0001_b.sql": {Data: []byte("SELECT 1;")},
		},
		"empty": {},
	}
	for name, fsys := range cases {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
