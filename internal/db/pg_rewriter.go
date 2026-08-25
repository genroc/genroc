package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	dbgen "genroc/internal/db/gen"
)

// pgRewriter translates the SQLite-generated placeholders (?N / ?) to Postgres $N at
// execution: one binary compiles against the SQLite sqlc package and must rewrite before
// talking to a Postgres connection.
type pgRewriter struct{ dbgen.DBTX }

func (r pgRewriter) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return r.DBTX.ExecContext(ctx, rewritePlaceholders(q), args...)
}
func (r pgRewriter) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	return r.DBTX.PrepareContext(ctx, rewritePlaceholders(q))
}
func (r pgRewriter) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return r.DBTX.QueryContext(ctx, rewritePlaceholders(q), args...)
}
func (r pgRewriter) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return r.DBTX.QueryRowContext(ctx, rewritePlaceholders(q), args...)
}

// rewritePlaceholders converts SQLite placeholder syntax to PostgreSQL:
//   - ?N  (named positional, e.g. ?1) → $N   (same index, parameter reused)
//   - ?   (plain positional)          → $N   (auto-incremented counter)
func rewritePlaceholders(query string) string {
	var b strings.Builder
	b.Grow(len(query))
	n := 0
	for i := 0; i < len(query); {
		if query[i] != '?' {
			b.WriteByte(query[i])
			i++
			continue
		}
		j := i + 1
		for j < len(query) && query[j] >= '0' && query[j] <= '9' {
			j++
		}
		b.WriteByte('$')
		if j > i+1 {
			b.WriteString(query[i+1 : j]) // ?N → $N
		} else {
			n++
			fmt.Fprintf(&b, "%d", n) // ? → $counter
		}
		i = j
	}
	return b.String()
}

// beginTx starts a transaction that fsyncs at every durability level. Everything not
// deliberately classified goes through here: see beginTxAt.
func (db *DB) beginTx(ctx context.Context, opts *sql.TxOptions) (*syncTx, *dbgen.Queries, dbgen.DBTX, error) {
	return db.beginTxAt(ctx, syncAlways, opts)
}

// beginTxAt starts a transaction whose commit is flushed only when the configured level is
// at or above floor, returning the raw handle, a *dbgen.Queries, and a DBTX executor (the
// latter two pgRewriter-wrapped on Postgres). Use the returned executor, not the raw
// *sql.Tx, for hand-written SQL so ? placeholders work on both engines.
//
// The two engines relax a commit in different places and neither is portable: Postgres
// takes SET LOCAL inside the transaction, SQLite a PRAGMA on the connection — which is why
// SQLite pins one (specs/durability-levels.md §5).
func (db *DB) beginTxAt(ctx context.Context, floor Durability, opts *sql.TxOptions) (*syncTx, *dbgen.Queries, dbgen.DBTX, error) {
	relaxed := !db.level().syncs(floor)

	s := &syncTx{db: db}
	if relaxed && db.dialect == "sqlite" {
		// Pinned, not set through the pool: with one connection a PRAGMA issued separately
		// could be overtaken by another goroutine's write, which would then commit at this
		// transaction's level instead of its own.
		conn, err := db.sqldb.Conn(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA synchronous = NORMAL"); err != nil {
			conn.Close()
			return nil, nil, nil, fmt.Errorf("relax sqlite durability: %w", err)
		}
		s.conn = conn
	}

	var err error
	if s.conn != nil {
		s.tx, err = s.conn.BeginTx(ctx, opts)
	} else {
		s.tx, err = db.sqldb.BeginTx(ctx, opts)
	}
	if err != nil {
		s.release()
		return nil, nil, nil, err
	}

	var dbtx dbgen.DBTX = s.tx
	if db.dialect == "postgres" {
		dbtx = pgRewriter{dbtx}
		if relaxed {
			// LOCAL: scoped to this transaction, so the pooled connection carries nothing
			// back to the next caller.
			if _, err := dbtx.ExecContext(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
				s.Rollback()
				return nil, nil, nil, fmt.Errorf("relax postgres durability: %w", err)
			}
		}
	}
	return s, dbgen.New(dbtx), dbtx, nil
}

// syncTx is a transaction plus the connection pinned for it. Only SQLite pins one: its
// durability lives on the connection, so the pragma has to be put back before the
// connection returns to the pool or the next unpinned write inherits this one's level.
type syncTx struct {
	tx   *sql.Tx
	conn *sql.Conn
	db   *DB
}

func (s *syncTx) Commit() error {
	err := s.tx.Commit()
	s.release()
	return err
}

// Rollback is idempotent, so the `defer tx.Rollback()` that follows every Commit still
// works — and it is what releases the pinned connection on the error paths.
func (s *syncTx) Rollback() error {
	err := s.tx.Rollback()
	s.release()
	return err
}

func (s *syncTx) release() {
	if s.conn == nil {
		return
	}
	// Restored unconditionally, including after a failed commit: an unrestored connection
	// silently downgrades every later write on it, which no test would attribute here.
	s.conn.ExecContext(context.Background(), "PRAGMA synchronous = "+s.db.sqliteBaseSync)
	s.conn.Close()
	s.conn = nil
}

// withTx runs fn inside a transaction that fsyncs at every level.
func (db *DB) withTx(ctx context.Context, fn func(qtx *dbgen.Queries, exec dbgen.DBTX) error) error {
	return db.withTxAt(ctx, syncAlways, fn)
}

// withTxAt runs fn inside a transaction flushed only at or above floor, committing on
// success and rolling back on error. fn receives the pgRewriter-wrapped *dbgen.Queries and
// DBTX executor — use those, never the raw *sql.Tx, for hand-written SQL so ? placeholders
// keep working on both engines.
func (db *DB) withTxAt(ctx context.Context, floor Durability, fn func(qtx *dbgen.Queries, exec dbgen.DBTX) error) error {
	tx, qtx, exec, err := db.beginTxAt(ctx, floor, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(qtx, exec); err != nil {
		return err
	}
	return tx.Commit()
}
