package db

// Flush is the only_once bracket's primitive: it makes the claim behind it durable at a
// level where ordinary commits are not flushed. specs/durability-levels.md s4.

import (
	"context"
	"path/filepath"
	"testing"
)

// markerN reads the row itself, which is the point of these tests: they are about SQLite's
// half of Flush, where a page HAS to change for there to be anything to fsync. Everything
// asking the engine-level question "did the bracket fire" uses FlushCount instead, which
// means the same thing on both engines.
func markerN(t *testing.T, db *DB) int64 {
	t.Helper()
	var n int64
	if err := db.sqldb.QueryRow("select n from durability_marker where id = 1").Scan(&n); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return n
}

func TestFlush_WritesAtRelaxedLevels(t *testing.T) {
	db := openTempSQLite(t, DurabilityOnlyOnce)
	before := markerN(t, db)
	if err := db.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := markerN(t, db); got != before+1 {
		t.Fatalf("marker %d -> %d; Flush must commit something, or there is no fsync and the claim stays unhardened", before, got)
	}
}

func TestFlush_IsANoOpAtStrict(t *testing.T) {
	// At strict every commit is already flushed, so a Flush would be a second fsync for a
	// guarantee that is already held -- on the hottest path there is.
	db := openTempSQLite(t, DurabilityStrict)
	before := markerN(t, db)
	if err := db.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := markerN(t, db); got != before {
		t.Fatalf("marker moved %d -> %d at strict; Flush must cost nothing where every commit already flushes", before, got)
	}
}

func TestFlush_RunsAtFullSynchronous(t *testing.T) {
	// The point of Flush is the fsync, so it must not itself be relaxed. Observable the
	// same way the relaxed writes are: the connection it borrowed goes back at the base
	// level, and a Flush that had relaxed it would leave NORMAL behind.
	db := openTempSQLite(t, DurabilityOnlyOnce)
	if err := db.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := pragmaSynchronous(t, db); got != sqliteSyncFull {
		t.Fatalf("after Flush: synchronous=%d, want %d", got, sqliteSyncFull)
	}
}

func TestFlush_MarkerSurvivesReopen(t *testing.T) {
	// Migration 032 seeds the row. Without it the UPDATE matches nothing, Flush commits
	// nothing, and it silently stops being a flush -- the failure this whole mechanism
	// cannot afford, and the one that leaves no trace.
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "m.db"), "FULL")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.sqldb.QueryRow("select count(*) from durability_marker").Scan(&n); err != nil {
		t.Fatalf("count marker rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("durability_marker has %d rows, want exactly 1 seeded by the migration", n)
	}
}
