package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
)

// The backfill in migration 040 runs exactly once, over rows nobody can produce again, so it
// is the one part of the change that no ordinary test exercises: every test database is
// created empty and migrated before a row exists. This runs the migration's OWN statements --
// read from the file, not copied -- over a tree built in the pre-migration shape.
func TestMigration040BackfillsTheTree(t *testing.T) {
	dir := t.TempDir()
	sqldb, err := sql.Open("sqlite3", dir+"/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()

	// The columns the backfill reads and writes, at their pre-migration defaults.
	if _, err := sqldb.Exec(`
		CREATE TABLE process_instances (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL DEFAULT '',
		                                root_id TEXT NOT NULL DEFAULT '');
		CREATE TABLE process_logs (id TEXT PRIMARY KEY, instance_id TEXT NOT NULL,
		                           root_id TEXT NOT NULL DEFAULT '');`); err != nil {
		t.Fatal(err)
	}
	// root → kid → grandkid, a second root, and a log row whose instance is already gone.
	for _, r := range [][2]string{{"root", ""}, {"kid", "root"}, {"grandkid", "kid"}, {"other", ""}} {
		if _, err := sqldb.Exec(`INSERT INTO process_instances (id, parent_id) VALUES (?, ?)`, r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	for i, inst := range []string{"root", "kid", "grandkid", "other", "pruned-away"} {
		if _, err := sqldb.Exec(`INSERT INTO process_logs (id, instance_id) VALUES (?, ?)`, i, inst); err != nil {
			t.Fatal(err)
		}
	}

	for _, stmt := range backfillStatements(t) {
		if _, err := sqldb.Exec(stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}

	wantInstances := map[string]string{
		"root": "root", "kid": "root", "grandkid": "root", "other": "other",
	}
	for id, want := range wantInstances {
		var got string
		if err := sqldb.QueryRow(`SELECT root_id FROM process_instances WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("instance %s: want root %s, got %s", id, want, got)
		}
	}

	wantLogs := map[string]string{
		"root": "root", "kid": "root", "grandkid": "root", "other": "other",
		// An orphan keeps its own id as its root, so it stays addressable by the only id it
		// names rather than dropping out of every listing.
		"pruned-away": "pruned-away",
	}
	for inst, want := range wantLogs {
		var got string
		if err := sqldb.QueryRow(`SELECT root_id FROM process_logs WHERE instance_id = ?`, inst).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("log on %s: want root %s, got %s", inst, want, got)
		}
	}
}

// The indexes are what the whole change bought -- 23 buffers per page against 18,616 -- and
// losing one makes no noise: every query still returns the right rows, by scanning. Not
// hypothetical here: migration 012 had to hand-recreate a partial index because SQLite's
// ALTER TABLE forced a table rebuild, and the next migration that rebuilds either table has
// the same line to remember. Asserted on the SCHEMA rather than on a query plan, which is the
// optimiser's business and not a promise anyone made.
func TestMigration040LeavesItsIndexes(t *testing.T) {
	dir := t.TempDir()
	sqldb, err := sql.Open("sqlite3", dir+"/idx.db")
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	if err := runMigrations(sqldb, "sqlite"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ index, table, cols string }{
		{"idx_process_logs_root", "process_logs", "(root_id, created_at, id)"},
		{"idx_instances_root", "process_instances", "(root_id)"},
	} {
		var ddl string
		err := sqldb.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
			want.index).Scan(&ddl)
		if errors.Is(err, sql.ErrNoRows) {
			t.Errorf("%s is gone: %s reads by %s cost the table rather than the page",
				want.index, want.table, want.cols)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ReplaceAll(ddl, " ", ""), strings.ReplaceAll(want.cols, " ", "")) {
			t.Errorf("%s is on %q, not %s -- the keyset page needs the sort columns in the index",
				want.index, ddl, want.cols)
		}
	}
}

// backfillStatements is the UPDATEs from the migration itself: a copy here could pass while
// the shipped statement is broken, which is the only failure this test exists to catch.
func backfillStatements(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile("migrations/040_root_id.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, stmt := range strings.Split(string(body), ";") {
		trimmed := strings.TrimSpace(stripSQLComments(stmt))
		if strings.HasPrefix(trimmed, "WITH RECURSIVE") || strings.HasPrefix(trimmed, "UPDATE") {
			out = append(out, trimmed)
		}
	}
	if len(out) != 2 {
		t.Fatalf("expected the two backfill statements, found %d", len(out))
	}
	return out
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
