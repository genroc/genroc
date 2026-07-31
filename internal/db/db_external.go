package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// externalPaginator is the pagination policy for the external-task queue.
// baseWhere keeps wait_state='external' a literal so Postgres matches the partial
// idx_external_queue index (a bound parameter would not); that index's trailing
// updated_at column also backs the sort. Keys on park time (updated_at) with the
// UUIDv7 id tiebreaker, oldest first.
//
// Paused instances are excluded: ResolveExternalTask rejects anything that is not
// running, so advertising a suspended task would hand external workers something they
// cannot submit a result for. They reappear in the queue when the process resumes.
var externalPaginator = paginator{
	table:      "process_instances",
	columns:    instanceColumns,
	baseWhere:  "wait_state = 'external' AND status = 'running'",
	filterCols: []string{"process_name", "process_version", "task"},
	sorts: map[string]sortMode{
		"updated": {{"updated_at", kindInt}, {"id", kindText}},
	},
	defSort:  "updated",
	defDesc:  false,
	defLimit: 20,
	maxLimit: 100,
}

// ListExternalTasks returns a page of instances parked on an external task, filtered
// by process name/version (empty/0 = any) and current task id (empty = any). task is
// the current-task column (the resolvable task id for a parked instance), so it
// filters in SQL — pages stay full and the before/after counts stay accurate.
func (db *DB) ListExternalTasks(processName string, processVersion int, task string, req PageReq) ([]*model.ProcessInstance, PageInfo, error) {
	b, err := externalPaginator.query(req).
		EqIf("process_name", processName, processName != "").
		EqIf("process_version", int64(processVersion), processVersion != 0).
		EqIf("task", task, task != "").
		build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return db.queryInstancePage(b)
}

// ResolveExternalTask atomically delivers a result to an instance parked on an external
// task and un-parks it. Under the row lock (FOR UPDATE on Postgres; SQLite single-writer)
// it rejects anything but a live external wait: an expired/absent wait, a live lease (a
// timeout claim in flight — the timeout wins), or a token mismatch (a stale token from a
// prior arming — the exact-occurrence guarantee). The engine consumes the stored result
// on the next claim.
func (db *DB) ResolveExternalTask(ctx context.Context, instanceID, token string, result any) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		var status, waitState, externalData string
		var workerID sql.NullString
		var leaseExpiresAt sql.NullInt64
		err := raw.QueryRowContext(ctx,
			`SELECT status, wait_state, external_data, worker_id, lease_expires_at
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
			Scan(&status, &waitState, &externalData, &workerID, &leaseExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("external task: %w", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("lock instance: %w", err)
		}

		if status != string(model.StatusRunning) || model.WaitState(waitState) != model.WaitStateExternal {
			return fmt.Errorf("task is not waiting for an external result: %w", ErrConflict)
		}
		// A live lease means a worker already claimed this instance (a timeout firing); the
		// timeout wins, so reject the submit rather than racing its advance.
		if workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis() {
			return fmt.Errorf("external task is being processed; try again: %w", ErrConflict)
		}

		storedToken, err := externalToken(externalData)
		if err != nil {
			return err
		}
		if storedToken == "" || storedToken != token {
			return fmt.Errorf("token does not match the waiting task (it may have already been resolved or re-armed): %w", ErrConflict)
		}

		newExt, err := withExternalResult(externalData, result)
		if err != nil {
			return fmt.Errorf("marshal external_data: %w", err)
		}
		// The status/wait_state/token/lease checks above ran under the row lock, so the
		// un-park is unconditional here.
		if err := qtx.SetExternalResult(ctx, dbgen.SetExternalResultParams{
			ExternalData: newExt,
			UpdatedAt:    nowMillis(),
			ID:           instanceID,
		}); err != nil {
			return fmt.Errorf("resolve external task: %w", err)
		}
		return nil
	})
}
