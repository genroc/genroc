package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/idgen"
	"genroc/internal/model"
)

// LogQuery holds the optional filters shared by ListLogs and ListTreeLogs plus
// the pagination request. The zero value (empty Level, zero Created, zero Page)
// returns the first page of the newest logs.
type LogQuery struct {
	Level   string
	Created Window // on created_at, a trail's only sort
	Page    PageReq
}

// Log pagination: time order only — (created_at, id) preserves insertion order (UUIDv7
// monotonic per ms) and is index-backed. Columns are pl.-qualified so build() serves the
// flat query; the subtree CTE supplies its own prefixes via buildSource.
var logPaginator = paginator{
	table:      "process_logs pl",
	columns:    logColumns,
	filterCols: []string{"pl.instance_id", "pl.level", "pl.created_at"},
	sorts: map[string]sortMode{
		"created": {{"pl.created_at", kindInt}, {"pl.id", kindText}},
	},
	defSort:  "created",
	defDesc:  true, // newest first, as every list endpoint defaults
	defLimit: 20,
	maxLimit: 100,
}

func logCursorVals(_ string, e *model.LogEntry) []any {
	return []any{e.CreatedAt.UnixMilli(), e.ID}
}

// logFlushInterval is how often the background flusher drains buffered audit-log
// rows. logBatchRows bounds a single multi-row INSERT: at 10 columns/row it stays
// under SQLite's default 999 bind-parameter limit, and is also the buffer size that
// triggers an immediate inline flush so a burst never grows the buffer unbounded.
const (
	logFlushInterval = 5 * time.Millisecond
	logBatchRows     = 90
)

// AppendLog stamps and buffers one audit-trail row. Best-effort by contract: a failure
// here must never abort an instance advance, and a buffered row may be lost on crash
// (migration 008 — an observability gap, never state corruption). The row is stamped
// here, not at flush time, so the (created_at, id) sort preserves insertion order; the
// write is batched off the hot path by logFlusher (or inline once it hits logBatchRows).
func (db *DB) AppendLog(entry *model.LogEntry) error {
	params, err := buildLogParams(entry)
	if err != nil {
		return err
	}
	db.logMu.Lock()
	db.logBuf = append(db.logBuf, params)
	full := len(db.logBuf) >= logBatchRows
	db.logMu.Unlock()
	if full {
		return db.flushLogs()
	}
	return nil
}

// buildLogParams stamps an entry's id/created_at/meta into the process_logs row params.
// A blank id gets a fresh UUIDv7 (monotonic within a millisecond, so the (created_at,
// id) sort preserves insertion order for co-millisecond events); a zero CreatedAt gets
// the DB clock.
func buildLogParams(entry *model.LogEntry) (dbgen.InsertLogParams, error) {
	id := entry.ID
	if id == "" {
		id = idgen.New()
	}
	createdAt := nowMillis()
	if !entry.CreatedAt.IsZero() {
		createdAt = entry.CreatedAt.UnixMilli()
	}
	// meta is structured (and small), so it is stored as JSON; data is the raw,
	// possibly-truncated body and is stored verbatim.
	meta := ""
	if len(entry.Meta) > 0 {
		b, err := json.Marshal(entry.Meta)
		if err != nil {
			return dbgen.InsertLogParams{}, err
		}
		meta = string(b)
	}
	return dbgen.InsertLogParams{
		ID:         id,
		InstanceID: entry.InstanceID,
		Level:      string(entry.Level),
		Event:      entry.Event,
		TaskID:     entry.TaskID,
		Message:    entry.Message,
		Code:       entry.Code,
		Data:       entry.Data, // raw payload snippet (input/output/request/response body), or ""
		Meta:       meta,
		CreatedAt:  createdAt,
	}, nil
}

// logFlusher drains the audit-log buffer every logFlushInterval until Close stops it,
// then flushes once more. Errors are dropped (best-effort): a transient DB error costs
// at most that batch, exactly the loss the schema tolerates.
func (db *DB) logFlusher() {
	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-db.logStop:
			_ = db.flushLogs()
			close(db.logStopped)
			return
		case <-ticker.C:
			_ = db.flushLogs()
		}
	}
}

// flushLogs detaches the current buffer and writes it. Safe from any goroutine: the
// swap is done under the lock, so each buffered row is written exactly once.
func (db *DB) flushLogs() error {
	db.logMu.Lock()
	if len(db.logBuf) == 0 {
		db.logMu.Unlock()
		return nil
	}
	batch := db.logBuf
	db.logBuf = nil
	db.logMu.Unlock()
	return db.writeLogBatch(batch)
}

// writeLogBatch inserts rows in chunks of logBatchRows, one multi-row INSERT per chunk
// (one round-trip per chunk instead of per event). Runs through db.exec, so ? is
// rewritten to $N on Postgres.
func (db *DB) writeLogBatch(rows []dbgen.InsertLogParams) error {
	for start := 0; start < len(rows); start += logBatchRows {
		end := min(start+logBatchRows, len(rows))
		chunk := rows[start:end]
		var sb strings.Builder
		sb.WriteString(`INSERT INTO process_logs (id, instance_id, level, event, task_id, message, code, data, meta, created_at) VALUES `)
		args := make([]any, 0, len(chunk)*10)
		for i, r := range chunk {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?,?,?,?,?,?)")
			args = append(args, r.ID, r.InstanceID, r.Level, r.Event, r.TaskID, r.Message, r.Code, r.Data, r.Meta, r.CreatedAt)
		}
		if _, err := db.exec.ExecContext(context.Background(), sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// logColumns is the pl.-qualified SELECT list shared by both log queries (the
// flat query aliases process_logs pl; the subtree query joins it as pl).
const logColumns = `pl.id, pl.instance_id, pl.level, pl.event, pl.task_id, pl.message, pl.code, pl.data, pl.meta, pl.created_at`

// logSubtreeCTE walks parent_id from a seed, tagging depth. Hand-written (sqlc's SQLite
// grammar can't parse WITH RECURSIVE; both drivers support it). treeLogsPrefix is the
// page SELECT; treeLogsCountInner the count's inner row source.
const logSubtreeCTE = `
WITH RECURSIVE subtree(id, depth) AS (
    SELECT id, 0 FROM process_instances WHERE id = ?
    UNION ALL
    SELECT pi.id, s.depth + 1 FROM process_instances pi JOIN subtree s ON pi.parent_id = s.id
)`

const treeLogsJoin = `
FROM process_logs pl
JOIN subtree st ON st.id = pl.instance_id`

const treeLogsPrefix = logSubtreeCTE + `
SELECT ` + logColumns + `, st.depth` + treeLogsJoin

const treeLogsCountInner = logSubtreeCTE + `
SELECT 1` + treeLogsJoin

func (db *DB) ListLogs(instanceID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	db.flushLogs() // make any buffered rows for this instance visible to the read
	q := logPaginator.query(opts.Page).
		Eq("pl.instance_id", instanceID).
		EqIf("pl.level", opts.Level, opts.Level != "")
	b, err := opts.Created.apply(q, "pl.created_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, func(s rowScanner) (*model.LogEntry, error) {
		return scanLogRow(s, false)
	}, logCursorVals)
}

// ListTreeLogs returns a page of every log in the subtree rooted at rootID (any node,
// itself + all descendants); each entry's Depth is its distance from rootID (0 at the
// root). The CTE prefixes are trusted constants; filters/cursor/ORDER BY come from the
// shared paginator via buildSource.
func (db *DB) ListTreeLogs(rootID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	db.flushLogs() // make any buffered rows for the subtree visible to the read
	q := logPaginator.query(opts.Page).
		EqIf("pl.level", opts.Level, opts.Level != "")
	b, err := opts.Created.apply(q, "pl.created_at").
		buildSource(treeLogsPrefix, treeLogsCountInner, []any{rootID})
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, func(s rowScanner) (*model.LogEntry, error) {
		return scanLogRow(s, true)
	}, logCursorVals)
}

// scanLogRow scans one log row. When withDepth, the row carries a trailing
// st.depth column (the subtree query); otherwise it is the flat column list.
func scanLogRow(s rowScanner, withDepth bool) (*model.LogEntry, error) {
	var r dbgen.ProcessLog
	var depth int64
	dest := []any{&r.ID, &r.InstanceID, &r.Level, &r.Event, &r.TaskID, &r.Message, &r.Code, &r.Data, &r.Meta, &r.CreatedAt}
	if withDepth {
		dest = append(dest, &depth)
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}
	e, err := toLogEntry(r)
	if err != nil {
		return nil, err
	}
	e.Depth = int(depth)
	return e, nil
}

// PruneLogs deletes every log older than before (unix millis), returning the count.
// Buffered rows are flushed first so an already-old row can't linger past a prune.
func (db *DB) PruneLogs(before int64) (int64, error) {
	db.flushLogs()
	return db.q.DeleteLogsBefore(context.Background(), before)
}

func toLogEntry(r dbgen.ProcessLog) (*model.LogEntry, error) {
	e := &model.LogEntry{
		ID:         r.ID,
		InstanceID: r.InstanceID,
		Level:      model.LogLevel(r.Level),
		Event:      r.Event,
		TaskID:     r.TaskID,
		Message:    r.Message,
		Code:       r.Code,
		Data:       r.Data,
		CreatedAt:  toTime(r.CreatedAt),
	}
	if r.Meta != "" && r.Meta != "{}" {
		if err := json.Unmarshal([]byte(r.Meta), &e.Meta); err != nil {
			return nil, err
		}
	}
	return e, nil
}
