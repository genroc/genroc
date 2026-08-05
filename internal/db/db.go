package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"

	dbgen "genroc/internal/db/gen"
)

//go:embed migrations/*.sql
var sqlMigrations embed.FS

//go:embed pg_functions.sql
var pgFunctionsSQL string

// DB wraps a *sql.DB and implements all persistence for both SQLite and PostgreSQL.
type DB struct {
	sqldb   *sql.DB
	q       *dbgen.Queries
	exec    dbgen.DBTX // rewrites ?→$N on Postgres; use for hand-written SQL
	dialect string     // "sqlite" | "postgres"

	// defCache memoises GetDefinition (the hottest read; contends with SQLite's single
	// connection). Raw JSON keyed by (name, version), re-unmarshalled per call so callers
	// never share Task pointers; SaveDefinition invalidates for the ON CONFLICT overwrite.
	defCache sync.Map // defKey → string

	// Audit logs are best-effort, decoupled from instance state (migration 008): AppendLog
	// buffers, logFlusher batch-inserts, and reads/prune flush first so appends stay visible.
	// A crash drops only buffered rows — an observability gap, never state corruption.
	logMu      sync.Mutex
	logBuf     []dbgen.InsertLogParams
	logStop    chan struct{} // closed by Close() to stop the flusher
	logStopped chan struct{} // closed by the flusher after its final flush

	// objectRetentionMs is the window a dereferenced process_objects row survives
	// before GC, mirroring the audit-log retention so a log referencing an object
	// outlives the log itself. Set by the engine at startup (SetObjectRetention);
	// 0 means "keep forever" — dereferenced objects are left pinned (never swept),
	// consistent with logs-forever.
	objectRetentionMs atomic.Int64
}

type defKey struct {
	name    string
	version int
}

// OpenSQLite opens (or creates) the SQLite database at path and runs migrations.
// synchronous is the PRAGMA synchronous level (empty = NORMAL): NORMAL fsyncs the WAL
// only at checkpoints (fast; recent commits can be lost on power loss, DB stays
// consistent), FULL fsyncs per commit (power-loss durable, matching Postgres). The
// genroc binary defaults its flag to FULL; OFF and EXTRA are also accepted.
func OpenSQLite(path, synchronous string) (*DB, error) {
	sync, err := sqliteSynchronous(synchronous)
	if err != nil {
		return nil, err
	}
	dsn := path + "?_journal_mode=WAL&_synchronous=" + sync + "&_foreign_keys=ON&_busy_timeout=5000"
	sqldb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite supports only one writer at a time.
	return open(sqldb, "sqlite")
}

// sqliteSynchronous whitelists the PRAGMA synchronous level placed on the DSN, so a
// flag value can never inject extra connection parameters. Empty defaults to NORMAL.
func sqliteSynchronous(mode string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "", "NORMAL":
		return "NORMAL", nil
	case "OFF":
		return "OFF", nil
	case "FULL":
		return "FULL", nil
	case "EXTRA":
		return "EXTRA", nil
	default:
		return "", fmt.Errorf("invalid sqlite synchronous mode %q (want OFF, NORMAL, FULL, or EXTRA)", mode)
	}
}

// OpenPostgres opens a PostgreSQL connection and runs migrations. maxOpenConns caps
// the pool (idle = half; <= 0 defaults to 50); size a worker fleet so
// workers*maxOpenConns stays under the server's max_connections.
func OpenPostgres(dsn string, maxOpenConns int) (*DB, error) {
	sqldb, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if maxOpenConns <= 0 {
		maxOpenConns = 50
	}
	sqldb.SetMaxOpenConns(maxOpenConns)
	sqldb.SetMaxIdleConns(max(maxOpenConns/2, 1))
	return open(sqldb, "postgres")
}

func open(sqldb *sql.DB, dialect string) (*DB, error) {
	if err := runMigrations(sqldb, dialect); err != nil {
		sqldb.Close()
		return nil, err
	}
	if dialect == "postgres" {
		if err := bootstrapPostgres(sqldb); err != nil {
			sqldb.Close()
			return nil, err
		}
	}
	var dbtx dbgen.DBTX = sqldb
	if dialect == "postgres" {
		dbtx = pgRewriter{dbtx}
	}
	db := &DB{
		sqldb:      sqldb,
		q:          dbgen.New(dbtx),
		exec:       dbtx,
		dialect:    dialect,
		logStop:    make(chan struct{}),
		logStopped: make(chan struct{}),
	}
	go db.logFlusher()
	return db, nil
}

// pgBootstrapLockKey is the advisory-lock key that serializes bootstrapPostgres
// across concurrently-starting workers. Any fixed int64 works (it only needs to be
// the same for every worker); this one spells "genroc".
const pgBootstrapLockKey int64 = 0x67656E74 // "genroc"

// bootstrapPostgres runs the post-migration Postgres-only setup (json_each helper +
// aggressive autovacuum on process_instances). Both rewrite a system-catalog tuple, so
// concurrent worker starts race ("tuple concurrently updated"); a transaction-scoped
// advisory lock serializes the block and the losers re-apply it idempotently.
func bootstrapPostgres(sqldb *sql.DB) error {
	ctx := context.Background()
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin postgres bootstrap: %w", err)
	}
	defer tx.Rollback()

	// Held until the transaction ends (commit below), so only one worker is inside
	// the bootstrap at a time.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgBootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx, pgFunctionsSQL); err != nil {
		return fmt.Errorf("create json_each function: %w", err)
	}

	// High-churn queue table: completions leave dead tuples in idx_instances_runnable that
	// every claim must skip until vacuumed. Aggressive unthrottled autovacuum reclaims them
	// promptly (SQLite updates in place — no equivalent). See CLAUDE.md.
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE process_instances SET (
			autovacuum_vacuum_scale_factor = 0.02,
			autovacuum_vacuum_threshold    = 50,
			autovacuum_vacuum_cost_delay   = 0
		)`); err != nil {
		return fmt.Errorf("tune process_instances autovacuum: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit postgres bootstrap: %w", err)
	}
	return nil
}

// Ping verifies a connection to the database is still usable, acquiring one from the pool
// if none is idle. It is the health endpoint's readiness check: an engine whose database is
// unreachable can claim nothing, so a worker in that state should not be routed to.
func (db *DB) Ping(ctx context.Context) error { return db.sqldb.PingContext(ctx) }

// Dialect reports the engine backing this DB: "sqlite" or "postgres".
func (db *DB) Dialect() string { return db.dialect }

// Close flushes buffered audit-log rows, stops the flusher, and closes the pool.
func (db *DB) Close() error {
	close(db.logStop)
	<-db.logStopped
	return db.sqldb.Close()
}

// pageInfo runs the before/after counts for a page bounded by first/last (display
// order; nil for an empty page) and assembles PageInfo. A cursor is set only for a
// direction that has more rows, so cursor presence is the has-more signal.
func (db *DB) pageInfo(b built, first, last []any) (PageInfo, error) {
	query, args := b.countQuery(first, last)
	var before, after int64
	if err := db.exec.QueryRowContext(context.Background(), query, args...).Scan(&before, &after); err != nil {
		return PageInfo{}, err
	}
	order := "asc"
	if b.desc {
		order = "desc"
	}
	info := PageInfo{
		Size:        b.limit,
		ItemsBefore: before,
		ItemsAfter:  after,
		Sort:        b.sort,
		Order:       order,
	}
	var err error
	if before > 0 {
		if info.Before, err = encodeCursor(b.sort, b.desc, b.mode, first); err != nil {
			return PageInfo{}, err
		}
	}
	if after > 0 {
		if info.After, err = encodeCursor(b.sort, b.desc, b.mode, last); err != nil {
			return PageInfo{}, err
		}
	}
	return info, nil
}

// ── time helpers ─────────────────────────────────────────────────────────────

// All DB timestamps are unix milliseconds (BIGINT columns).

// clockOffset (milliseconds) shifts this process's notion of "now" for all DB
// reads/writes. Only ever increased, via AdvanceClock (debug /tick endpoint),
// so tests can expire leases and retry timers without real waits.
var clockOffset atomic.Int64

func nowMillis() int64 { return time.Now().UnixMilli() + clockOffset.Load() }

// AdvanceClock shifts the DB clock forward by d. Testing only.
func AdvanceClock(d time.Duration) time.Duration {
	return time.Duration(clockOffset.Add(d.Milliseconds())) * time.Millisecond
}

// Now returns the current time as seen by the DB clock (including any test
// offset). Anything compared against DB timestamps must use this, not time.Now.
func Now() time.Time { return toTime(nowMillis()) }

func toTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toTimePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := toTime(n.Int64)
	return &t
}

func fromTimePtr(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
