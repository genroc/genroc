package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// ArmExternalOrConsumeSignal is the engine's atomic entry into an external task. Under
// the instance row lock (shared with DeliverSignal, so the two never interleave) it
// either consumes the oldest buffered signal — a progress checkpoint that stores it as
// _external_result and releases the lease, so the next claim resumes via runExternal
// phase 2 — or parks the instance (wait_state='external', per-occurrence token + input
// in _external), also releasing. Pop-and-write is one commit, so the signal is never
// lost: a crash after it finds the result on the row, a refused (stale-lease) write
// rolls the pop back into the queue. Both branches end the work session; no outcome
// keeps the lease.
func (db *DB) ArmExternalOrConsumeSignal(ctx context.Context, inst *model.ProcessInstance, taskID, token string, input any, wakeAt *time.Time) (consumed bool, result any, err error) {
	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()

	// Take the instance row lock first — the same lock DeliverSignal takes — so a signal
	// arriving during arming serializes either fully before (we pop it) or fully after
	// (it finds us parked and resolves directly). No lost signal, no deadlock. The FOR
	// UPDATE makes this read hand-written; everything else goes through sqlc.
	var one int
	switch err := raw.QueryRowContext(ctx, `SELECT 1 FROM process_instances WHERE id = ?`+db.forUpdate(), inst.ID).Scan(&one); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return false, nil, fmt.Errorf("instance %q: %w", inst.ID, ErrNotFound)
	default:
		return false, nil, fmt.Errorf("lock instance: %w", err)
	}

	// An empty signal queue is the ordinary case, not a lookup failure: it is what
	// sends this call down the park branch below. Hence sql.ErrNoRows here, never
	// ErrNotFound.
	resultStr, popErr := qtx.PopOldestSignal(ctx, dbgen.PopOldestSignalParams{InstanceID: inst.ID, TaskID: taskID})
	if popErr != nil && !errors.Is(popErr, sql.ErrNoRows) {
		return false, nil, fmt.Errorf("pop signal: %w", popErr)
	}

	now := nowMillis()

	if popErr == nil {
		// A buffered signal was waiting: consume it now, as an ordinary progress
		// checkpoint — the result lands in external_data and the lease is RELEASED, so
		// the work session ends and the next claim resumes via runExternal phase 2 on
		// the durable copy. (Uniform with every other persist: no outcome keeps the
		// lease.) Fenced on the engine's grant: a stale arm must not consume the
		// signal, and the refused write rolls the pop back with it, so the signal
		// stays queued — at its FIFO position — for whoever owns the instance now.
		var p any
		if err := json.Unmarshal([]byte(resultStr), &p); err != nil {
			return false, nil, fmt.Errorf("decode buffered signal: %w", err)
		}
		inst.ContextData[model.CtxExternalResult] = p
		delete(inst.ContextData, model.CtxExternal)
		inst.WaitState = model.WaitStateNone
		inst.WakeAt = nil
		cols, err := db.persistContext(ctx, qtx, inst, now)
		if err != nil {
			return false, nil, err
		}
		if err := requireFenced(qtx.UpdateInstanceProgress(ctx, dbgen.UpdateInstanceProgressParams{
			ID:           inst.ID,
			Task:         inst.Task,
			OutputsData:  cols.OutputsData,
			ErrorData:    cols.ErrorData,
			ExternalData: cols.ExternalData,
			EngineState:  cols.EngineState,
			RetryCount:   int64(inst.RetryCount),
			WakeAt:       sql.NullInt64{},
			WaitState:    string(model.WaitStateNone),
			UpdatedAt:    now,
			LeaseEpoch:   inst.LeaseEpoch,
		})); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return false, nil, err
			}
			return false, nil, fmt.Errorf("consume buffered signal: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, nil, err
		}
		return true, p, nil
	}

	// No buffered signal: park. Snapshot the input + per-occurrence token under _external;
	// UpdateInstance writes the parked state and clears worker_id/lease (the parked instance
	// is non-runnable, so the engine returns noop).
	inst.ContextData[model.CtxExternal] = map[string]any{"task_id": taskID, "token": token, "input": input}
	delete(inst.ContextData, model.CtxExternalResult)
	inst.WaitState = model.WaitStateExternal
	inst.WakeAt = wakeAt
	cols, err := db.persistContext(ctx, qtx, inst, now)
	if err != nil {
		return false, nil, err
	}
	params := updateInstanceParams(inst, cols, now)
	if err := requireFenced(qtx.UpdateInstance(ctx, params)); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return false, nil, err
		}
		return false, nil, fmt.Errorf("park external: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return false, nil, nil
}

// DeliverSignal delivers a signal to (instance, external task). Under the instance row
// lock it resolves the task immediately when armed now (and not mid-timeout-claim),
// otherwise buffers the result FIFO for the next arming (delivered reports which). The
// caller validates the result against the task's result_schema first.
func (db *DB) DeliverSignal(ctx context.Context, instanceID, taskID, signalID string, result any) (delivered bool, err error) {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("marshal result: %w", err)
	}

	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var status, waitState, currentTask, externalData string
	var workerID sql.NullString
	var leaseExpiresAt sql.NullInt64
	switch err := raw.QueryRowContext(ctx,
		`SELECT status, wait_state, task, external_data, worker_id, lease_expires_at
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
		Scan(&status, &waitState, &currentTask, &externalData, &workerID, &leaseExpiresAt); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("instance %q: %w", instanceID, ErrNotFound)
	default:
		return false, fmt.Errorf("lock instance: %w", err)
	}
	// A paused instance still accepts signals. A pause suspends execution, not delivery:
	// rejecting here would make a pause lose events, which is exactly what a pause is
	// not supposed to do.
	if status != string(model.StatusRunning) &&
		status != string(model.StatusPaused) && status != string(model.StatusPausing) {
		return false, fmt.Errorf("instance is not running (status %s); cannot signal: %w", status, ErrConflict)
	}

	// The `task` column is the current task id, so the instance is armed for this
	// signal iff it is parked on an external wait at exactly that task.
	//
	// Status is deliberately NOT part of this test. Delivering to a paused instance is
	// safe and is the only correct option: SetExternalResult stores the result and clears
	// the wait but leaves status alone, so the instance stays unclaimable and does not
	// advance until it is resumed. Treating a paused-but-armed task as unarmed would
	// buffer the signal instead — and a task that is already armed never re-arms, so the
	// buffered result would sit there unread forever.
	armed := model.WaitState(waitState) == model.WaitStateExternal && currentTask == taskID
	// A live lease means a worker is mid-advance on this row (a timeout firing); don't race
	// it — buffer instead, and the signal is consumed if the task re-arms.
	liveLeased := workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis()

	if armed && !liveLeased {
		newExt, err := withExternalResult(externalData, result)
		if err != nil {
			return false, err
		}
		// armed/lease checked above under the row lock, so the un-park is unconditional.
		if err := qtx.SetExternalResult(ctx, dbgen.SetExternalResultParams{
			ExternalData: newExt,
			UpdatedAt:    nowMillis(),
			ID:           instanceID,
		}); err != nil {
			return false, fmt.Errorf("deliver signal: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}

	if err := qtx.InsertSignal(ctx, dbgen.InsertSignalParams{
		ID:         signalID,
		InstanceID: instanceID,
		TaskID:     taskID,
		Result:     string(resultJSON),
		CreatedAt:  nowMillis(),
	}); err != nil {
		return false, fmt.Errorf("buffer signal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func (db *DB) CountBufferedSignals(instanceID, taskID string) (int, error) {
	n, err := db.q.CountBufferedSignals(context.Background(), dbgen.CountBufferedSignalsParams{
		InstanceID: instanceID,
		TaskID:     taskID,
	})
	return int(n), err
}
