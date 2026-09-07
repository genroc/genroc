package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	dbgen "genroc/internal/db/gen"
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
// monotonic per ms) and is index-backed, under instance_id and under root_id alike.
var logPaginator = paginator{
	table:      "process_logs pl",
	columns:    logColumns,
	filterCols: []string{"pl.instance_id", "pl.root_id", "pl.level", "pl.created_at"},
	sorts: map[string]sortMode{
		// seq orders two rows sharing a millisecond; pl.id follows it because the keyset
		// cursor needs a UNIQUE key, and because rows written before migration 042 carry
		// seq 0 and fall through to the sortable ids they were ordered by then.
		"created": {{"pl.created_at", kindInt}, {"pl.seq", kindInt}, {"pl.id", kindText}},
	},
	defSort:  "created",
	defDesc:  true, // newest first, as every list endpoint defaults
	defLimit: 20,
	maxLimit: 100,
}

func logCursorVals(_ string, e *model.LogEntry) []any {
	return []any{e.CreatedAt.UnixMilli(), int64(e.Seq), e.ID}
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
	params, err := db.buildLogParams(entry)
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
	params, err := db.buildLogParams(entry)
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
// A blank id gets a fresh one -- minted ids rise within a process, so the (created_at, id)
// sort preserves insertion order for co-millisecond events; a zero CreatedAt gets the DB clock.
func (db *DB) buildLogParams(entry *model.LogEntry) (dbgen.InsertLogParams, error) {
	// The counter behind the id, stored beside it: created_at is millisecond-granular, so it
	// is what orders two rows written in one advance (migration 042).
	id, seq := db.nextIDSeq()
	if entry.ID != "" {
		id = entry.ID
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
		Seq:        seq,
		InstanceID: entry.InstanceID,
		Level:      string(entry.Level),
		Event:      entry.Event,
		TaskID:     entry.TaskID,
		Message:    entry.Message,
		Code:       entry.Code,
		Data:       entry.Data, // the payload as a value, with its cut pieces removed
		Actor:      entry.Actor,
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
			// This column list is the SECOND place a log column is spelled -- InsertLog in
			// queries.sql is the other, used by AppendLogValue for rows carrying objects. A
			// column added to one and not the other is written on the rare path and dropped
			// on the common one, which reads as the feature not working at all.
			sb.WriteString(`INSERT INTO process_logs (id, instance_id, root_id, seq, level, event, task_id, message, code, data, objects, meta, created_at, actor) VALUES `)
			args := make([]any, 0, len(chunk)*15)
			for i, r := range chunk {
				if i > 0 {
					sb.WriteByte(',')
				}
				// root_id is read off the instance, exactly as InsertLog does it -- the
				// derivation is part of the column, so both spellings carry it or one path
				// writes rows that no tree read can find.
				sb.WriteString("(?,?," +
					"COALESCE((SELECT p.root_id FROM process_instances p WHERE p.id = ?), ?)," +
					"?,?,?,?,?,?,?,?,?,?,?)")
				args = append(args, r.ID, r.InstanceID, r.InstanceID, r.InstanceID, r.Seq,
					r.Level, r.Event, r.TaskID, r.Message, r.Code, r.Data, r.Objects, r.Meta, r.CreatedAt, r.Actor)
			}
			if _, err := exec.ExecContext(ctx, sb.String(), args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// logColumns is the pl.-qualified SELECT list shared by both log queries, which differ only
// in the column they filter on.
const logColumns = `pl.id, pl.instance_id, pl.level, pl.event, pl.task_id, pl.message, pl.code, pl.data, pl.objects, pl.meta, pl.created_at, pl.actor, pl.seq`

func (db *DB) ListLogs(instanceID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	db.flushLogs() // make any buffered rows for this instance visible to the read
	q := logPaginator.query(opts.Page).
		Eq("pl.instance_id", instanceID).
		EqIf("pl.level", opts.Level, opts.Level != "")
	b, err := opts.Created.apply(q, "pl.created_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, scanLogRow, logCursorVals)
}

// LogsFor answers a trail for one id: the whole TREE when the id names a root, that instance's
// own rows otherwise. A tree is addressed by its ROOT here as it is everywhere else (requireRoot
// gates pause/resume/retry/upgrade the same way), so a child id is a question about that child --
// there is no walk left to answer a subtree hanging off one, which is the cost this removed.
// flat asks a root for its own rows alone.
//
// An id whose instance is gone reads as not-a-root: its rows are still addressable by their own
// instance_id, which is the most that can be said about them.
func (db *DB) LogsFor(id string, flat bool, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	if !flat {
		if root, err := db.q.GetInstanceRoot(context.Background(), id); err == nil && root == id {
			return db.ListTreeLogs(id, opts)
		}
	}
	return db.ListLogs(id, opts)
}

// ListTreeLogs returns a page of every log written anywhere in the tree rooted at rootID.
// Identical to ListLogs but for the column it filters on: the tree is a stored fact on the
// row (migration 040), so this walks nothing and pages at the cost of the page.
func (db *DB) ListTreeLogs(rootID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	db.flushLogs() // make any buffered rows for the tree visible to the read
	q := logPaginator.query(opts.Page).
		Eq("pl.root_id", rootID).
		EqIf("pl.level", opts.Level, opts.Level != "")
	b, err := opts.Created.apply(q, "pl.created_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, scanLogRow, logCursorVals)
}

func scanLogRow(s rowScanner) (*model.LogEntry, error) {
	var r dbgen.ProcessLog
	if err := s.Scan(&r.ID, &r.InstanceID, &r.Level, &r.Event, &r.TaskID, &r.Message,
		&r.Code, &r.Data, &r.Objects, &r.Meta, &r.CreatedAt, &r.Actor, &r.Seq); err != nil {
		return nil, err
	}
	return toLogEntry(r)
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
		Seq:        r.Seq,
		Level:      model.LogLevel(r.Level),
		Event:      r.Event,
		TaskID:     r.TaskID,
		Message:    r.Message,
		Code:       r.Code,
		Actor:      r.Actor,
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
