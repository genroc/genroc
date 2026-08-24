package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// ArmExternalOrConsumeSignal is the engine's atomic entry into an external task. Under
// the instance row lock (shared with DeliverSignal, so the two never interleave) it
// either consumes the oldest buffered signal — stored under the slot its kind owns, resumed
// by the next claim via runExternal phase 2 — or parks the instance (wait_state='external',
// input in _external). Both branches release the lease; pop-and-write is one
// commit, so the signal survives a crash or a refused (stale-lease) write.
func (db *DB) ArmExternalOrConsumeSignal(ctx context.Context, inst *model.ProcessInstance, taskID string, input any, wakeAt *time.Time) (consumed bool, outcome model.ExternalOutcome, err error) {
	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, model.ExternalOutcome{}, err
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
		return false, model.ExternalOutcome{}, fmt.Errorf("instance %q: %w", inst.ID, ErrNotFound)
	default:
		return false, model.ExternalOutcome{}, fmt.Errorf("lock instance: %w", err)
	}

	// An empty signal queue is the ordinary case, not a lookup failure: it is what
	// sends this call down the park branch below. Hence sql.ErrNoRows here, never
	// ErrNotFound.
	outcomeStr, popErr := qtx.PopOldestSignal(ctx, dbgen.PopOldestSignalParams{InstanceID: inst.ID, TaskID: taskID})
	if popErr != nil && !errors.Is(popErr, sql.ErrNoRows) {
		return false, model.ExternalOutcome{}, fmt.Errorf("pop signal: %w", popErr)
	}

	now := nowMillis()

	if popErr == nil {
		// Consume as an ordinary checkpoint: the outcome into external_data, lease released,
		// next claim resumes via runExternal phase 2. The fence shares the pop's
		// transaction, so a stale arm rolls the signal back to its FIFO position.
		outcome, err := model.UnmarshalOutcome(outcomeStr)
		if err != nil {
			return false, model.ExternalOutcome{}, err
		}
		key, value := outcome.ContextValue()
		inst.ContextData[key] = value
		delete(inst.ContextData, model.CtxExternal)
		inst.WaitState = model.WaitStateNone
		inst.WakeAt = nil
		cols, err := db.persistContext(ctx, qtx, inst, now)
		if err != nil {
			return false, model.ExternalOutcome{}, err
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
				return false, model.ExternalOutcome{}, err
			}
			return false, model.ExternalOutcome{}, fmt.Errorf("consume buffered signal: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, model.ExternalOutcome{}, err
		}
		return true, outcome, nil
	}

	// No buffered signal: park. Snapshot the input under _external;
	// UpdateInstance writes the parked state and clears worker_id/lease (the parked instance
	// is non-runnable, so the engine returns noop).
	// No token here: the occurrence is task_epoch on this very row, and a copy in
	// external_data would be a second thing to keep true.
	inst.ContextData[model.CtxExternal] = map[string]any{"task_id": taskID, "input": input}
	delete(inst.ContextData, model.CtxExternalResult)
	inst.WaitState = model.WaitStateExternal
	inst.WakeAt = wakeAt
	cols, err := db.persistContext(ctx, qtx, inst, now)
	if err != nil {
		return false, model.ExternalOutcome{}, err
	}
	params := updateInstanceParams(inst, cols, now)
	if err := requireFenced(qtx.UpdateInstance(ctx, params)); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return false, model.ExternalOutcome{}, err
		}
		return false, model.ExternalOutcome{}, fmt.Errorf("park external: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, model.ExternalOutcome{}, err
	}
	return false, model.ExternalOutcome{}, nil
}

// DeliverSignal delivers an outcome -- a result or a failure -- to (instance, external task).
// Under the instance row lock it resolves the task immediately when armed now (and not
// mid-timeout-claim), otherwise buffers it FIFO for the next arming (delivered reports which).
// The caller validates it against what the task declares first.
func (db *DB) DeliverSignal(ctx context.Context, instanceID, taskID, signalID string, outcome model.ExternalOutcome) (delivered bool, err error) {
	outcomeJSON, err := model.MarshalOutcome(outcome)
	if err != nil {
		return false, err
	}

	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var status, waitState, currentTask, externalData string
	var workerID, extWorkerID sql.NullString
	var leaseExpiresAt, extLeaseExpiresAt sql.NullInt64
	switch err := raw.QueryRowContext(ctx,
		`SELECT status, wait_state, task, external_data, worker_id, lease_expires_at,
		        external_worker_id, external_lease_expires_at
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
		Scan(&status, &waitState, &currentTask, &externalData, &workerID, &leaseExpiresAt,
			&extWorkerID, &extLeaseExpiresAt); {
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

	// Armed iff parked on an external wait at exactly this task. Status is deliberately NOT
	// tested: delivering to a paused instance stores the result and leaves it unclaimable —
	// treating it as unarmed would buffer a result no re-arm will ever read.
	armed := model.WaitState(waitState) == model.WaitStateExternal && currentTask == taskID
	// A live lease means a worker is mid-advance on this row (a timeout firing); don't race
	// it — buffer instead, and the signal is consumed if the task re-arms. A live external
	// CLAIM is the same situation with a different holder — someone is working on this answer
	// right now — so it gets the same treatment rather than a rule of its own. A signal is
	// deliberately the unclaimed push route: it carries no handle to fence with, so deferring
	// is the only way it cannot answer over a worker mid-flight.
	liveLeased := (workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis()) ||
		(extWorkerID.Valid && extLeaseExpiresAt.Valid && extLeaseExpiresAt.Int64 > nowMillis())

	if armed && !liveLeased {
		newExt, err := withExternalOutcome(externalData, outcome)
		if err != nil {
			return false, err
		}
		// armed/lease checked above under the row lock, so the un-park is unconditional.
		if err := qtx.SetExternalOutcome(ctx, dbgen.SetExternalOutcomeParams{
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
		Outcome:    outcomeJSON,
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
