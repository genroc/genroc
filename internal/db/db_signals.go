package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/idgen"
	"genroc/internal/model"
)

// ArmExternalUnlessSignalled parks the instance on an external wait -- unless an answer is
// already buffered for the task, in which case it leaves the row claimable so the next claim
// consumes it through runExternal phase 2.
//
// It does NOT consume. The decision it makes is only park-or-not, and it must be atomic against a
// concurrent delivery: a signal landing between "the buffer looked empty" and "we parked" would
// find the row unparked, buffer without un-parking, and leave the instance asleep until its
// timeout. Hence the row lock -- the same one DeliverSignal takes.
// specs/external-outcome-as-signal.md.
func (db *DB) ArmExternalUnlessSignalled(ctx context.Context, inst *model.ProcessInstance, taskID string, input any, wakeAt *time.Time) (armed bool, err error) {
	// Parking is an ordinary mid-process write. What must survive is the DELIVERY into
	// this park, which is inbound and syncs on its own path (DeliverSignal, §4).
	tx, qtx, raw, err := db.beginTxAt(ctx, syncStrict, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Take the instance row lock first -- the same lock DeliverSignal takes -- so a signal
	// arriving during arming serializes either fully before (we see it and do not park) or
	// fully after (it finds us parked and un-parks us). No lost signal, no deadlock. The FOR
	// UPDATE makes this read hand-written; everything else goes through sqlc.
	var one int
	switch err := raw.QueryRowContext(ctx, `SELECT 1 FROM process_instances WHERE id = ?`+db.forUpdate(), inst.ID).Scan(&one); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("instance %q: %w", inst.ID, ErrNotFound)
	default:
		return false, fmt.Errorf("lock instance: %w", err)
	}

	// An empty queue is the ordinary case, not a lookup failure: it is what sends this call
	// down the park branch. Hence sql.ErrNoRows here, never ErrNotFound.
	_, peekErr := qtx.PeekOldestSignal(ctx, dbgen.PeekOldestSignalParams{InstanceID: inst.ID, TaskID: taskID})
	if peekErr != nil && !errors.Is(peekErr, sql.ErrNoRows) {
		return false, fmt.Errorf("peek signal: %w", peekErr)
	}
	now := nowMillis()

	if peekErr == nil {
		// An answer is already waiting. Write an ordinary checkpoint instead of parking: the
		// lease is released, the row stays claimable, and the next claim reaches phase 2.
		inst.WaitState = model.WaitStateNone
		inst.WakeAt = nil
		cols, err := db.persistState(ctx, qtx, inst, now)
		if err != nil {
			return false, err
		}
		if err := requireFenced(qtx.UpdateInstanceProgress(ctx, progressParams(inst, cols, now))); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return false, err
			}
			return false, fmt.Errorf("skip park: %w", err)
		}
		return false, tx.Commit()
	}

	// No buffered answer: park. Snapshot the input under _external; UpdateInstance writes the
	// parked state and clears worker_id/lease (the parked instance is non-runnable, so the
	// engine returns noop). No token here: the occurrence is task_epoch on this very row, and a
	// copy in external_data would be a second thing to keep true.
	inst.State[model.StateExternal] = map[string]any{"task_id": taskID, "input": input}
	inst.WaitState = model.WaitStateExternal
	inst.WakeAt = wakeAt
	cols, err := db.persistState(ctx, qtx, inst, now)
	if err != nil {
		return false, err
	}
	if err := requireFenced(qtx.UpdateInstance(ctx, updateInstanceParams(inst, cols, now))); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return false, err
		}
		return false, fmt.Errorf("park external: %w", err)
	}
	return true, tx.Commit()
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

	var status, waitState, currentTask string
	var workerID, extWorkerID sql.NullString
	var leaseExpiresAt, extLeaseExpiresAt sql.NullInt64
	switch err := raw.QueryRowContext(ctx,
		`SELECT status, wait_state, task, worker_id, lease_expires_at,
		        external_worker_id, external_lease_expires_at
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
		Scan(&status, &waitState, &currentTask, &workerID, &leaseExpiresAt,
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

	// One destination. `armed` no longer picks WHERE the outcome goes -- only whether this call
	// also makes the row claimable, so the engine reaches it now rather than at the next arm.
	if err := qtx.InsertSignal(ctx, dbgen.InsertSignalParams{
		ID:         signalID,
		InstanceID: instanceID,
		TaskID:     taskID,
		Outcome:    outcomeJSON,
		CreatedAt:  nowMillis(),
	}); err != nil {
		return false, fmt.Errorf("buffer signal: %w", err)
	}
	if armed && !liveLeased {
		// armed/lease checked above under the row lock, so the un-park is unconditional.
		if err := qtx.UnparkExternal(ctx, dbgen.UnparkExternalParams{
			UpdatedAt: nowMillis(),
			ID:        instanceID,
		}); err != nil {
			return false, fmt.Errorf("deliver signal: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return armed && !liveLeased, nil
}

// PeekSignal returns the oldest buffered outcome for (instance, task) and its id, without
// removing it. The caller acts on the outcome and hands the id back on the instance
// (ConsumedSignalID) so the delete lands in the same transaction as the state it produced.
func (db *DB) PeekSignal(instanceID, taskID string) (id string, outcome model.ExternalOutcome, ok bool, err error) {
	row, err := db.q.PeekOldestSignal(context.Background(), dbgen.PeekOldestSignalParams{
		InstanceID: instanceID, TaskID: taskID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", model.ExternalOutcome{}, false, nil
	}
	if err != nil {
		return "", model.ExternalOutcome{}, false, fmt.Errorf("peek signal: %w", err)
	}
	o, err := model.UnmarshalOutcome(row.Outcome)
	if err != nil {
		return "", model.ExternalOutcome{}, false, err
	}
	return row.ID, o, true, nil
}

func (db *DB) CountBufferedSignals(instanceID, taskID string) (int, error) {
	n, err := db.q.CountBufferedSignals(context.Background(), dbgen.CountBufferedSignalsParams{
		InstanceID: instanceID,
		TaskID:     taskID,
	})
	return int(n), err
}

// bufferOutcome appends an outcome to the FIFO for (instance, task). The ONE way an answer
// reaches a parked instance: whether the task is armed decides only whether the caller also
// un-parks it, never where the outcome goes. specs/external-outcome-as-signal.md.
func bufferOutcome(ctx context.Context, qtx *dbgen.Queries, instanceID, taskID string, outcome model.ExternalOutcome) error {
	outcomeJSON, err := model.MarshalOutcome(outcome)
	if err != nil {
		return err
	}
	if err := qtx.InsertSignal(ctx, dbgen.InsertSignalParams{
		ID:         idgen.New(),
		InstanceID: instanceID,
		TaskID:     taskID,
		Outcome:    outcomeJSON,
		CreatedAt:  nowMillis(),
	}); err != nil {
		return fmt.Errorf("buffer outcome: %w", err)
	}
	return nil
}
