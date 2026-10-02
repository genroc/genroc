package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// ClaimBinding is the claim half of a submitted handle: the epoch a three-part token named, or
// Unclaimed for the two-part form. Checked under the same row lock as the wait state.
type ClaimBinding struct {
	epoch int64
	bound bool
}

// Unclaimed is the binding a two-part token carries: no grant is being named.
var Unclaimed = ClaimBinding{}

// BoundToClaim binds an answer to the grant a three-part token named.
func BoundToClaim(epoch int64) ClaimBinding { return ClaimBinding{epoch: epoch, bound: true} }

// check: a bound handle must name the CURRENT grant (an expiry writes nothing, so an overrun never
// taken over still answers); an unbound one is refused only while a claim is LIVE, since the queue
// hands two-part tokens to any caller. specs/external-task-queue.md.
func (c ClaimBinding) check(current int64, worker sql.NullString, expires sql.NullInt64) error {
	if c.bound {
		if c.epoch != current {
			return fmt.Errorf("claim was taken over (the lease expired and the task was re-claimed): %w", ErrConflict)
		}
		return nil
	}
	if worker.Valid && expires.Valid && expires.Int64 > nowMillis() {
		return fmt.Errorf("task is claimed by worker %q; answer with the claim's token or wait for it to expire: %w", worker.String, ErrConflict)
	}
	return nil
}

// ResolveExternalTask atomically buffers an outcome for an instance parked on an external task and
// un-parks it; the engine consumes it, routing a failure through on_error, on its next claim.
// ErrConflict if the wait is gone, a lease is live, or epoch names a PRIOR arming.
func (db *DB) ResolveExternalTask(ctx context.Context, instanceID string, epoch int64, claim ClaimBinding, outcome model.ExternalOutcome) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		var status, phase, taskID string
		var workerID, extWorkerID sql.NullString
		var leaseExpiresAt, extLeaseExpiresAt sql.NullInt64
		var taskEpoch, claimEpoch int64
		err := raw.QueryRowContext(ctx,
			`SELECT status, phase, task, worker_id, lease_expires_at, task_epoch,
			        external_worker_id, external_lease_expires_at, external_claim_epoch
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
			Scan(&status, &phase, &taskID, &workerID, &leaseExpiresAt, &taskEpoch,
				&extWorkerID, &extLeaseExpiresAt, &claimEpoch)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("external task: %w", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("lock instance: %w", err)
		}

		// A pause suspends execution, not delivery: refusing here leaves the deadline running
		// toward an external.timeout an only_once task can never retry.
		// specs/external-task-queue.md §Pause.
		if !model.Status(status).AcceptsExternalOutcome() || model.Phase(phase) != model.PhaseExternal {
			return fmt.Errorf("task is not waiting for an external result: %w", ErrConflict)
		}
		// A live lease is a timeout firing; it wins rather than racing its advance.
		if workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis() {
			return fmt.Errorf("external task is being processed; try again: %w", ErrConflict)
		}

		if taskEpoch != epoch {
			return fmt.Errorf("token does not match the waiting task (it may have already been resolved or re-armed): %w", ErrConflict)
		}
		if err := claim.check(claimEpoch, extWorkerID, extLeaseExpiresAt); err != nil {
			return err
		}

		// The outcome never touches the instance row: only the engine's encode under lease can cut,
		// declare and claim it. specs/external-outcome-as-signal.md.
		if err := db.bufferOutcome(ctx, qtx, instanceID, taskID, outcome); err != nil {
			return err
		}
		// Unconditional: every check above ran under the row lock.
		if err := qtx.UnparkExternal(ctx, dbgen.UnparkExternalParams{
			UpdatedAt: nowMillis(),
			ID:        instanceID,
		}); err != nil {
			return fmt.Errorf("resolve external task: %w", err)
		}
		return nil
	})
}
