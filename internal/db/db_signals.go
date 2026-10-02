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

// ArmExternalUnlessSignalled parks inst on an external wait unless an answer is already buffered,
// in which case it leaves the row claimable; it does NOT consume. Atomic against DeliverSignal
// under the same row lock. specs/external-outcome-as-signal.md.
func (db *DB) ArmExternalUnlessSignalled(ctx context.Context, inst *model.ProcessInstance, taskID string, input any, wakeAt *time.Time) (armed bool, err error) {
	// Parking is an ordinary mid-process write. What must survive is the DELIVERY into
	// this park, which is inbound and syncs on its own path (DeliverSignal, §4).
	tx, qtx, raw, err := db.beginTxAt(ctx, syncStrict, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// DeliverSignal's row lock: a signal arriving now lands fully before (no park) or fully after
	// (it un-parks us). No lost signal, no deadlock.
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
		inst.Phase = model.PhaseNone
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

	// UpdateInstance writes the park and clears the lease. No token stored: it is task_epoch on
	// this very row.
	inst.State[model.StateExternalInput] = input
	inst.Phase = model.PhaseExternal
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

// DeliverSignal buffers an outcome FIFO for (instance, external task) and, if the task is armed
// now with no live lease or claim, un-parks it (delivered reports which). The caller validates the
// outcome against the task's declaration first.
func (db *DB) DeliverSignal(ctx context.Context, instanceID, taskID string, outcome model.ExternalOutcome) (delivered bool, err error) {
	outcomeJSON, err := model.MarshalOutcome(outcome)
	if err != nil {
		return false, err
	}

	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var status, phase, currentTask string
	var workerID, extWorkerID sql.NullString
	var leaseExpiresAt, extLeaseExpiresAt sql.NullInt64
	switch err := raw.QueryRowContext(ctx,
		`SELECT status, phase, task, worker_id, lease_expires_at,
		        external_worker_id, external_lease_expires_at
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
		Scan(&status, &phase, &currentTask, &workerID, &leaseExpiresAt,
			&extWorkerID, &extLeaseExpiresAt); {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("instance %q: %w", instanceID, ErrNotFound)
	default:
		return false, fmt.Errorf("lock instance: %w", err)
	}
	// A pause suspends execution, not delivery: rejecting here would lose events.
	if status != string(model.StatusRunning) &&
		status != string(model.StatusPaused) && status != string(model.StatusPausing) {
		return false, fmt.Errorf("instance is not running (status %s); cannot signal: %w", status, ErrConflict)
	}

	// Status deliberately NOT tested: a paused instance stores the result unclaimable, and treating
	// it as unarmed would buffer a result no re-arm will ever read.
	armed := model.Phase(phase) == model.PhaseExternal && currentTask == taskID
	// A live lease or external CLAIM means someone is mid-flight: do not un-park over them. A
	// signal carries no handle to fence with, so deferring is the only safe answer.
	liveLeased := (workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis()) ||
		(extWorkerID.Valid && extLeaseExpiresAt.Valid && extLeaseExpiresAt.Int64 > nowMillis())

	// `armed` decides only whether the row also becomes claimable now. id and seq come from one
	// mint, so the FIFO can order every row.
	id, seq := db.nextSignalID()
	if err := qtx.InsertSignal(ctx, dbgen.InsertSignalParams{
		ID:         id,
		Seq:        seq,
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
func (db *DB) bufferOutcome(ctx context.Context, qtx *dbgen.Queries, instanceID, taskID string, outcome model.ExternalOutcome) error {
	outcomeJSON, err := model.MarshalOutcome(outcome)
	if err != nil {
		return err
	}
	id, seq := db.nextSignalID()
	if err := qtx.InsertSignal(ctx, dbgen.InsertSignalParams{
		ID:         id,
		Seq:        seq,
		InstanceID: instanceID,
		TaskID:     taskID,
		Outcome:    outcomeJSON,
		CreatedAt:  nowMillis(),
	}); err != nil {
		return fmt.Errorf("buffer outcome: %w", err)
	}
	return nil
}
