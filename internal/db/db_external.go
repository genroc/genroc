package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// Pagination for the external-task queue. baseWhere keeps wait_state='external' literal so
// Postgres matches the partial idx_external_queue index; sorted by park time. Paused
// instances are excluded — resolve rejects them, so listing one hands out dead work.
var externalPaginator = paginator{
	table:      "process_instances",
	columns:    instanceColumns,
	baseWhere:  "wait_state = 'external' AND status = 'running'",
	filterCols: []string{"process_name", "process_version", "task", "updated_at"},
	sorts: map[string]sortMode{
		"updated": {{"updated_at", kindInt}, {"id", kindText}},
	},
	// updated, not created: updated_at is when the instance parked, which is what the
	// queue is ordered by and what idx_external_queue covers. created_at would order by
	// when instances started, which for a wait queue is a different question.
	defSort:  "updated",
	defDesc:  true, // newest first, as every list endpoint defaults
	defLimit: 20,
	maxLimit: 100,
}

// ListExternalTasks returns a page of instances parked on an external task, filtered
// by process name/version (empty/0 = any), current task id (empty = any), and a Window on
// updated_at — the park time, and this list's only sort (zero = unbounded). task is the
// current-task column (the resolvable task id for a parked instance), so it filters in
// SQL — pages stay full and the before/after counts stay accurate.
func (db *DB) ListExternalTasks(processName string, processVersion int, task string, updated Window, req PageReq) ([]*model.ProcessInstance, PageInfo, error) {
	q := externalPaginator.query(req).
		EqIf("process_name", processName, processName != "").
		EqIf("process_version", int64(processVersion), processVersion != 0).
		EqIf("task", task, task != "")
	b, err := updated.apply(q, "updated_at").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return db.queryInstancePage(b)
}

// ClaimBinding is the claim half of a submitted handle: the epoch a three-part token named, or
// Unclaimed for the two-part form. It is checked under the same row lock as the wait state and
// task_epoch -- requireFenced/ErrLeaseLost in the API's vocabulary, refusing with a conflict
// that names re-claim as the cause.
type ClaimBinding struct {
	epoch int64
	bound bool
}

// Unclaimed is the binding a two-part token carries: no grant is being named.
var Unclaimed = ClaimBinding{}

// BoundToClaim binds an answer to the grant a three-part token named.
func BoundToClaim(epoch int64) ClaimBinding { return ClaimBinding{epoch: epoch, bound: true} }

// check enforces the two directions. A bound handle must name the CURRENT grant: an expiry
// writes nothing, so a worker that overran its lease and was never taken over still answers
// successfully -- strictly better than discarding work already done, and how the engine treats
// its own late writes. An unbound handle is refused only while a claim is LIVE: the queue hands
// two-part tokens to any caller, and one must not be able to answer over a working holder.
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

// ResolveExternalTask atomically delivers an outcome -- a result or a failure -- to an
// instance parked on an external task, and un-parks it. The engine consumes it on the next
// claim; a failure is routed through on_error there rather than here, because resolving a
// retry policy and moving retry_count/wake_at are writes on a leased row and this call holds
// no lease. See specs/external-task-queue.md.
//
// Under the row lock (FOR UPDATE on Postgres; SQLite single-writer) it rejects an
// expired/absent wait, a live lease (a timeout claim in flight -- the timeout wins), and an
// epoch mismatch (an outcome submitted against a PRIOR arming -- the exact-occurrence
// guarantee). The epoch comes off the row rather than a token copied into external_data:
// task_epoch is already the number of the occurrence, so storing it twice only creates two
// things that can disagree. See internal/db/CLAUDE.md.
func (db *DB) ResolveExternalTask(ctx context.Context, instanceID string, epoch int64, claim ClaimBinding, outcome model.ExternalOutcome) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {

		var status, waitState, externalData string
		var workerID, extWorkerID sql.NullString
		var leaseExpiresAt, extLeaseExpiresAt sql.NullInt64
		var taskEpoch, claimEpoch int64
		err := raw.QueryRowContext(ctx,
			`SELECT status, wait_state, external_data, worker_id, lease_expires_at, task_epoch,
			        external_worker_id, external_lease_expires_at, external_claim_epoch
		   FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
			Scan(&status, &waitState, &externalData, &workerID, &leaseExpiresAt, &taskEpoch,
				&extWorkerID, &extLeaseExpiresAt, &claimEpoch)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("external task: %w", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("lock instance: %w", err)
		}

		// A pause suspends execution, not delivery: an answer to work already handed out is
		// always accepted, and only the CLAIM side refuses a suspended tree. Refusing here
		// would leave the instance parked with its deadline still running, and on an
		// only_once task the external.timeout that follows can never be retried -- so the
		// work would be lost after it had already taken effect. Mirrors the status set
		// DeliverSignal accepts. specs/external-task-queue.md §Pause.
		if !model.Status(status).AcceptsExternalOutcome() || model.WaitState(waitState) != model.WaitStateExternal {
			return fmt.Errorf("task is not waiting for an external result: %w", ErrConflict)
		}
		// A live lease means a worker already claimed this instance (a timeout firing); the
		// timeout wins, so reject the submit rather than racing its advance.
		if workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis() {
			return fmt.Errorf("external task is being processed; try again: %w", ErrConflict)
		}

		if taskEpoch != epoch {
			return fmt.Errorf("token does not match the waiting task (it may have already been resolved or re-armed): %w", ErrConflict)
		}
		if err := claim.check(claimEpoch, extWorkerID, extLeaseExpiresAt); err != nil {
			return err
		}

		newExt, err := withExternalOutcome(externalData, outcome)
		if err != nil {
			return fmt.Errorf("marshal external_data: %w", err)
		}
		// The status/wait_state/token/lease checks above ran under the row lock, so the
		// un-park is unconditional here.
		if err := qtx.SetExternalOutcome(ctx, dbgen.SetExternalOutcomeParams{
			ExternalData: newExt,
			UpdatedAt:    nowMillis(),
			ID:           instanceID,
		}); err != nil {
			return fmt.Errorf("resolve external task: %w", err)
		}
		return nil
	})
}
