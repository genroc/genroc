package db

import (
	"database/sql"
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
