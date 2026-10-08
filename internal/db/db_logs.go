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

// LogQuery filters ListLogs/ListTreeLogs; the zero value is the first page of the newest logs.
type LogQuery struct {
	Level   string // a FLOOR: this level and everything above it (model.LogLevelsAtLeast)
	Created Window // on created_at, a trail's only sort
	Page    PageReq
}

// Log pagination: time order only — (created_at, seq, id), index-backed under instance_id and
// under root_id alike.
var logPaginator = paginator{
	table:      "process_logs pl",
	columns:    logColumns,
	filterCols: []string{"pl.instance_id", "pl.root_id", "pl.level", "pl.created_at"},
	sorts: map[string]sortMode{
		// seq orders two rows sharing a millisecond; id follows it to keep the cursor key
		// unique, and to order rows written before 042 (seq 0) by the UUIDs they carry.
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

// logBatchRows bounds one multi-row INSERT's bind count, and is the buffer size that triggers an
// inline flush, so a burst cannot grow the buffer unbounded.
const (
	logFlushInterval = 5 * time.Millisecond
	logBatchRows     = 90
)

// AppendLog stamps and buffers one audit row. Best-effort by contract: a failure must never abort
// an advance, and a buffered row may be lost on crash (migration 008). Stamped here, not at flush,
// so the (created_at, seq, id) sort keeps insertion order.
func (db *DB) AppendLog(entry *model.LogEntry) error {
	params, err := db.buildLogParams(entry)
	if err != nil {
		return err
	}
	db.logMu.Lock()
	first := len(db.logBuf) == 0
	db.logBuf = append(db.logBuf, params)
	full := len(db.logBuf) >= logBatchRows
	db.logMu.Unlock()
	if full {
		return db.flushLogs()
	}
	// Only the empty-to-non-empty append wakes the flusher; any later row rides that wake.
	if first {
		select {
		case db.logWake <- struct{}{}:
		default:
		}
	}
	return nil
}

// AppendLogValue stores an audit row whose payload is a VALUE, cut like any other. A row with
// objects is written synchronously, row and claims in ONE transaction: a buffered row would leave
// a claim with no owner yet, which the sweep retires.
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

// decodeRefs reads a malformed list as nothing rather than failing the read: the payload is still
// there, and an audit row is best-effort.
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
// A blank id gets a fresh one; a zero CreatedAt gets the DB clock.
func (db *DB) buildLogParams(entry *model.LogEntry) (dbgen.InsertLogParams, error) {
	// created_at is millisecond-granular, so seq is what orders two rows from one advance.
	id, seq := db.nextLogID()
	if entry.ID != "" {
		id = entry.ID
	}
	createdAt := nowMillis()
	if !entry.CreatedAt.IsZero() {
		createdAt = entry.CreatedAt.UnixMilli()
	}
	// meta is small and structured, so it is marshalled here; data arrives already
	// marshalled (AppendLogValue cut it) and is stored verbatim.
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

// logFlusher drops errors: a transient failure costs at most that batch, the loss the schema
// tolerates. It sleeps until a wake, so an idle server pays no timer.
func (db *DB) logFlusher() {
	defer close(db.logStopped)
	for {
		select {
		case <-db.logStop:
			_ = db.flushLogs()
			return
		case <-db.logWake:
		}
		// The window opens at the first row, so an advance's rows share one INSERT.
		select {
		case <-db.logStop:
			_ = db.flushLogs()
			return
		case <-time.After(logFlushInterval):
		}
		_ = db.flushLogs()
	}
}

// flushLogs is safe from any goroutine: each row is detached, so written, exactly once, and
// logFlushMu holds a reader until a concurrent batch has landed.
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

// writeLogBatch runs at syncStrict, not the always-sync default: the trail already drops buffered
// rows on crash, so flushing each 5ms batch buys nothing and is the largest fsync source left.
func (db *DB) writeLogBatch(rows []dbgen.InsertLogParams) error {
	ctx := context.Background()
	// One transaction rather than one per chunk, so a batch is also all-or-nothing.
	return db.withTxAt(ctx, syncStrict, func(_ *dbgen.Queries, exec dbgen.DBTX) error {
		for start := 0; start < len(rows); start += logBatchRows {
			end := min(start+logBatchRows, len(rows))
			chunk := rows[start:end]
			var sb strings.Builder
			// The SECOND spelling of a log column (InsertLog in queries.sql is the other): a
			// column added only there is dropped on this, the common path.
			sb.WriteString(`INSERT INTO process_logs (id, instance_id, root_id, seq, level, event, task_id, message, code, data, objects, meta, created_at, actor) VALUES `)
			args := make([]any, 0, len(chunk)*15)
			for i, r := range chunk {
				if i > 0 {
					sb.WriteByte(',')
				}
				// root_id is derived off the instance exactly as InsertLog does, or this path
				// writes rows no tree read can find.
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

// levelFloor: an unknown level matches only itself, i.e. nothing -- the API refuses it first,
// and inventing a floor for it would WIDEN the read.
func levelFloor(min string) []any {
	levels := model.LogLevelsAtLeast(model.LogLevel(min))
	if len(levels) == 0 {
		return []any{min}
	}
	out := make([]any, len(levels))
	for i, l := range levels {
		out[i] = string(l)
	}
	return out
}

// logColumns is the pl.-qualified SELECT list shared by both log queries, which differ only
// in the column they filter on.
const logColumns = `pl.id, pl.instance_id, pl.level, pl.event, pl.task_id, pl.message, pl.code, pl.data, pl.objects, pl.meta, pl.created_at, pl.actor, pl.seq`

func (db *DB) ListLogs(instanceID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	db.flushLogs() // make any buffered rows for this instance visible to the read
	q := logPaginator.query(opts.Page).
		Eq("pl.instance_id", instanceID).
		InIf("pl.level", levelFloor(opts.Level), opts.Level != "")
	b, err := opts.Created.apply(q, "pl.created_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, scanLogRow, logCursorVals)
}

// LogsFor returns the whole TREE's trail when id names a root, that instance's own rows otherwise;
// flat asks a root for its own rows alone. An id whose instance is gone reads as not-a-root.
func (db *DB) LogsFor(id string, flat bool, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	if !flat {
		if root, err := db.q.GetInstanceRoot(context.Background(), id); err == nil && root == id {
			return db.ListTreeLogs(id, opts)
		}
	}
	return db.ListLogs(id, opts)
}

// ListTreeLogs returns a page of every log in the tree rooted at rootID; root_id is stored on
// the row (migration 040), so this walks nothing.
func (db *DB) ListTreeLogs(rootID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	db.flushLogs() // make any buffered rows for the tree visible to the read
	q := logPaginator.query(opts.Page).
		Eq("pl.root_id", rootID).
		InIf("pl.level", levelFloor(opts.Level), opts.Level != "")
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
