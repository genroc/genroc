package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"genroc/internal/numeric"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// Pagination for ListInstances over the summary columns (never the context blob). Sorts:
// created (immutable, so cursor walks stay stable under engine writes; the default) and
// updated (the CLI's recent-activity view). A new sort key needs a matching index.
var instancePaginator = paginator{
	table:   "process_instances",
	columns: instanceSummaryColumns,
	sorts: map[string]sortMode{
		"created": {{"created_at", kindInt}, {"id", kindText}},
		"updated": {{"updated_at", kindInt}, {"id", kindText}},
	},
	filterCols: []string{"status", "error_code", "process_name", "process_version", "parent_id", "created_at", "updated_at"},
	defSort:    "created",
	defDesc:    true, // newest first
	defLimit:   20,
	maxLimit:   100,
}

// instanceCursorVals returns inst's key-column values for the active sort, matching
// externalPaginator's column order (the external-task queue keys on updated_at).
func instanceCursorVals(sort string, inst *model.ProcessInstance) []any {
	switch sort {
	case "updated": // external-task queue
		return []any{inst.UpdatedAt.UnixMilli(), inst.ID}
	default: // created
		return []any{inst.CreatedAt.UnixMilli(), inst.ID}
	}
}

// instanceSummaryCursorVals is instanceCursorVals for the summary list path
// (instancePaginator's created/updated sorts).
func instanceSummaryCursorVals(sort string, s *model.InstanceSummary) []any {
	switch sort {
	case "created":
		return []any{s.CreatedAt.UnixMilli(), s.ID}
	default: // updated (default)
		return []any{s.UpdatedAt.UnixMilli(), s.ID}
	}
}

// instanceColumns is the full process_instances column list, in the order
// scanInstance reads them. Shared by the hand-written ClaimInstances and
// RetryProcess queries so adding a column touches one place.
const instanceColumns = `id, process_name, process_version, parent_id,
	call_stack, retry_count, wake_at, status, error_message,
	created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
	input_data, outputs_data, output_data, error_internal, external_data, engine_state, task,
	error_code, lease_epoch, task_epoch, parent_task_epoch,
	external_worker_id, external_lease_expires_at, external_claim_epoch, objects,
	next_replayable, error_data`

// Lightweight ListInstances projection — no context/call-stack blobs; order matches
// scanInstanceSummary. error_code stays despite the rule: short, and it is what a list
// is scanned for when something has gone wrong.
const instanceSummaryColumns = `id, parent_id, process_name, process_version, retry_count,
	status, wait_state, task, error_message, error_code, created_at, updated_at`

// Column order must match instanceSummaryColumns.
func scanInstanceSummary(s interface{ Scan(...any) error }) (*model.InstanceSummary, error) {
	var (
		r                          model.InstanceSummary
		processVersion, retryCount int64
		status, waitState          string
		createdAt, updatedAt       int64
	)
	if err := s.Scan(
		&r.ID, &r.ParentID, &r.ProcessName, &processVersion, &retryCount,
		&status, &waitState, &r.Task, &r.Error, &r.ErrorCode, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	r.ProcessVersion = int(processVersion)
	r.RetryCount = int(retryCount)
	r.Status = model.Status(status)
	r.WaitState = model.WaitState(waitState)
	r.CreatedAt = toTime(createdAt)
	r.UpdatedAt = toTime(updatedAt)
	return &r, nil
}

// Column order must match instanceColumns.
func scanInstance(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, error) {
	var r dbgen.ProcessInstance
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.ErrorMessage,
		&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
		&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorInternal, &r.ExternalData, &r.EngineState, &r.Task,
		&r.ErrorCode, &r.LeaseEpoch, &r.TaskEpoch, &r.ParentTaskEpoch,
		&r.ExternalWorkerID, &r.ExternalLeaseExpiresAt, &r.ExternalClaimEpoch, &r.Objects,
		&r.NextReplayable, &r.ErrorData,
	)
	return r, err
}

// stateCols holds the decomposed state columns as serialized JSON, ready to drop into an
// Insert/Update params struct. The value columns hold values; Objects lists every externalized
// piece with the path it was cut from, rooted at the CONTEXT -- one place to read what this
// instance references, in the shape the API puts on the wire. specs/object-store.md.
type stateCols struct {
	InputData, OutputsData, OutputData, ErrorInternal, ErrorData, ExternalData, EngineState, Objects string
}

// outputsColumn is the on-disk shape of outputs_data: the completion order plus the
// per-task output envelopes, each independently inline-or-externalized.
type outputsColumn struct {
	Items map[string]json.RawMessage `json:"items,omitempty"`
}

// CurrentTask resolves an instance's current task object from its (immutable,
// version-pinned) definition, or nil when there is no current task (Task == "";
// completed or drained). Successors are implied by task order, so no queue is materialised.
func (db *DB) CurrentTask(inst *model.ProcessInstance) (*model.Task, error) {
	if inst.Task == "" {
		return nil, nil
	}
	def, err := db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, err
	}
	for _, t := range def.Tasks {
		if t.ID == inst.Task {
			return t, nil
		}
	}
	return nil, fmt.Errorf("task %q not found in %s v%d", inst.Task, inst.ProcessName, inst.ProcessVersion)
}

// joinPath roots a slot-relative path at the context. On a fresh slice: root is reused across
// every ref of a slot, and append would alias it into the second one.
func joinPath(root []any, rest []any) []any {
	out := make([]any, 0, len(root)+len(rest))
	out = append(out, root...)
	return append(out, rest...)
}

// encodeState splits inst.State into the value columns plus ONE objects list, collecting
// the content to write (pending) and the hashes the context still references (referenced) so the
// write transaction can claim new objects and release dropped ones.
//
// Each context key is cut as its own slot against its own budget, and every reference the cut
// produced goes into the same list with its path rooted at the CONTEXT -- ["outputs","x","code"],
// not ["code"] beside a column. One place to read what this instance references, and the shape
// the API already puts on the wire. specs/object-store.md.
func encodeState(inst *model.ProcessInstance) (cols stateCols, pending []*pendingObject, referenced map[string]struct{}, err error) {
	referenced = map[string]struct{}{}
	var refs []*model.ObjectRef
	cd := inst.State

	// cut returns the stripped value as JSON and files its references under root.
	cut := func(v any, root ...any) (string, error) {
		stripped, slotRefs, objs, e := cutSlot(v)
		if e != nil {
			return "", e
		}
		pending = append(pending, objs...)
		for _, r := range slotRefs {
			// EVERY ref: a hash missing from referenced is a claim the write releases while the
			// context still points at it.
			referenced[r.Ref] = struct{}{}
			refs = append(refs, &model.ObjectRef{Ref: r.Ref, Size: r.Size, Path: joinPath(root, r.Path)})
		}
		b, e := json.Marshal(stripped)
		return string(b), e
	}

	if v, ok := cd["input"]; ok {
		if cols.InputData, err = cut(v, "input"); err != nil {
			return
		}
	}
	if outs, ok := cd["outputs"].(map[string]any); ok {
		oc := outputsColumn{Items: map[string]json.RawMessage{}}
		for k, v := range outs {
			b, e := cut(v, "outputs", k)
			if e != nil {
				err = e
				return
			}
			oc.Items[k] = json.RawMessage(b)
		}
		b, e := json.Marshal(oc)
		if e != nil {
			err = e
			return
		}
		cols.OutputsData = string(b)
	}
	if v, ok := cd["output"]; ok {
		if cols.OutputData, err = cut(v, "output"); err != nil {
			return
		}
	}
	if v, ok := cd["error"]; ok {
		// An ordinary slot. `error.code` staying cheap to read is the ACCESSOR's job now
		// (model.Context walks to the path and loads nothing else), not the column's.
		if cols.ErrorInternal, err = cut(v, "error"); err != nil {
			return
		}
	}
	if v, ok := cd[model.StateErrorData]; ok {
		if cols.ErrorData, err = cut(v, model.StateErrorData); err != nil {
			return
		}
	}
	if cols.ExternalData, err = encodeExternalData(cd, cut); err != nil {
		return
	}
	if cols.EngineState, err = encodeEngineState(cd); err != nil {
		return
	}
	if len(refs) > 0 {
		b, e := json.Marshal(refs)
		if e != nil {
			err = e
			return
		}
		cols.Objects = string(b)
	}
	return
}

// encodeExternalData serialises the parked external-task bookkeeping (task_id, input snapshot)
// into external_data, or "" when none is present. Its references are rooted at _external, which
// is where the decode puts the map back -- an outcome never lands here, so the column holds one
// context key and its paths address the context like every other slot's.
// specs/external-outcome-as-signal.md.
func encodeExternalData(cd map[string]any, cut func(any, ...any) (string, error)) (string, error) {
	ext := map[string]any{}
	if e, ok := cd[model.StateExternal].(map[string]any); ok {
		for k, v := range e {
			ext[k] = v
		}
	}
	if len(ext) == 0 {
		return "", nil
	}
	return cut(ext, model.StateExternal)
}

// withExternalLost sets only the lost marker, with no has_<slot> companion: unlike the two
// outcome slots it carries no value to distinguish from absence, so a second key would be
// stored state that says nothing.
func withExternalLost(externalData string) (string, error) {
	return withExternalKeys(externalData, map[string]any{model.StateExternalLost: true})
}

// withExternalKeys sets keys in the external_data column without decoding the context around it.
// The instance's references live in their own column, which this does not write -- so a targeted
// outcome cannot disturb what the parked task's input still points at, and there is nothing to
// remember to carry along. It does not CUT either: this path holds only the instance row lock and
// has no reference set to reconcile, so an oversized outcome waits for the next full write.
func withExternalKeys(externalData string, keys map[string]any) (string, error) {
	ext := map[string]any{}
	if externalData != "" {
		if err := numeric.Decode([]byte(externalData), &ext); err != nil {
			return "", fmt.Errorf("decode external_data: %w", err)
		}
	}
	for k, v := range keys {
		ext[k] = v
	}
	b, err := json.Marshal(ext)
	return string(b), err
}

func withExternalSlot(externalData, slot string, v any) (string, error) {
	return withExternalKeys(externalData, map[string]any{slot: v, "has_" + slot: true})
}

// encodeEngineState serialises the spawn/children bookkeeping into engine_state.
// Returns "" when none is present.
func encodeEngineState(cd map[string]any) (string, error) {
	es := map[string]any{}
	for ctxKey, col := range engineStateKeys {
		if v, ok := cd[ctxKey]; ok {
			es[col] = v
		}
	}
	if len(es) == 0 {
		return "", nil
	}
	b, err := json.Marshal(es)
	return string(b), err
}

// engineStateKeys maps the engine-internal context keys to their engine_state field
// names (and back, in decodeState).
// It is a WHITELIST: a key missing from it is dropped on write with nothing reporting it,
// which reads at runtime as the value having never been set.
var engineStateKeys = map[string]string{
	"_spawn_child_key": "spawn_child_key",
	"_spawn_index":     "spawn_index",
	// How many times this slot has been re-spawned. Load-bearing for termination: read back
	// as zero, every retry round admits again and the batch never settles.
	// specs/child-error-handling.md s5.5.
	"_spawn_attempt": "spawn_attempt",
	// One-shot: the operator's `retry` grants the parent's raised slots one attempt past
	// their budget. It is a MARKER rather than a re-spawn done here, because the replacement's
	// input must be re-evaluated against the parent's current definition -- which is how an
	// upgraded fix reaches the child -- and this layer cannot evaluate expressions.
	// specs/child-error-handling.md s12.
	"_retry_override": "retry_override",
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// persistState encodes inst's state, writes/dereferences the implied objects via
// qtx (inside the caller's transaction), and returns the column strings for the
// caller's Insert/Update params.
func (db *DB) persistState(ctx context.Context, qtx *dbgen.Queries, inst *model.ProcessInstance, now int64) (stateCols, error) {
	cols, pending, referenced, err := encodeState(inst)
	if err != nil {
		return stateCols{}, err
	}
	if err := db.applyContextObjectDiff(ctx, qtx, inst.ID, pending, inst.LoadedObjectHashes, referenced, now); err != nil {
		return stateCols{}, err
	}
	// The consumed signal goes with the state it produced -- one transaction, so an answer is
	// never lost by a refused write nor applied twice by a refused delete.
	if inst.ConsumedSignalID != "" {
		if err := qtx.DeleteSignal(ctx, inst.ConsumedSignalID); err != nil {
			return stateCols{}, fmt.Errorf("consume signal %s: %w", inst.ConsumedSignalID, err)
		}
	}
	return cols, nil
}

func progressParams(inst *model.ProcessInstance, cols stateCols, now int64) dbgen.UpdateInstanceProgressParams {
	return dbgen.UpdateInstanceProgressParams{
		ID:             inst.ID,
		Task:           inst.Task,
		OutputsData:    cols.OutputsData,
		ErrorInternal:  cols.ErrorInternal,
		ExternalData:   cols.ExternalData,
		EngineState:    cols.EngineState,
		Objects:        cols.Objects,
		RetryCount:     int64(inst.RetryCount),
		WakeAt:         fromTimePtr(inst.WakeAt),
		WaitState:      string(inst.WaitState),
		UpdatedAt:      now,
		LeaseEpoch:     inst.LeaseEpoch,
		WorkerID:       fenceWorker(inst),
		NextReplayable: boolToInt(inst.NextReplayable),
		TaskEpoch:      inst.TaskEpoch,
	}
}

func updateInstanceParams(inst *model.ProcessInstance, cols stateCols, now int64) dbgen.UpdateInstanceParams {
	return dbgen.UpdateInstanceParams{
		ID:             inst.ID,
		Task:           inst.Task,
		OutputsData:    cols.OutputsData,
		OutputData:     cols.OutputData,
		ErrorInternal:  cols.ErrorInternal,
		ErrorData:      cols.ErrorData,
		ExternalData:   cols.ExternalData,
		EngineState:    cols.EngineState,
		Objects:        cols.Objects,
		RetryCount:     int64(inst.RetryCount),
		WakeAt:         fromTimePtr(inst.WakeAt),
		Status:         string(inst.Status),
		WaitState:      string(inst.WaitState),
		ErrorMessage:   inst.ErrorMessage,
		ErrorCode:      inst.ErrorCode,
		UpdatedAt:      now,
		LeaseEpoch:     inst.LeaseEpoch,
		WorkerID:       fenceWorker(inst),
		NextReplayable: boolToInt(inst.NextReplayable),
		TaskEpoch:      inst.TaskEpoch,
	}
}

// boolToInt stores a flag in the INTEGER column both engines share.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// fenceWorker is the worker half of the lease fence. The empty string stands for an
// unheld row, matching the COALESCE in the fenced UPDATEs, so a lease-less caller binding
// what it read under its row lock compares equal. specs/lease-fencing.md.
func fenceWorker(inst *model.ProcessInstance) string {
	if inst.WorkerID == nil {
		return ""
	}
	return *inst.WorkerID
}

// requireFenced converts a fenced write's rows-affected into the fence verdict: zero
// rows means the grant this write was made under is gone, and the caller's transaction
// rolls back with ErrLeaseLost.
func requireFenced(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// insertInstanceParams builds InsertInstance params from inst + already-encoded
// columns. status and the created/updated timestamps are passed explicitly so callers
// can override them (e.g. spawned children inherit the parent's status).
func insertInstanceParams(inst *model.ProcessInstance, cols stateCols, status string, createdAt, updatedAt int64) (dbgen.InsertInstanceParams, error) {
	callStack, err := json.Marshal(inst.CallStack)
	if err != nil {
		return dbgen.InsertInstanceParams{}, err
	}
	return dbgen.InsertInstanceParams{
		ID:              inst.ID,
		NextReplayable:  boolToInt(inst.NextReplayable),
		ProcessName:     inst.ProcessName,
		ProcessVersion:  int64(inst.ProcessVersion),
		Task:            inst.Task,
		InputData:       cols.InputData,
		OutputsData:     cols.OutputsData,
		OutputData:      cols.OutputData,
		ErrorInternal:   cols.ErrorInternal,
		ErrorData:       cols.ErrorData,
		ExternalData:    cols.ExternalData,
		EngineState:     cols.EngineState,
		Objects:         cols.Objects,
		ParentID:        inst.ParentID,
		SpawnTaskID:     inst.SpawnTaskID,
		ParentTaskEpoch: inst.ParentTaskEpoch,
		TaskEpoch:       inst.TaskEpoch,
		CallStack:       string(callStack),
		RetryCount:      int64(inst.RetryCount),
		WakeAt:          fromTimePtr(inst.WakeAt),
		Status:          status,
		WaitState:       string(inst.WaitState),
		ErrorMessage:    inst.ErrorMessage,
		ErrorCode:       inst.ErrorCode,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}, nil
}

func (db *DB) SaveInstance(inst *model.ProcessInstance) error {
	ctx := context.Background()
	now := nowMillis()
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		cols, err := db.persistState(ctx, qtx, inst, now)
		if err != nil {
			return err
		}
		params, err := insertInstanceParams(inst, cols, string(inst.Status), now, now)
		if err != nil {
			return err
		}
		return qtx.InsertInstance(ctx, params)
	})
}

func (db *DB) UpdateInstance(inst *model.ProcessInstance) error {
	ctx := context.Background()
	now := nowMillis()
	return db.withTxAt(ctx, instanceWriteFloor(inst.Status), func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		cols, err := db.persistState(ctx, qtx, inst, now)
		if err != nil {
			return err
		}
		return requireFenced(qtx.UpdateInstance(ctx, updateInstanceParams(inst, cols, now)))
	})
}

// UpdateInstanceProgress writes the mutable task state (context, retry counters,
// wait_state) without overwriting status or error, so a concurrent FailAncestors result
// survives to the next tick. The one status transition it does make is landing a pending
// pause ('pausing' → 'paused'), which has to happen on this write: a checkpoint may park
// the instance on a wait_state that removes it from the claim predicate, after which no
// later claim could settle it. wait_state IS written: it is owned by the lease-holding
// worker, and the post-collect reset to ” must persist or a stale 'collecting' would make
// the next spawn task skip phase 1.
func (db *DB) UpdateInstanceProgress(inst *model.ProcessInstance) error {
	ctx := context.Background()
	now := nowMillis()
	// A checkpoint means "still running", so this write is never the terminal one: it is
	// the ordinary mid-process write the ladder exists to stop flushing.
	return db.withTxAt(ctx, syncStrict, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		cols, err := db.persistState(ctx, qtx, inst, now)
		if err != nil {
			return err
		}
		return requireFenced(qtx.UpdateInstanceProgress(ctx, progressParams(inst, cols, now)))
	})
}

func (db *DB) GetInstance(id string) (*model.ProcessInstance, error) {
	r, err := db.q.GetInstance(context.Background(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("instance %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return toInstance(r)
}

// ListInstances returns a page of instance summaries, optionally filtered by status
// (empty = all), error code, process name, process version (0 = any), roots only, and a Window on
// either timestamp (zero =
// unbounded). The two windows are separate rather than one resolved against the active
// sort: a caller walking forward pairs its bound with the sort it ordered by, and naming
// the column keeps that pairing the caller's to state instead of this function's to guess.
// Summaries omit the context blob — use GetInstance for full detail.
// rootsOnly is the DEFAULT at every layer above this one: a tree is one unit of work, and
// a child_list fan-out otherwise buries the roots it belongs to. specs/id-list-commands.md.
func (db *DB) ListInstances(status, errorCode, process string, version int, rootsOnly bool, created, updated Window, req PageReq) ([]*model.InstanceSummary, PageInfo, error) {
	q := instancePaginator.query(req).
		EqIf("status", status, status != "").
		EqIf("error_code", errorCode, errorCode != "").
		EqIf("process_name", process, process != "").
		EqIf("process_version", version, version != 0).
		// parent_id is NOT NULL DEFAULT '' (migration 001), so a root is the empty string
		// rather than a null -- and the predicate can use the plain index on it.
		EqIf("parent_id", "", rootsOnly)
	b, err := updated.apply(created.apply(q, "created_at"), "updated_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return runPage(db, b, scanInstanceSummary, instanceSummaryCursorVals)
}

// queryInstancePage runs a built instance-listing query and returns the scanned page
// plus its PageInfo. Shared by the full-instance list paths (same columns and keys).
func (db *DB) queryInstancePage(b built) ([]*model.ProcessInstance, PageInfo, error) {
	return runPage(db, b, func(s rowScanner) (*model.ProcessInstance, error) {
		r, err := scanInstance(s)
		if err != nil {
			return nil, err
		}
		return toInstance(r)
	}, instanceCursorVals)
}

// ChildrenForTask returns ONE batch: the children spawned under parentTaskEpoch. The pair
// (parentID, spawnTaskID) repeats every time a loop re-enters the task, so the epoch is what
// separates this batch from the ones before it.
func (db *DB) ChildrenForTask(ctx context.Context, parentID, spawnTaskID string, parentTaskEpoch int64) ([]*model.ProcessInstance, error) {
	rows, err := db.q.GetChildrenForTask(ctx, dbgen.GetChildrenForTaskParams{
		ParentID:        parentID,
		SpawnTaskID:     spawnTaskID,
		ParentTaskEpoch: parentTaskEpoch,
	})
	if err != nil {
		return nil, fmt.Errorf("get children for task: %w", err)
	}
	out := make([]*model.ProcessInstance, len(rows))
	for i, r := range rows {
		inst, err := toInstance(r)
		if err != nil {
			return nil, err
		}
		out[i] = inst
	}
	return out, nil
}

// ── row → model conversion ────────────────────────────────────────────────────

func toInstance(r dbgen.ProcessInstance) (*model.ProcessInstance, error) {
	inst := &model.ProcessInstance{
		ID:              r.ID,
		ProcessName:     r.ProcessName,
		ProcessVersion:  int(r.ProcessVersion),
		Task:            r.Task,
		NextReplayable:  r.NextReplayable != 0,
		ParentID:        r.ParentID,
		SpawnTaskID:     r.SpawnTaskID,
		RetryCount:      int(r.RetryCount),
		Status:          model.Status(r.Status),
		WaitState:       model.WaitState(r.WaitState),
		ErrorMessage:    r.ErrorMessage,
		ErrorCode:       r.ErrorCode,
		CreatedAt:       toTime(r.CreatedAt),
		UpdatedAt:       toTime(r.UpdatedAt),
		WakeAt:          toTimePtr(r.WakeAt),
		WorkerID:        nullStringPtr(r.WorkerID),
		LeaseExpiresAt:  toTimePtr(r.LeaseExpiresAt),
		LeaseEpoch:      r.LeaseEpoch,
		TaskEpoch:       r.TaskEpoch,
		ParentTaskEpoch: r.ParentTaskEpoch,

		ExternalWorkerID:       nullStringPtr(r.ExternalWorkerID),
		ExternalLeaseExpiresAt: toTimePtr(r.ExternalLeaseExpiresAt),
		ExternalClaimEpoch:     r.ExternalClaimEpoch,
	}
	cd, loaded, err := decodeState(r)
	if err != nil {
		return nil, err
	}
	inst.State = cd
	inst.LoadedObjectHashes = loaded
	if err := json.Unmarshal([]byte(r.CallStack), &inst.CallStack); err != nil {
		return nil, fmt.Errorf("unmarshal call_stack: %w", err)
	}
	return inst, nil
}

// decodeState reassembles the six context columns into the in-memory State map.
// Externalized parts become *model.ObjectRef markers at the path they were cut from (resolved
// lazily through model.Context); loaded is the set of referenced hashes, which the next write
// diffs against to release the ones the value no longer points at.
func decodeState(r dbgen.ProcessInstance) (map[string]any, map[string]struct{}, error) {
	cd := map[string]any{}

	value := func(str, what string) (any, error) {
		if str == "" {
			return nil, nil
		}
		var v any
		if err := numeric.Decode([]byte(str), &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", what, err)
		}
		return v, nil
	}
	into := func(str, key string) error {
		if str == "" {
			return nil
		}
		v, err := value(str, key)
		if err != nil {
			return err
		}
		cd[key] = v
		return nil
	}

	if err := into(r.InputData, "input"); err != nil {
		return nil, nil, err
	}
	if err := into(r.OutputData, "output"); err != nil {
		return nil, nil, err
	}
	if err := into(r.ErrorInternal, "error"); err != nil {
		return nil, nil, err
	}
	if err := into(r.ErrorData, model.StateErrorData); err != nil {
		return nil, nil, err
	}
	if r.OutputsData != "" {
		var oc outputsColumn
		if err := numeric.Decode([]byte(r.OutputsData), &oc); err != nil {
			return nil, nil, fmt.Errorf("decode outputs_data: %w", err)
		}
		// The one wrapper that stays: each task output is cut against its own budget, and the
		// completion order rides along.
		items := make(map[string]any, len(oc.Items))
		for k, raw := range oc.Items {
			v, err := value(string(raw), "output "+k)
			if err != nil {
				return nil, nil, err
			}
			items[k] = v
		}
		cd["outputs"] = items
	}
	if r.ExternalData != "" {
		v, err := value(r.ExternalData, "external_data")
		if err != nil {
			return nil, nil, err
		}
		ext, _ := v.(map[string]any)
		if ext == nil {
			ext = map[string]any{}
		}
		cd[model.StateExternal] = ext
	}
	if r.EngineState != "" {
		var es map[string]any
		if err := numeric.Decode([]byte(r.EngineState), &es); err != nil {
			return nil, nil, fmt.Errorf("decode engine_state: %w", err)
		}
		for ctxKey, col := range engineStateKeys {
			if v, ok := es[col]; ok {
				cd[ctxKey] = v
			}
		}
	}

	// Every reference back where it was cut from, from the ONE list, before anything reads the
	// context. loaded is that list -- the next write diffs against it to release what the value
	// no longer points at, so a ref missing here is a claim nothing can ever drop.
	loaded := map[string]struct{}{}
	if r.Objects != "" {
		var refs []*model.ObjectRef
		if err := numeric.Decode([]byte(r.Objects), &refs); err != nil {
			return nil, nil, fmt.Errorf("decode objects: %w", err)
		}
		for _, ref := range refs {
			loaded[ref.Ref] = struct{}{}
			if len(ref.Path) == 1 {
				cd[ref.Path[0].(string)] = ref // a whole slot moved out
				continue
			}
			model.Place(cd, ref.Path, ref)
		}
	}

	return cd, loaded, nil
}

func childPathOf(at []any, key any) []any {
	out := make([]any, len(at)+1)
	copy(out, at)
	out[len(at)] = key
	return out
}

// ChildSpawn is one child as its parent's detail view names it: which task spawned it, and
// which slot of that task's batch it occupies.
type ChildSpawn struct {
	ID     string
	TaskID string
	Key    string // child_map only
	Index  int    // child_list only
	// Superseded marks a retired attempt at a slot (s5.5/s12). The row is still a child of
	// this parent, so it belongs in this list; what it is not is the slot's current occupant.
	Superseded bool
}

// ChildrenOfInstance rebuilds the spawn placeholder from the child rows. It is derived, not
// stored: the parent's own row would otherwise carry a copy of a relation the children already
// state, and a copy is a second source to keep in step. The discriminants live on the CHILD
// (engine_state), which is why they come back with it.
func (db *DB) ChildrenOfInstance(id string) ([]ChildSpawn, error) {
	rows, err := db.q.ChildrenOfInstance(context.Background(), id)
	if err != nil {
		return nil, err
	}
	out := make([]ChildSpawn, 0, len(rows))
	for _, r := range rows {
		c := ChildSpawn{ID: r.ID, TaskID: r.SpawnTaskID, Superseded: r.SupersededAt.Valid}
		if r.EngineState != "" {
			var es struct {
				SpawnChildKey string `json:"spawn_child_key"`
				SpawnIndex    *int   `json:"spawn_index"`
			}
			if err := json.Unmarshal([]byte(r.EngineState), &es); err != nil {
				return nil, fmt.Errorf("decode engine_state of %s: %w", r.ID, err)
			}
			c.Key = es.SpawnChildKey
			if es.SpawnIndex != nil {
				c.Index = *es.SpawnIndex
			}
		}
		out = append(out, c)
	}
	return out, nil
}
