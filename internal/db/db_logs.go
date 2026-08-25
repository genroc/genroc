package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/numeric"
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
// rows. logBatchRows bounds a single multi-row INSERT: at 11 columns/row it stays
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

// AppendLogValue stores one audit row whose payload is a VALUE, cutting it like any other and
// claiming each externalized piece for the row itself.
//
// A row with objects is written synchronously, row and claims in ONE transaction, rather than
// through the buffer. The claim's owner is the row, so a buffered row would leave a claim whose
// owner does not exist yet -- and the sweep, which retires exactly those, would take it. Rows
// without objects (nearly all of them) keep the buffered path and its batching.
func (db *DB) AppendLogValue(entry *model.LogEntry, v any, target int64) error {
	if v == nil {
		return db.AppendLog(entry) // no payload, no envelope: the column stays empty
	}
	stripped, refs, objs, referenced, err := cutLogPayload(v, target)
	if err != nil {
		return err
	}
	b, err := json.Marshal(stripped)
	if err != nil {
		return err
	}
	entry.Data = string(b)
	entry.Objects = refs
	if len(referenced) == 0 {
		return db.AppendLog(entry)
	}
	params, err := buildLogParams(entry)
	if err != nil {
		return err
	}
	now := nowMillis()
	return db.withTx(context.Background(), func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		if err := claimObjects(context.Background(), qtx, model.ObjectOwnerLog, params.ID,
			objs, referenced, now); err != nil {
			return err
		}
		return qtx.InsertLog(context.Background(), params)
	})
}

// marshalRefs / decodeRefs move an owner's objects list between its column and the value. A
// malformed list decodes to nothing rather than failing the read: the payload is still there, and
// an audit row is best-effort by contract.
func marshalRefs(refs []*model.ObjectRef) string {
	if len(refs) == 0 {
		return ""
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeRefs(s string) []*model.ObjectRef {
	if s == "" {
		return nil
	}
	var refs []*model.ObjectRef
	if err := numeric.Decode([]byte(s), &refs); err != nil {
		return nil
	}
	return refs
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
		Data:       entry.Data, // the payload as a value, with its cut pieces removed
		Objects:    marshalRefs(entry.Objects),
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

// flushLogs writes every buffered row. Safe from any goroutine: the detach is done under
// the lock, so each buffered row is written exactly once. logFlushMu covers the detach and
// the insert together — a reader that flushed while a concurrent flush held a detached
// batch would otherwise see an empty buffer and query before those rows landed.
func (db *DB) flushLogs() error {
	db.logFlushMu.Lock()
	defer db.logFlushMu.Unlock()
	batch := db.detachLogs()
	if len(batch) == 0 {
		return nil
	}
	return db.writeLogBatch(batch)
}

// detachLogs takes the buffer for the caller to write. Held only for the swap, so an
// AppendLog concurrent with a flush never waits on the insert.
func (db *DB) detachLogs() []dbgen.InsertLogParams {
	db.logMu.Lock()
	defer db.logMu.Unlock()
	batch := db.logBuf
	db.logBuf = nil
	return batch
}

// writeLogBatch inserts rows in chunks of logBatchRows, one multi-row INSERT per chunk
// (one round-trip per chunk instead of per event).
//
// syncStrict, not the always-sync default: the trail is best-effort by contract already —
// a crash drops whatever was still buffered — so flushing each 5ms batch below `strict`
// would buy a durability the rest of the audit path does not offer. It was also the single
// largest remaining fsync source once instance writes were classified, because the flusher
// commits far more often than instances complete.
func (db *DB) writeLogBatch(rows []dbgen.InsertLogParams) error {
	ctx := context.Background()
	// One transaction rather than one per chunk, so a batch is also all-or-nothing.
	return db.withTxAt(ctx, syncStrict, func(_ *dbgen.Queries, exec dbgen.DBTX) error {
		for start := 0; start < len(rows); start += logBatchRows {
			end := min(start+logBatchRows, len(rows))
			chunk := rows[start:end]
			var sb strings.Builder
			sb.WriteString(`INSERT INTO process_logs (id, instance_id, level, event, task_id, message, code, data, objects, meta, created_at) VALUES `)
			args := make([]any, 0, len(chunk)*11)
			for i, r := range chunk {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?)")
				args = append(args, r.ID, r.InstanceID, r.Level, r.Event, r.TaskID, r.Message, r.Code, r.Data, r.Objects, r.Meta, r.CreatedAt)
			}
			if _, err := exec.ExecContext(ctx, sb.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// logColumns is the pl.-qualified SELECT list shared by both log queries (the
// flat query aliases process_logs pl; the subtree query joins it as pl).
const logColumns = `pl.id, pl.instance_id, pl.level, pl.event, pl.task_id, pl.message, pl.code, pl.data, pl.objects, pl.meta, pl.created_at`

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
	dest := []any{&r.ID, &r.InstanceID, &r.Level, &r.Event, &r.TaskID, &r.Message, &r.Code, &r.Data, &r.Objects, &r.Meta, &r.CreatedAt}
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
		Objects:    decodeRefs(r.Objects),
		CreatedAt:  toTime(r.CreatedAt),
	}
	if r.Meta != "" && r.Meta != "{}" {
		if err := json.Unmarshal([]byte(r.Meta), &e.Meta); err != nil {
			return nil, err
		}
	}
	return e, nil
}
