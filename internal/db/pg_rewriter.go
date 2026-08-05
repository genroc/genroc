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

// beginTx starts a transaction, returning the raw *sql.Tx, a *dbgen.Queries, and a DBTX
// executor (the latter two pgRewriter-wrapped on Postgres). Use the returned executor,
// not the raw *sql.Tx, for hand-written SQL so ? placeholders work on both engines.
func (db *DB) beginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, *dbgen.Queries, dbgen.DBTX, error) {
	tx, err := db.sqldb.BeginTx(ctx, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	var dbtx dbgen.DBTX = tx
	if db.dialect == "postgres" {
		dbtx = pgRewriter{dbtx}
	}
	return tx, dbgen.New(dbtx), dbtx, nil
}

// withTx runs fn inside a transaction, committing on success and rolling back on error.
// fn receives the pgRewriter-wrapped *dbgen.Queries and DBTX executor — use those, never
// the raw *sql.Tx, for hand-written SQL so ? placeholders keep working on both engines.
func (db *DB) withTx(ctx context.Context, fn func(qtx *dbgen.Queries, exec dbgen.DBTX) error) error {
	tx, qtx, exec, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(qtx, exec); err != nil {
		return err
	}
	return tx.Commit()
}
