package db

// commit_delay is set per pooled connection rather than in postgresql.conf, so genroc's
// connections get it and no other database on the server pays for it. Postgres skips the
// delay entirely unless commit_siblings transactions are open, which is what keeps it off
// the narrow, causally-sequential workloads it would otherwise slow down.
// specs/durability-levels.md §6.

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// settingOn reads a GUC on a connection pinned by tx, so each caller is looking at a
// different pooled connection rather than the same one four times.
func settingOn(t *testing.T, tx *sql.Tx, name string) string {
	t.Helper()
	var got string
	if err := tx.QueryRow("select current_setting('" + name + "')").Scan(&got); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return got
}

func TestCommitDelay_AppliedToEveryPooledConnection(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set; commit_delay is a PostgreSQL setting")
	}

	// Pool wider than the connections pinned below: migration and bootstrap retain one,
	// so pinning maxOpenConns of them deadlocks on the last BeginTx.
	const pinned = 4
	database, err := OpenPostgres(dsn, pinned+4, WithCommitDelay(750))
	if err != nil {
		t.Fatalf("OpenPostgres with commit delay: %v", err)
	}
	defer database.Close()

	// Hold every connection in the pool open at once. A setting applied to whichever
	// connection happened to be handed out first would pass a single read and still be
	// wrong for the rest of the pool, which is where it has to hold.
	var txs []*sql.Tx
	for i := 0; i < pinned; i++ {
		tx, err := database.sqldb.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("pin connection %d: %v", i, err)
		}
		defer tx.Rollback()
		txs = append(txs, tx)
	}
	for i, tx := range txs {
		if got := settingOn(t, tx, "commit_delay"); got != "750" {
			t.Fatalf("pooled connection %d has commit_delay=%q, want \"750\"", i, got)
		}
	}
}

func TestCommitDelay_OffLeavesTheServerDefault(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set; commit_delay is a PostgreSQL setting")
	}
	// Give the server a non-zero delay of its own first. Against a stock server (0) this
	// test cannot tell "sent no SET" from "sent SET commit_delay = 0" — both read back 0 —
	// so it would pass without discriminating. Establishing the precondition is what makes
	// it a test rather than a coincidence.
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open (admin): %v", err)
	}
	defer admin.Close()
	var dbName string
	if err := admin.QueryRow("select current_database()").Scan(&dbName); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if _, err := admin.Exec(`ALTER DATABASE "` + dbName + `" SET commit_delay = 1500`); err != nil {
		t.Skipf("cannot set a server-side commit_delay to test against (%v); needs a superuser DSN", err)
	}
	t.Cleanup(func() { admin.Exec(`ALTER DATABASE "` + dbName + `" RESET commit_delay`) })

	// ALTER DATABASE reaches only sessions opened after it, so the oracle must be a pool
	// opened here — reading through `admin` races its own pre-ALTER connection. It is a raw
	// driver handle rather than OpenPostgres-with-no-options for a second reason: both
	// OpenPostgres paths run the same option code, so they move together under a bug and
	// comparing them proves nothing. This reference has never been near it.
	raw, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	var server string
	if err := raw.QueryRow("select current_setting('commit_delay')").Scan(&server); err != nil {
		t.Fatalf("read server commit_delay: %v", err)
	}
	if server == "0" {
		t.Fatalf("precondition did not take: server commit_delay is still 0")
	}

	// Zero must send no SET at all, so whatever the server configured survives — only
	// observable on a server that configured something, which is why this reads the
	// server's own value rather than asserting "0".
	withZero, err := OpenPostgres(dsn, 2, WithCommitDelay(0))
	if err != nil {
		t.Fatalf("OpenPostgres (delay 0): %v", err)
	}
	defer withZero.Close()

	tx, err := withZero.sqldb.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if got := settingOn(t, tx, "commit_delay"); got != server {
		t.Fatalf("commit_delay = %q with the option off, want the server's own %q", got, server)
	}
}
