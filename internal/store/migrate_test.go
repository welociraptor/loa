package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		"no underscore":       {"migrations/0001.sql": {Data: []byte("SELECT 1;")}},
		"no .sql suffix":      {"migrations/0001_one.sql.bak": {Data: []byte("SELECT 1;")}},
		"no numeric prefix":   {"migrations/initial.sql": {Data: []byte("SELECT 1;")}},
		"non-numeric version": {"migrations/abc_one.sql": {Data: []byte("SELECT 1;")}},
		"zero version":        {"migrations/0000_x.sql": {Data: []byte("SELECT 1;")}},
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

func TestMigrationDirectoriesAreIgnored(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_one.sql":    {Data: []byte("SELECT 1;")},
		"migrations/nested/0002.sql": {Data: []byte("SELECT 1;")},
	}
	migrations, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) != 1 || migrations[0].name != "0001_one.sql" {
		t.Errorf("migrations = %v, want only 0001_one.sql", migrations)
	}
}

// applyMigrationsUpTo runs the real embedded migrations through latest, as a
// database in the field would have before a newer binary opened it.
func applyMigrationsUpTo(t *testing.T, db *sql.DB, latest int) {
	t.Helper()
	migrations, err := loadMigrations(migrationsFS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, m := range migrations {
		if m.version > latest {
			break
		}
		if err := applyMigration(context.Background(), db, m); err != nil {
			t.Fatalf("apply migration %d: %v", m.version, err)
		}
	}
}

// TestRealMigrationsFromEveryVersion opens a database at each shipped version,
// fills it through the columns that exist there, and checks that the real chain
// to the newest version keeps the rows and gives the added columns their
// defaults. A new migration adds a case here.
func TestRealMigrationsFromEveryVersion(t *testing.T) {
	ctx := context.Background()
	const created = "2024-01-02T03:04:05Z"

	for n := 1; n <= 9; n++ {
		t.Run(fmt.Sprintf("from_v%d", n), func(t *testing.T) {
			db := rawDB(t)
			applyMigrationsUpTo(t, db, n)
			if got := version(t, db); got != n {
				t.Fatalf("user_version = %d, want %d", got, n)
			}

			// Only columns present since v1 are named, so this insert works at
			// every starting version. The scope and dependency give ALTER
			// TABLE something to preserve besides the task row itself.
			res, err := db.ExecContext(ctx,
				`INSERT INTO tasks (title, body, created_at, updated_at) VALUES (?, ?, ?, ?)`,
				"survivor", "kept across the chain", created, created)
			if err != nil {
				t.Fatalf("insert task at v%d: %v", n, err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("task id: %v", err)
			}

			res, err = db.ExecContext(ctx,
				`INSERT INTO tasks (title, created_at, updated_at) VALUES (?, ?, ?)`,
				"prerequisite", created, created)
			if err != nil {
				t.Fatalf("insert dependency at v%d: %v", n, err)
			}
			dep, err := res.LastInsertId()
			if err != nil {
				t.Fatalf("dependency id: %v", err)
			}

			if _, err := db.ExecContext(ctx,
				`INSERT INTO task_scopes (task_id, scope) VALUES (?, ?)`, id, "repo:loa"); err != nil {
				t.Fatalf("insert scope at v%d: %v", n, err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO task_deps (task_id, depends_on) VALUES (?, ?)`, id, dep); err != nil {
				t.Fatalf("insert dependency edge at v%d: %v", n, err)
			}

			if err := migrate(ctx, db, migrationsFS); err != nil {
				t.Fatalf("migrate to the newest version: %v", err)
			}
			if got := version(t, db); got != 9 {
				t.Errorf("user_version = %d, want 9", got)
			}
			if !hasTable(t, db, "runs") {
				t.Error("runs table missing after the chain")
			}

			var title, workspace, role, prompt, promptHash, workspaceID, paneID, verdict, verdictNotes string
			var checkCommand, checkOutput, mergedAt, mergeCommit string
			var runMS int64
			var checkPassed int
			err = db.QueryRowContext(ctx,
				`SELECT title, workspace, role, prompt, prompt_hash, workspace_id, pane_id, run_ms,
				        verdict, verdict_notes, check_command, check_passed, check_output,
				        merged_at, merge_commit
				 FROM tasks WHERE id = ?`, id).
				Scan(&title, &workspace, &role, &prompt, &promptHash, &workspaceID, &paneID, &runMS,
					&verdict, &verdictNotes, &checkCommand, &checkPassed, &checkOutput,
					&mergedAt, &mergeCommit)
			if err != nil {
				t.Fatalf("read migrated task: %v", err)
			}
			if title != "survivor" {
				t.Errorf("title = %q, want the row inserted before the migration", title)
			}
			if workspace != "" || workspaceID != "" || paneID != "" || runMS != 0 {
				t.Errorf("new columns = (%q, %q, %q, %d), want empty defaults",
					workspace, workspaceID, paneID, runMS)
			}
			if role != "implement" || prompt != "" || promptHash != "" {
				t.Errorf("role/prompt/hash = (%q, %q, %q), want implement with an empty dispatch record",
					role, prompt, promptHash)
			}
			if verdict != "" || verdictNotes != "" {
				t.Errorf("verdict/notes = (%q, %q), want empty defaults", verdict, verdictNotes)
			}
			if checkCommand != "" || checkPassed != 0 || checkOutput != "" {
				t.Errorf("check = (%q, %d, %q), want empty defaults", checkCommand, checkPassed, checkOutput)
			}
			if mergedAt != "" || mergeCommit != "" {
				t.Errorf("merge = (%q, %q), want empty defaults", mergedAt, mergeCommit)
			}

			var scope string
			if err := db.QueryRowContext(ctx,
				`SELECT scope FROM task_scopes WHERE task_id = ?`, id).Scan(&scope); err != nil {
				t.Fatalf("read migrated scope: %v", err)
			}
			if scope != "repo:loa" {
				t.Errorf("scope = %q, want repo:loa", scope)
			}

			var deps int
			if err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM task_deps WHERE task_id = ? AND depends_on = ?`, id, dep).
				Scan(&deps); err != nil {
				t.Fatalf("read migrated dependency: %v", err)
			}
			if deps != 1 {
				t.Errorf("dependency rows = %d, want 1", deps)
			}
		})
	}
}

// TestRealSchemaSnapshot pins the full schema produced by the real migrations.
// A change to any migration changes this file, forcing a review; a schema
// change belongs in a new migration, not an edit to a shipped one.
func TestRealSchemaSnapshot(t *testing.T) {
	db := rawDB(t)
	if err := migrate(context.Background(), db, migrationsFS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rows, err := db.QueryContext(context.Background(),
		`SELECT type, name, coalesce(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()

	var got strings.Builder
	for rows.Next() {
		var typ, name, sql string
		if err := rows.Scan(&typ, &name, &sql); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		// An implicit index has no SQL; its empty body is part of the record.
		fmt.Fprintf(&got, "-- %s %s\n%s\n\n", typ, name, sql)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		//nolint:gosec // G306: a schema fixture is not secret and is committed.
		if err := os.WriteFile("testdata/schema.golden", []byte(got.String()), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile("testdata/schema.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got.String() != string(want) {
		t.Errorf("schema differs from testdata/schema.golden; a schema change belongs "+
			"in a new migration (run UPDATE_GOLDEN=1 go test to record it)\ngot:\n%s\nwant:\n%s",
			got.String(), want)
	}
}

// TestMigrationRollsBackOnFailure proves a step is atomic: the valid half of a
// failed migration must not survive with the version left behind.
func TestMigrationRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)

	fsys := fstest.MapFS{
		"migrations/0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER);")},
		"migrations/0002_two.sql": {Data: []byte(
			"CREATE TABLE two (id INTEGER);\nINSERT INTO missing_table (id) VALUES (1);")},
	}
	if err := migrate(ctx, db, fsys); err == nil {
		t.Fatal("migrate accepted invalid SQL")
	}
	if got := version(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
	if !hasTable(t, db, "one") {
		t.Error("the applied step was lost")
	}
	if hasTable(t, db, "two") {
		t.Error("the failed step left a table behind")
	}
}

// TestMigrateStopsAtTheFirstFailure proves a later valid step is not applied
// after an earlier one fails.
func TestMigrateStopsAtTheFirstFailure(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t)

	fsys := fstest.MapFS{
		"migrations/0001_one.sql":   {Data: []byte("CREATE TABLE one (id INTEGER);")},
		"migrations/0002_two.sql":   {Data: []byte("CREATE TABLE two (id INTEGER) nonsense;")},
		"migrations/0003_three.sql": {Data: []byte("CREATE TABLE three (id INTEGER);")},
	}
	if err := migrate(ctx, db, fsys); err == nil {
		t.Fatal("migrate accepted invalid SQL")
	}
	if got := version(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
	if !hasTable(t, db, "one") {
		t.Error("the first step was not applied")
	}
	if hasTable(t, db, "three") {
		t.Error("a step after the failure was applied")
	}
}

// TestConcurrentOpenMigratesOnce is blocked: two simultaneous store.Open calls
// on a fresh file race in production. Each reads user_version before the other
// commits, so the loser re-runs 0001 ("table tasks already exists"), and the
// journal_mode(WAL) pragma runs before busy_timeout, so a connection can also
// fail with SQLITE_BUSY during Ping. Unskip this once migrate serialises the
// version read with the migration transaction and busy_timeout precedes WAL
// conversion; do not weaken the assertions to make it pass.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	t.Skip("known production race: concurrent store.Open re-applies a migration; see migrate.go")

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")

	const openers = 2
	start := make(chan struct{})
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	for range openers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(ctx, path)
			if err != nil {
				errs <- err
				return
			}
			if err := s.Close(); err != nil {
				errs <- fmt.Errorf("close: %w", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Open: %v", err)
	}

	db := rawDBAt(t, path)
	if got := version(t, db); got != 9 {
		t.Errorf("user_version = %d, want 9", got)
	}
	if !hasTable(t, db, "tasks") {
		t.Error("migrations did not produce the schema")
	}
}

// rawDBAt opens an existing database without applying migrations.
func rawDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
