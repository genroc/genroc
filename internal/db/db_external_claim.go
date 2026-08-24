package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// claimableWhere is the external-task claim predicate. A row is claimable when it is parked on
// an external wait, its tree is running, no live claim holds it, and its own deadline has not
// already fired -- handing out work the engine is about to time out would spend a worker on an
// answer that can no longer be accepted.
//
// It reads NONE of the engine's lease columns, which is what keeps the two claims independent:
// ClaimInstances still takes the row at wake_at however long a worker holds it, and the
// external.timeout it raises stays on time. specs/external-task-queue.md.
const claimableWhere = `wait_state = 'external' AND status = 'running'
		  AND (external_worker_id IS NULL OR external_lease_expires_at <= ?)
		  AND (wake_at IS NULL OR wake_at > ?)`

// ClaimExternalTasks atomically leases up to limit parked external tasks to workerID, oldest
// park first (FIFO -- the reverse of ListExternalTasks, whose newest-first is a UI affordance).
// Filters are the queue's own: process name, version and task id, each empty/0 for any.
//
// The ONLY place external_claim_epoch moves: a claim is a grant, and the bump fences out
// whoever held the previous one. Three things it must not do, each of which breaks silently:
// touch task_epoch (a claim is not a new occurrence, and bumping it invalidates every handle
// already given out), touch the engine's lease columns (above), or clear external_worker_id on
// expiry (that is the evidence a lost claim is recognised by).
func (db *DB) ClaimExternalTasks(workerID string, leaseDur time.Duration, limit int, processName string, processVersion int, task string) ([]*model.ProcessInstance, error) {
	now := nowMillis()
	leaseExpiry := now + leaseDur.Milliseconds()
	ctx := context.Background()

	where := claimableWhere
	args := []any{now, now}
	if processName != "" {
		where += ` AND process_name = ?`
		args = append(args, processName)
	}
	if processVersion != 0 {
		where += ` AND process_version = ?`
		args = append(args, int64(processVersion))
	}
	if task != "" {
		where += ` AND task = ?`
		args = append(args, task)
	}

	if db.dialect == "postgres" {
		// One statement, as ClaimInstances does it: a CTE picks the candidates under
		// FOR UPDATE SKIP LOCKED so concurrent workers never block on each other.
		query := `
			WITH cand AS (
				SELECT id AS cand_id
				FROM process_instances
				WHERE ` + where + `
				ORDER BY updated_at ASC, id ASC
				LIMIT ? FOR UPDATE SKIP LOCKED
			)
			UPDATE process_instances
			SET external_worker_id = ?, external_lease_expires_at = ?,
			    external_claim_epoch = process_instances.external_claim_epoch + 1
			FROM cand
			WHERE process_instances.id = cand.cand_id
			RETURNING ` + instanceColumns

		rows, err := db.exec.QueryContext(ctx, query, append(args, limit, workerID, leaseExpiry)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []*model.ProcessInstance
		for rows.Next() {
			r, err := scanInstance(rows)
			if err != nil {
				return nil, err
			}
			inst, err := toInstance(r)
			if err != nil {
				return nil, err
			}
			result = append(result, inst)
		}
		return result, rows.Err()
	}

	// SQLite cannot reference a FROM table in RETURNING, so it selects then updates inside one
	// transaction; the single-writer model makes that atomic without FOR UPDATE.
	tx, _, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := raw.QueryContext(ctx, `SELECT `+instanceColumns+`
		FROM process_instances
		WHERE `+where+`
		ORDER BY updated_at ASC, id ASC
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	var result []*model.ProcessInstance
	ids := make([]string, 0, limit)
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		inst, err := toInstance(r)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, inst)
		ids = append(ids, inst.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // the cursor must close before the UPDATE on the single connection
	if len(result) == 0 {
		return nil, tx.Commit()
	}

	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE process_instances SET external_worker_id = ?, external_lease_expires_at = ?,
		    external_claim_epoch = external_claim_epoch + 1
		 WHERE id IN (SELECT value FROM json_each(?))`,
		workerID, leaseExpiry, string(idsJSON)); err != nil {
		return nil, err
	}

	// Reflect the new grant on the returned rows; the epoch was scanned before the UPDATE, so
	// the new one is old+1 (atomic under the single writer).
	newLease := toTime(leaseExpiry)
	w := workerID
	for _, inst := range result {
		inst.ExternalWorkerID = &w
		inst.ExternalLeaseExpiresAt = &newLease
		inst.ExternalClaimEpoch++
	}
	return result, tx.Commit()
}

// RenewExternalClaims re-stamps this worker's claims on the listed instances to now+leaseDur,
// in chunks so one contended row stalls only its chunk. Mirrors RenewWorkerLeases, including
// the two rules that break silently: it must NOT bump external_claim_epoch (a renewal extends a
// grant; bumping would fence the worker out of its own answer) and must NOT clear
// external_worker_id (an unlisted row expires with the holder intact, which is the hand-back).
//
// Returns how many claims were renewed, so a worker learns it lost one rather than discovering
// it when its answer is refused.
func (db *DB) RenewExternalClaims(ctx context.Context, workerID string, ids []string, leaseDur time.Duration) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	newExpiry := nowMillis() + leaseDur.Milliseconds()
	var total int64
	for start := 0; start < len(ids); start += renewChunkSize {
		end := min(start+renewChunkSize, len(ids))
		idsJSON, err := json.Marshal(ids[start:end])
		if err != nil {
			return total, err
		}
		res, err := db.exec.ExecContext(ctx,
			`UPDATE process_instances SET external_lease_expires_at = ?
			 WHERE id IN (SELECT value FROM json_each(?)) AND external_worker_id = ?`,
			newExpiry, string(idsJSON), workerID)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// ReleaseExternalClaim hands a claimed task straight back to the queue rather than waiting out
// its lease -- the nack, and what makes a graceful worker shutdown possible.
//
// It bumps the claim epoch, unlike an expiry, which writes nothing: releasing is a deliberate
// hand-back, so the releasing worker's own handle must stop working immediately. The holder is
// verified by claim epoch, so a worker already fenced out cannot release the new holder's work.
func (db *DB) ReleaseExternalClaim(ctx context.Context, instanceID string, taskEpoch, claimEpoch int64) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		res, err := raw.ExecContext(ctx,
			`UPDATE process_instances
			   SET external_worker_id = NULL, external_lease_expires_at = NULL,
			       external_claim_epoch = external_claim_epoch + 1
			 WHERE id = ? AND task_epoch = ? AND external_claim_epoch = ?
			   AND wait_state = 'external' AND external_worker_id IS NOT NULL`,
			instanceID, taskEpoch, claimEpoch)
		if err != nil {
			return fmt.Errorf("release external claim: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("claim is no longer held (it may have expired and been re-claimed): %w", ErrConflict)
		}
		return nil
	})
}
