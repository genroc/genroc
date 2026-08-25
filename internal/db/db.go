package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
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

	// flushes counts successful Flush calls, for tests and diagnostics. In process, not
	// read back from durability_marker: that row only moves on SQLite, so a test built on
	// it would quietly assert nothing on Postgres.
	flushes atomic.Int64
	// durability is the ladder level every write is measured against
	// (specs/durability-levels.md §5). Set once at startup via SetDurability; atomic
	// because it is read on every write path from every worker goroutine.
	durability atomic.Int64
	// sqliteBaseSync is the PRAGMA synchronous level an unrelaxed write runs at, and the
	// value a relaxed transaction restores when it hands the connection back. It is the
	// operator's --sqlite-synchronous: durability lowers writes beneath this, never above.
	sqliteBaseSync string

	// defCache memoises GetDefinition (the hottest read; contends with SQLite's single
	// connection). Raw JSON keyed by (name, version), re-unmarshalled per call so callers
	// never share Task pointers; SaveDefinition invalidates for the ON CONFLICT overwrite.
	defCache sync.Map // defKey → string

	// Audit logs are best-effort, decoupled from instance state (migration 008): AppendLog
	// buffers, logFlusher batch-inserts, and reads/prune flush first so appends stay visible.
	// A crash drops only buffered rows — an observability gap, never state corruption.
	logMu sync.Mutex // guards logBuf only; never held across the insert
	// logFlushMu spans a flush's detach *and* its insert, so a reader that flushes while
	// another goroutine is mid-flush waits for that batch instead of finding the buffer
	// empty and querying without it.
	logFlushMu sync.Mutex
	logBuf     []dbgen.InsertLogParams
	logStop    chan struct{} // closed by Close() to stop the flusher
	logStopped chan struct{} // closed by the flusher after its final flush

	// objectGraceMs: how long a RELEASED object stays fetchable, so a reference already handed
	// out still resolves after the data moved on. specs/object-store.md.
	objectGraceMs atomic.Int64

	// objectRetentionMs: how long a log's claim on an object survives before GC,
	// mirroring log retention so a log referencing an object outlives the log. Set by the
	// engine at startup; 0 = keep forever, consistent with logs-forever.
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
// Pass WithFullFsync to make FULL mean what it says on macOS.
func OpenSQLite(path, synchronous string, opts ...SQLiteOption) (*DB, error) {
	sync, err := sqliteSynchronous(synchronous)
	if err != nil {
		return nil, err
	}
	var cfg sqliteConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	dsn := path + "?_journal_mode=WAL&_synchronous=" + sync + "&_foreign_keys=ON&_busy_timeout=5000"
	sqldb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite supports only one writer at a time.
	if cfg.fullFsync {
		// Both pragmas are connection state; they survive because the pool holds exactly
		// one connection. Raising SetMaxOpenConns requires moving this to a ConnectHook.
		for _, p := range []string{"PRAGMA fullfsync = 1", "PRAGMA checkpoint_fullfsync = 1"} {
			if _, err := sqldb.Exec(p); err != nil {
				sqldb.Close()
				return nil, fmt.Errorf("%s: %w", p, err)
			}
		}
	}
	db, err := open(sqldb, "sqlite")
	if err != nil {
		return nil, err
	}
	// The level a relaxed transaction restores to. Durability only ever lowers a write
	// beneath this, so an operator who asked for NORMAL keeps NORMAL everywhere.
	db.sqliteBaseSync = sync
	return db, nil
}

type sqliteConfig struct{ fullFsync bool }

// SQLiteOption configures OpenSQLite beyond the PRAGMA synchronous level.
type SQLiteOption func(*sqliteConfig)

// WithFullFsync issues F_FULLFSYNC instead of fsync(2) on Apple platforms, where plain
// fsync(2) returns before the drive flushes its write cache — so synchronous=FULL alone
// is not power-loss durable there. Costs ~4ms/commit on an M1 versus ~22us for the
// no-op, so a benchmark without it is measuring nothing. No effect off Darwin.
func WithFullFsync() SQLiteOption {
	return func(c *sqliteConfig) { c.fullFsync = true }
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
// the pool (idle = half; <= 0 defaults to 50). It is also the group-commit batch-width
// ceiling: only transactions in flight together can coalesce into one flush, so the pool
// bounds how many ever do. Size a worker fleet so workers*maxOpenConns stays under the
// server's max_connections.
func OpenPostgres(dsn string, maxOpenConns int, opts ...PostgresOption) (*DB, error) {
	var cfg pgConfig
	for _, o := range opts {
		o(&cfg)
	}

	// Probed on a throwaway connection before the pool is built, so a setting this role
	// cannot apply is one clear failure at startup rather than one per pooled connection
	// later. It only ever gets here because someone passed the flag: the default is off.
	if err := probeSessionSettings(dsn, cfg.sessionSettings()); err != nil {
		return nil, err
	}

	var sqldb *sql.DB
	if settings := cfg.sessionSettings(); len(settings) > 0 {
		// Per-session rather than postgresql.conf: the setting then applies to genroc's
		// own connections and taxes no other database on the server.
		c, err := pq.NewConnector(dsn)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		sqldb = sql.OpenDB(sessionConnector{Connector: c, settings: settings})
	} else {
		var err error
		if sqldb, err = sql.Open("postgres", dsn); err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
	}

	if maxOpenConns <= 0 {
		maxOpenConns = 50
	}
	sqldb.SetMaxOpenConns(maxOpenConns)
	sqldb.SetMaxIdleConns(max(maxOpenConns/2, 1))
	return open(sqldb, "postgres")
}

// probeSessionSettings reports whether this DSN's role may apply them.
func probeSessionSettings(dsn string, settings []string) error {
	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer probe.Close()
	for _, s := range settings {
		if _, err := probe.Exec(s); err != nil {
			return fmt.Errorf("%s: %w (a superuser-context setting: connect as a "+
				"superuser, or set it in postgresql.conf and drop the flag)", s, err)
		}
	}
	return nil
}

// PostgresOption configures OpenPostgres beyond the pool size.
type PostgresOption func(*pgConfig)

type pgConfig struct{ commitDelayUs int }

func (c pgConfig) sessionSettings() []string {
	if c.commitDelayUs <= 0 {
		return nil
	}
	return []string{fmt.Sprintf("SET commit_delay = %d", c.commitDelayUs)}
}

// WithCommitDelay holds each WAL flush back by us microseconds so more commits coalesce
// into it — throughput bought with latency, and no durability: every commit is still
// flushed before it is acknowledged. Postgres applies it only while at least
// commit_siblings (default 5) transactions are open, so it disables itself on the narrow,
// causally-sequential workloads it would otherwise slow down. Zero leaves it off.
// specs/durability-levels.md §6.
func WithCommitDelay(us int) PostgresOption {
	return func(c *pgConfig) { c.commitDelayUs = us }
}

// sessionConnector applies settings to each pooled connection as it opens. A failure here
// fails the connection rather than being swallowed: a performance setting the operator
// asked for and did not get is worth a startup error, not a silent no-op.
type sessionConnector struct {
	driver.Connector
	settings []string
}

func (c sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("postgres driver cannot apply session settings")
	}
	for _, s := range c.settings {
		if _, err := exec.ExecContext(ctx, s, nil); err != nil {
			conn.Close()
			// commit_delay is a superuser-context setting, which is the failure that
			// actually happens here.
			return nil, fmt.Errorf("%s: %w (a superuser-context setting: connect as a "+
				"superuser, or set it in postgresql.conf and drop the flag)", s, err)
		}
	}
	return conn, nil
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
	// Not the zero value. Durability reads two ways and they disagree at zero: for a
	// write's FLOOR it means "sync at every level" (safe), for the configured LEVEL it
	// means the weakest one (not). A DB nobody called SetDurability on must be strict.
	db.SetDurability(DurabilityStrict)
	db.sqliteBaseSync = "FULL"
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
