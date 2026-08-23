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
	filterCols: []string{"status", "error_code", "created_at", "updated_at"},
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
	call_stack, retry_count, wake_at, status, error,
	created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
	input_data, outputs_data, output_data, error_data, external_data, engine_state, task,
	error_code, lease_epoch, task_epoch, parent_task_epoch`

// Lightweight ListInstances projection — no context/call-stack blobs; order matches
// scanInstanceSummary. error_code stays despite the rule: short, and it is what a list
// is scanned for when something has gone wrong.
const instanceSummaryColumns = `id, process_name, process_version, retry_count,
	status, wait_state, task, error, error_code, created_at, updated_at`

// Column order must match instanceSummaryColumns.
func scanInstanceSummary(s interface{ Scan(...any) error }) (*model.InstanceSummary, error) {
	var (
		r                          model.InstanceSummary
		processVersion, retryCount int64
		status, waitState          string
		createdAt, updatedAt       int64
	)
	if err := s.Scan(
		&r.ID, &r.ProcessName, &processVersion, &retryCount,
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
		&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.Error,
		&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
		&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorData, &r.ExternalData, &r.EngineState, &r.Task,
		&r.ErrorCode, &r.LeaseEpoch, &r.TaskEpoch, &r.ParentTaskEpoch,
	)
	return r, err
}

// contextCols holds the six decomposed context columns as serialized JSON, ready
// to drop into an Insert/Update params struct.
type contextCols struct {
	InputData, OutputsData, OutputData, ErrorData, ExternalData, EngineState string
}

// outputsColumn is the on-disk shape of outputs_data: the completion order plus the
// per-task output envelopes, each independently inline-or-externalized.
type outputsColumn struct {
	Order []string                   `json:"order,omitempty"`
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

// encodeValueSlot encodes a value-bearing slot: big values become an object reference
// (appended to pending, recorded in referenced), small ones stay inline, and an
// unchanged *model.ObjectRef is re-emitted with no new object.
func encodeValueSlot(v any, pending *[]*pendingObject, referenced map[string]struct{}) (model.Envelope, error) {
	env, p, err := encodeContextValue(v)
	if err != nil {
		return model.Envelope{}, err
	}
	if p != nil {
		*pending = append(*pending, p)
	}
	if env.IsRef() {
		referenced[env.Refs[0].Ref] = struct{}{}
	}
	return env, nil
}

// encodeContext splits inst.ContextData into the six column strings, collecting the
// objects to write (pending) and the hashes the value-slots still reference
// (referenced) so the write transaction can pin new objects and dereference dropped ones.
func encodeContext(inst *model.ProcessInstance) (cols contextCols, pending []*pendingObject, referenced map[string]struct{}, err error) {
	referenced = map[string]struct{}{}
	cd := inst.ContextData

	encodeSlot := func(v any) (string, error) {
		env, e := encodeValueSlot(v, &pending, referenced)
		if e != nil {
			return "", e
		}
		b, e := json.Marshal(env)
		return string(b), e
	}

	if v, ok := cd["input"]; ok {
		if cols.InputData, err = encodeSlot(v); err != nil {
			return
		}
	}
	if outs, ok := cd["outputs"].(map[string]any); ok {
		oc := outputsColumn{Order: toStringSlice(cd["output_order"]), Items: map[string]json.RawMessage{}}
		for k, v := range outs {
			env, e := encodeValueSlot(v, &pending, referenced)
			if e != nil {
				err = e
				return
			}
			b, e := json.Marshal(env)
			if e != nil {
				err = e
				return
			}
			oc.Items[k] = b
		}
		b, e := json.Marshal(oc)
		if e != nil {
			err = e
			return
		}
		cols.OutputsData = string(b)
	}
	if v, ok := cd["output"]; ok {
		if cols.OutputData, err = encodeSlot(v); err != nil {
			return
		}
	}
	if v, ok := cd["error"]; ok {
		// `data` is enveloped ALONE — externalized past the inline cutoff, like a task
		// output — while task/message/code stay inline. An error body is as large as any
		// response, and a handler reading only error.code must not drag one through an
		// object load. Old rows carry no `data` key and decode unchanged.
		if m, isMap := v.(map[string]any); isMap {
			if d, hasData := m["data"]; hasData {
				env, e := encodeValueSlot(d, &pending, referenced)
				if e != nil {
					err = e
					return
				}
				withEnv := make(map[string]any, len(m))
				for k, val := range m {
					withEnv[k] = val
				}
				withEnv["data"] = env
				v = withEnv
			}
		}
		b, e := json.Marshal(v)
		if e != nil {
			err = e
			return
		}
		cols.ErrorData = string(b)
	}
	if cols.ExternalData, err = encodeExternalData(cd); err != nil {
		return
	}
	cols.EngineState, err = encodeEngineState(cd)
	return
}

// encodeExternalData serialises the parked external-task bookkeeping (task_id, token,
// input snapshot, submitted outcome) into external_data, or "" when none is present.
// External payloads stay inline (their own column keeps them off the runnable index).
func encodeExternalData(cd map[string]any) (string, error) {
	ext := map[string]any{}
	if e, ok := cd[model.CtxExternal].(map[string]any); ok {
		for k, v := range e {
			ext[k] = v
		}
	}
	if r, ok := cd[model.CtxExternalResult]; ok {
		ext["result"] = r
		ext["has_result"] = true
	}
	if f, ok := cd[model.CtxExternalError]; ok {
		ext["error"] = f
		ext["has_error"] = true
	}
	if len(ext) == 0 {
		return "", nil
	}
	b, err := json.Marshal(ext)
	return string(b), err
}

// withExternalOutcome writes a submitted outcome into an external_data column value, under
// the slot its kind owns, marking has_<slot> so the engine consumes it on the next claim.
// Used by the resolve/deliver paths that operate on the column string rather than the
// in-memory context map. The two slots are mutually exclusive by construction: every caller
// holds the instance row lock and has already refused a row that is not parked, which is what
// lets phase 2 read them in a fixed order rather than reconciling them.
func withExternalOutcome(externalData string, o model.ExternalOutcome) (string, error) {
	key, v := o.ContextValue()
	slot := "result"
	if key == model.CtxExternalError {
		slot = "error"
	}
	return withExternalSlot(externalData, slot, v)
}

func withExternalSlot(externalData, slot string, v any) (string, error) {
	ext := map[string]any{}
	if externalData != "" {
		if err := numeric.Decode([]byte(externalData), &ext); err != nil {
			return "", fmt.Errorf("decode external_data: %w", err)
		}
	}
	ext[slot] = v
	ext["has_"+slot] = true
	b, err := json.Marshal(ext)
	return string(b), err
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
// names (and back, in decodeContext).
var engineStateKeys = map[string]string{
	"_children":          "children",
	"_spawn_action_type": "spawn_action_type",
	"_spawn_child_key":   "spawn_child_key",
	"_spawn_index":       "spawn_index",
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

// persistContext encodes inst's context, writes/dereferences the implied objects via
// qtx (inside the caller's transaction), and returns the column strings for the
// caller's Insert/Update params.
func (db *DB) persistContext(ctx context.Context, qtx *dbgen.Queries, inst *model.ProcessInstance, now int64) (contextCols, error) {
	cols, pending, referenced, err := encodeContext(inst)
	if err != nil {
		return contextCols{}, err
	}
	if err := db.applyContextObjectDiff(ctx, qtx, inst.ID, pending, inst.LoadedObjectHashes, referenced, now); err != nil {
		return contextCols{}, err
	}
	return cols, nil
}

func updateInstanceParams(inst *model.ProcessInstance, cols contextCols, now int64) dbgen.UpdateInstanceParams {
	return dbgen.UpdateInstanceParams{
		ID:           inst.ID,
		Task:         inst.Task,
		OutputsData:  cols.OutputsData,
		OutputData:   cols.OutputData,
		ErrorData:    cols.ErrorData,
		ExternalData: cols.ExternalData,
		EngineState:  cols.EngineState,
		RetryCount:   int64(inst.RetryCount),
		WakeAt:       fromTimePtr(inst.WakeAt),
		Status:       string(inst.Status),
		WaitState:    string(inst.WaitState),
		Error:        inst.Error,
		ErrorCode:    inst.ErrorCode,
		UpdatedAt:    now,
		LeaseEpoch:   inst.LeaseEpoch,
		TaskEpoch:    inst.TaskEpoch,
	}
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
func insertInstanceParams(inst *model.ProcessInstance, cols contextCols, status string, createdAt, updatedAt int64) (dbgen.InsertInstanceParams, error) {
	callStack, err := json.Marshal(inst.CallStack)
	if err != nil {
		return dbgen.InsertInstanceParams{}, err
	}
	return dbgen.InsertInstanceParams{
		ID:              inst.ID,
		ProcessName:     inst.ProcessName,
		ProcessVersion:  int64(inst.ProcessVersion),
		Task:            inst.Task,
		InputData:       cols.InputData,
		OutputsData:     cols.OutputsData,
		OutputData:      cols.OutputData,
		ErrorData:       cols.ErrorData,
		ExternalData:    cols.ExternalData,
		EngineState:     cols.EngineState,
		ParentID:        inst.ParentID,
		SpawnTaskID:     inst.SpawnTaskID,
		ParentTaskEpoch: inst.ParentTaskEpoch,
		TaskEpoch:       inst.TaskEpoch,
		CallStack:       string(callStack),
		RetryCount:      int64(inst.RetryCount),
		WakeAt:          fromTimePtr(inst.WakeAt),
		Status:          status,
		WaitState:       string(inst.WaitState),
		Error:           inst.Error,
		ErrorCode:       inst.ErrorCode,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}, nil
}

func (db *DB) SaveInstance(inst *model.ProcessInstance) error {
	ctx := context.Background()
	now := nowMillis()
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		cols, err := db.persistContext(ctx, qtx, inst, now)
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
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		cols, err := db.persistContext(ctx, qtx, inst, now)
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
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		cols, err := db.persistContext(ctx, qtx, inst, now)
		if err != nil {
			return err
		}
		return requireFenced(qtx.UpdateInstanceProgress(ctx, dbgen.UpdateInstanceProgressParams{
			ID:           inst.ID,
			Task:         inst.Task,
			OutputsData:  cols.OutputsData,
			ErrorData:    cols.ErrorData,
			ExternalData: cols.ExternalData,
			EngineState:  cols.EngineState,
			RetryCount:   int64(inst.RetryCount),
			WakeAt:       fromTimePtr(inst.WakeAt),
			WaitState:    string(inst.WaitState),
			UpdatedAt:    now,
			LeaseEpoch:   inst.LeaseEpoch,
			TaskEpoch:    inst.TaskEpoch,
		}))
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
// (empty = all), error code, and a Window on either timestamp (zero = unbounded). The two
// windows are separate rather than one resolved against the active sort: a caller walking
// forward pairs its bound with the sort it ordered by, and naming the column keeps that
// pairing the caller's to state instead of this function's to guess.
// Summaries omit the context blob — use GetInstance for full detail.
func (db *DB) ListInstances(status, errorCode string, created, updated Window, req PageReq) ([]*model.InstanceSummary, PageInfo, error) {
	q := instancePaginator.query(req).
		EqIf("status", status, status != "").
		EqIf("error_code", errorCode, errorCode != "")
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
		ParentID:        r.ParentID,
		SpawnTaskID:     r.SpawnTaskID,
		RetryCount:      int(r.RetryCount),
		Status:          model.Status(r.Status),
		WaitState:       model.WaitState(r.WaitState),
		Error:           r.Error,
		ErrorCode:       r.ErrorCode,
		CreatedAt:       toTime(r.CreatedAt),
		UpdatedAt:       toTime(r.UpdatedAt),
		WakeAt:          toTimePtr(r.WakeAt),
		WorkerID:        nullStringPtr(r.WorkerID),
		LeaseExpiresAt:  toTimePtr(r.LeaseExpiresAt),
		LeaseEpoch:      r.LeaseEpoch,
		TaskEpoch:       r.TaskEpoch,
		ParentTaskEpoch: r.ParentTaskEpoch,
	}
	cd, loaded, err := decodeContext(r)
	if err != nil {
		return nil, err
	}
	inst.ContextData = cd
	inst.LoadedObjectHashes = loaded
	if err := json.Unmarshal([]byte(r.CallStack), &inst.CallStack); err != nil {
		return nil, fmt.Errorf("unmarshal call_stack: %w", err)
	}
	return inst, nil
}

// decodeContext reassembles the six context columns into the in-memory ContextData
// map. Externalized value-slots become *model.ObjectRef markers (resolved lazily);
// loaded is the set of referenced hashes, used at write time to dereference dropped ones.
func decodeContext(r dbgen.ProcessInstance) (map[string]any, map[string]struct{}, error) {
	cd := map[string]any{}
	loaded := map[string]struct{}{}

	decodeSlot := func(s, key string) error {
		if s == "" {
			return nil
		}
		var env model.Envelope
		if err := numeric.Decode([]byte(s), &env); err != nil {
			return fmt.Errorf("decode %s envelope: %w", key, err)
		}
		if env.IsRef() {
			loaded[env.Refs[0].Ref] = struct{}{}
		}
		cd[key] = decodeEnvelope(env)
		return nil
	}

	if err := decodeSlot(r.InputData, "input"); err != nil {
		return nil, nil, err
	}
	if r.OutputsData != "" {
		var oc outputsColumn
		if err := numeric.Decode([]byte(r.OutputsData), &oc); err != nil {
			return nil, nil, fmt.Errorf("decode outputs_data: %w", err)
		}
		items := make(map[string]any, len(oc.Items))
		for k, raw := range oc.Items {
			var env model.Envelope
			if err := numeric.Decode(raw, &env); err != nil {
				return nil, nil, fmt.Errorf("decode output %s envelope: %w", k, err)
			}
			if env.IsRef() {
				loaded[env.Refs[0].Ref] = struct{}{}
			}
			items[k] = decodeEnvelope(env)
		}
		cd["outputs"] = items
		if oc.Order != nil {
			cd["output_order"] = oc.Order
		}
	}
	if err := decodeSlot(r.OutputData, "output"); err != nil {
		return nil, nil, err
	}
	if r.ErrorData != "" {
		var fields map[string]json.RawMessage
		if err := numeric.Decode([]byte(r.ErrorData), &fields); err != nil {
			return nil, nil, fmt.Errorf("decode error_data: %w", err)
		}
		ev := make(map[string]any, len(fields))
		for k, raw := range fields {
			if k == "data" {
				var env model.Envelope
				if err := numeric.Decode(raw, &env); err != nil {
					return nil, nil, fmt.Errorf("decode error data envelope: %w", err)
				}
				if env.IsRef() {
					loaded[env.Refs[0].Ref] = struct{}{}
				}
				ev[k] = decodeEnvelope(env)
				continue
			}
			var val any
			if err := numeric.Decode(raw, &val); err != nil {
				return nil, nil, fmt.Errorf("decode error_data %s: %w", k, err)
			}
			ev[k] = val
		}
		cd["error"] = ev
	}
	if r.ExternalData != "" {
		var ext map[string]any
		if err := numeric.Decode([]byte(r.ExternalData), &ext); err != nil {
			return nil, nil, fmt.Errorf("decode external_data: %w", err)
		}
		hasResult, _ := ext["has_result"].(bool)
		result := ext["result"]
		hasError, _ := ext["has_error"].(bool)
		failure, _ := ext["error"].(map[string]any)
		delete(ext, "has_result")
		delete(ext, "result")
		delete(ext, "has_error")
		delete(ext, "error")
		// The remainder is the parked bookkeeping (task_id, input); what is left after both
		// outcomes are lifted out is what _external holds.
		if len(ext) > 0 {
			cd[model.CtxExternal] = ext
		}
		if hasResult {
			cd[model.CtxExternalResult] = result
		}
		if hasError {
			cd[model.CtxExternalError] = failure
		}
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
	return cd, loaded, nil
}
