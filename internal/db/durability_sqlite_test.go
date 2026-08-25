package db

// SQLite's durability lives on the connection, not the transaction, so a relaxed write has
// to put it back. Nothing else would notice if it did not: the next unpinned write would
// simply commit at the wrong level, silently and forever.
// specs/durability-levels.md §5.

import (
	"path/filepath"
	"testing"

	"genroc/internal/model"
)

func openTempSQLite(t *testing.T, level Durability) *DB {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "d.db"), "FULL")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetDurability(level)
	return db
}

// pragmaSynchronous reads the setting as an unpinned write would see it: through the pool,
// which with one connection is the same connection a relaxed transaction borrowed.
func pragmaSynchronous(t *testing.T, db *DB) int {
	t.Helper()
	var got int
	if err := db.sqldb.QueryRow("PRAGMA synchronous").Scan(&got); err != nil {
		t.Fatalf("read PRAGMA synchronous: %v", err)
	}
	return got
}

const sqliteSyncFull = 2 // 0=OFF 1=NORMAL 2=FULL 3=EXTRA

func runningInstance(t *testing.T, db *DB, id string) *model.ProcessInstance {
	t.Helper()
	inst := &model.ProcessInstance{
		ID: id, ProcessName: "test", ProcessVersion: 1, Task: "step1",
		ContextData: map[string]any{}, Status: model.StatusRunning,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return inst
}

func TestDurability_RelaxedWriteRestoresTheConnection(t *testing.T) {
	db := openTempSQLite(t, DurabilityOnlyOnce)
	inst := runningInstance(t, db, "restore-1")

	if got := pragmaSynchronous(t, db); got != sqliteSyncFull {
		t.Fatalf("before any relaxed write: synchronous=%d, want %d", got, sqliteSyncFull)
	}
	// A checkpoint is the relaxed path at only-once, so this transaction runs at NORMAL.
	if err := db.UpdateInstanceProgress(inst); err != nil {
		t.Fatalf("UpdateInstanceProgress: %v", err)
	}
	if got := pragmaSynchronous(t, db); got != sqliteSyncFull {
		t.Fatalf("after a relaxed write: synchronous=%d, want %d — the connection went back to the pool "+
			"still relaxed, so every later write silently commits at the wrong level", got, sqliteSyncFull)
	}
}

func TestDurability_RestoredEvenWhenTheWriteIsRefused(t *testing.T) {
	db := openTempSQLite(t, DurabilityOnlyOnce)
	inst := runningInstance(t, db, "restore-2")

	// A fenced write nobody granted: the transaction rolls back rather than commits, which
	// is the path that would leak a relaxed connection if only Commit restored it.
	inst.LeaseEpoch = 99
	if err := db.UpdateInstanceProgress(inst); err == nil {
		t.Fatal("expected the fence to refuse a write made under no grant")
	}
	if got := pragmaSynchronous(t, db); got != sqliteSyncFull {
		t.Fatalf("after a refused relaxed write: synchronous=%d, want %d", got, sqliteSyncFull)
	}
}

func TestDurability_StrictNeverRelaxes(t *testing.T) {
	db := openTempSQLite(t, DurabilityStrict)
	inst := runningInstance(t, db, "strict-1")
	if err := db.UpdateInstanceProgress(inst); err != nil {
		t.Fatalf("UpdateInstanceProgress: %v", err)
	}
	if got := pragmaSynchronous(t, db); got != sqliteSyncFull {
		t.Fatalf("at strict: synchronous=%d, want %d", got, sqliteSyncFull)
	}
}

func TestDurability_DefaultsToStrictWithoutSetDurability(t *testing.T) {
	// The zero value of the atomic is only-once, so a DB nobody configured would run
	// relaxed. open() must have set strict instead.
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "d.db"), "FULL")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer db.Close()
	if got := db.level(); got != DurabilityStrict {
		t.Fatalf("a DB nobody called SetDurability on is at %s, want strict", got)
	}
}
