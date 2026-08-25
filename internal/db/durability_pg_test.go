package db

// Postgres relaxes a commit with SET LOCAL, whose scope is the transaction. If it were ever
// written without LOCAL it would ride the pooled connection into unrelated writes and
// silently relax them — the Postgres twin of the SQLite pragma leak.
// specs/durability-levels.md §5.

import (
	"context"
	"os"
	"testing"
)

func openPgAt(t *testing.T, level Durability) *DB {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set; SET LOCAL is a PostgreSQL path")
	}
	db, err := OpenPostgres(dsn, 4)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetDurability(level)
	return db
}

func TestDurability_PostgresRelaxIsTransactionScoped(t *testing.T) {
	db := openPgAt(t, DurabilityOnlyOnce)
	ctx := context.Background()

	tx, _, exec, err := db.beginTxAt(ctx, syncStrict, nil) // relaxed at only-once
	if err != nil {
		t.Fatalf("beginTxAt: %v", err)
	}
	var inside string
	if err := exec.QueryRowContext(ctx, "select current_setting('synchronous_commit')").Scan(&inside); err != nil {
		tx.Rollback()
		t.Fatalf("read inside: %v", err)
	}
	if inside != "off" {
		tx.Rollback()
		t.Fatalf("inside a relaxed transaction synchronous_commit=%q, want \"off\" — the relax never took", inside)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The next transaction on the same pool must be back to synchronous: LOCAL is what
	// makes the relax end with the transaction rather than follow the connection.
	tx2, _, exec2, err := db.beginTxAt(ctx, syncAlways, nil)
	if err != nil {
		t.Fatalf("beginTxAt (2): %v", err)
	}
	defer tx2.Rollback()
	var after string
	if err := exec2.QueryRowContext(ctx, "select current_setting('synchronous_commit')").Scan(&after); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after == "off" {
		t.Fatal("synchronous_commit is still off in a later transaction; the relax escaped its own transaction")
	}
}

func TestDurability_PostgresStrictNeverRelaxes(t *testing.T) {
	db := openPgAt(t, DurabilityStrict)
	ctx := context.Background()
	tx, _, exec, err := db.beginTxAt(ctx, syncStrict, nil) // NOT relaxed: level is strict
	if err != nil {
		t.Fatalf("beginTxAt: %v", err)
	}
	defer tx.Rollback()
	var got string
	if err := exec.QueryRowContext(ctx, "select current_setting('synchronous_commit')").Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == "off" {
		t.Fatal("synchronous_commit=off at --durability=strict; every commit must be flushed there")
	}
}
