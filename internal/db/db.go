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
	"genroc/internal/idgen"
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

	// One counter per KIND of row; ids from two streams can be equal.
	ids struct{ instances, logs, signals, tokens *idgen.Minter }

	// flushes counts successful Flush calls. Not read from durability_marker: that row moves
	// only on SQLite, so a test built on it asserts nothing on Postgres.
	flushes atomic.Int64
	// durability is the ladder level writes are measured against (specs/durability-levels.md §5).
	durability atomic.Int64
	// sqliteBaseSync is the operator's --sqlite-synchronous, restored by a relaxed transaction on
	// hand-back: durability lowers writes beneath it, never above.
	sqliteBaseSync string

	// defCache holds raw JSON, re-unmarshalled per call so callers never share Task pointers;
	// SaveDefinition must invalidate it (ON CONFLICT overwrites).
	defCache sync.Map // defKey → string

	// Logs are best-effort (migration 008): AppendLog buffers, logFlusher batch-inserts, and every
	// read/prune flushes first so appends stay visible. A crash drops only buffered rows.
	logMu sync.Mutex // guards logBuf only; never held across the insert
	// logFlushMu spans a flush's detach *and* its insert, so a reader flushing mid-flush waits
	// for that batch instead of finding the buffer empty and querying without it.
	logFlushMu sync.Mutex
	logBuf     []dbgen.InsertLogParams
	logStop    chan struct{} // closed by Close() to stop the flusher
	logStopped chan struct{} // closed by the flusher after its final flush

	// objectGraceMs: how long a RELEASED object stays fetchable, so a reference already handed
	// out still resolves after the data moved on. specs/object-store.md.
	objectGraceMs atomic.Int64
}

type defKey struct {
	name    string
	version int
}

// OpenSQLite opens (or creates) the SQLite database at path and runs migrations. synchronous is
// OFF, NORMAL (empty; fsyncs the WAL only at checkpoints), FULL (per commit, like Postgres) or
// EXTRA. On macOS, FULL means what it says only with WithFullFsync.
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
	db.sqliteBaseSync = sync
	return db, nil
}

type sqliteConfig struct{ fullFsync bool }

// SQLiteOption configures OpenSQLite beyond the PRAGMA synchronous level.
type SQLiteOption func(*sqliteConfig)

// WithFullFsync issues F_FULLFSYNC instead of fsync(2) on Apple platforms, where fsync returns
// before the drive flushes its cache, so synchronous=FULL alone is not power-loss durable.
// Costs ~4ms/commit on an M1 (versus ~22us); no effect off Darwin.
func WithFullFsync() SQLiteOption {
	return func(c *sqliteConfig) { c.fullFsync = true }
}

// sqliteSynchronous whitelists the level placed on the DSN, so a flag value can never inject
// extra connection parameters.
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

// OpenPostgres opens a PostgreSQL connection and runs migrations. maxOpenConns caps the pool
// (idle = half; <= 0 means 50) and with it the group-commit batch width; keep
// workers*maxOpenConns under the server's max_connections.
func OpenPostgres(dsn string, maxOpenConns int, opts ...PostgresOption) (*DB, error) {
	var cfg pgConfig
	for _, o := range opts {
		o(&cfg)
	}

	// Probed before the pool is built, so a setting this role cannot apply fails once at
	// startup rather than once per pooled connection.
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

// WithCommitDelay holds each WAL flush back by us microseconds so more commits coalesce into it:
// throughput for latency, never durability. Postgres applies it only while commit_siblings
// transactions are open. Zero leaves it off. specs/durability-levels.md §6.
func WithCommitDelay(us int) PostgresOption {
	return func(c *pgConfig) { c.commitDelayUs = us }
}

// sessionConnector applies settings to each pooled connection as it opens and fails the
// connection on error: a setting the operator asked for must not silently no-op.
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
			// commit_delay is superuser-context: the failure that actually happens here.
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
	// Fails the open: a process that cannot be told which ids are its own must not write any.
	worker, err := db.q.NextWorkerNumber(context.Background())
	if err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("allocate worker number: %w", err)
	}
	minter, err := idgen.NewMinter(worker)
	if err != nil {
		sqldb.Close()
		return nil, err
	}
	db.ids.instances = minter
	db.ids.logs = minter.Stream()
	db.ids.signals = minter.Stream()
	db.ids.tokens = minter.Stream()
	// Not the zero value: as a write's FLOOR zero means "sync at every level", as the configured
	// LEVEL it means the weakest. A DB nobody called SetDurability on must be strict.
	db.SetDurability(DurabilityStrict)
	db.sqliteBaseSync = "FULL"
	go db.logFlusher()
	return db, nil
}

// pgBootstrapLockKey serializes bootstrapPostgres across workers; any value works if all share it.
const pgBootstrapLockKey int64 = 0x67656E74 // "gent"

// bootstrapPostgres must stay idempotent and under the advisory lock: both statements rewrite a
// catalog tuple, so concurrent worker starts race ("tuple concurrently updated").
func bootstrapPostgres(sqldb *sql.DB) error {
	ctx := context.Background()
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin postgres bootstrap: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgBootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx, pgFunctionsSQL); err != nil {
		return fmt.Errorf("create json_each function: %w", err)
	}

	// Dead tuples in idx_instances_runnable slow every claim until vacuumed. internal/db/CLAUDE.md.
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

// Ping verifies a connection is usable, acquiring one from the pool if none is idle. It backs
// the readiness check: a worker that cannot reach its database should not be routed to.
func (db *DB) Ping(ctx context.Context) error { return db.sqldb.PingContext(ctx) }

// NextID mints an instance id; each kind of row counts on its own stream (internal/idgen).
func (db *DB) NextID() string {
	id, _ := db.ids.instances.Next()
	return id
}

func (db *DB) nextTokenID() string {
	id, _ := db.ids.tokens.Next()
	return id
}

// These also return the counter: an id does not sort, so a row whose ORDER matters keeps it in
// a `seq` column (migration 042).
func (db *DB) nextLogID() (string, int64)    { return db.ids.logs.Next() }
func (db *DB) nextSignalID() (string, int64) { return db.ids.signals.Next() }

// Dialect reports the engine backing this DB: "sqlite" or "postgres".
func (db *DB) Dialect() string { return db.dialect }

// Close flushes buffered audit-log rows, stops the flusher, and closes the pool.
func (db *DB) Close() error {
	close(db.logStop)
	<-db.logStopped
	return db.sqldb.Close()
}

// pageInfo counts rows around a page bounded by first/last (display order; nil when empty). A
// cursor is set only in a direction with more rows: its presence is the has-more signal.
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

// clockOffset (ms) shifts "now" for every DB read and write. Only ever increased, by AdvanceClock
// (debug /tick), so tests can expire leases and timers without real waits.
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
