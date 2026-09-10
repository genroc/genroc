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

// claimableWhere is the external-task claim predicate: parked on an external wait, tree running,
// no live claim, and its own deadline not already fired -- handing out work the engine is about
// to time out spends a worker on an answer that can no longer be accepted. It reads NONE of the
// engine's lease columns, which is what keeps the two claims independent.
// specs/external-task-queue.md.
const claimableWhere = `wait_state = 'external' AND status = 'running'
		  AND (external_worker_id IS NULL OR external_lease_expires_at <= ?)
		  AND (wake_at IS NULL OR wake_at > ?)`

// ClaimExternalTasks atomically leases up to limit parked external tasks to workerID, oldest
// park first (FIFO), filtered by process name, version and task id (each empty/0 for any).
// Claiming is the only way to enumerate the queue.
//
// The ONLY place external_claim_epoch moves, fencing out the previous holder. Three things it
// must not do, each breaking silently: touch task_epoch (invalidating every handle given out),
// touch the engine's lease columns, or clear external_worker_id on expiry (the evidence a lost
// claim is recognised by).
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
				SELECT id AS cand_id, external_worker_id AS prev_holder
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
			RETURNING ` + instanceColumns + `, cand.prev_holder`

		rows, err := db.exec.QueryContext(ctx, query, append(args, limit, workerID, leaseExpiry)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []*model.ProcessInstance
		for rows.Next() {
			// scanInstance cannot serve this list: the trailing prev_holder is not part of
			// instanceColumns, so a column added there has to be added here too.
			r, prevHolder, err := scanInstanceWithPrevHolder(rows)
			if err != nil {
				return nil, err
			}
			inst, err := toInstance(r)
			if err != nil {
				return nil, err
			}
			inst.ExternalReclaimed = prevHolder.Valid && prevHolder.String != ""
			result = append(result, inst)
		}
		return result, rows.Err()
	}

	// SQLite cannot reference a FROM table in RETURNING, so it selects then updates inside one
	// transaction; the single-writer model makes that atomic without FOR UPDATE.
	tx, qtx, raw, err := db.beginTx(ctx, nil)
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
		inst.ExternalReclaimed = inst.ExternalWorkerID != nil // prior holder present => a lapsed claim
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
	if err := qtx.GrantExternalLeases(ctx, dbgen.GrantExternalLeasesParams{
		ExternalWorkerID:       sql.NullString{String: workerID, Valid: true},
		ExternalLeaseExpiresAt: sql.NullInt64{Int64: leaseExpiry, Valid: true},
		Ids:                    string(idsJSON),
	}); err != nil {
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

// RenewOutcome is what one renewal round decided about each id the worker asked about; every
// requested id lands in exactly one list, because a worker holding several claims cannot act on
// a count. Lost and Cancelled are different instructions: Lost means the claim is someone
// else's -- stop, do NOT release, or the new holder's epoch is bumped out from under it --
// while Cancelled means stop and DO release. specs/external-task-queue.md.
type RenewOutcome struct {
	Renewed   []string
	Lost      []string
	Cancelled []string
}

// RenewExternalClaims re-stamps this worker's claims on the listed instances to now+leaseDur, in
// chunks so one contended row stalls only its chunk. Two rules that break silently: it must NOT
// bump external_claim_epoch (which would fence the worker out of its own answer) and must NOT
// clear external_worker_id (an unlisted row expires with the holder intact -- the hand-back).
//
// Renew is the only channel that reaches a worker, so cancellation rides it: the classifying
// read shares the renewal's transaction, or a row turning cancelled between the two is reported
// renewed.
func (db *DB) RenewExternalClaims(ctx context.Context, workerID string, ids []string, leaseDur time.Duration) (RenewOutcome, error) {
	out := RenewOutcome{}
	if len(ids) == 0 {
		return out, nil
	}
	newExpiry := nowMillis() + leaseDur.Milliseconds()
	held := make(map[string]model.Status, len(ids))

	for start := 0; start < len(ids); start += renewChunkSize {
		end := min(start+renewChunkSize, len(ids))
		idsJSON, err := json.Marshal(ids[start:end])
		if err != nil {
			return out, err
		}
		if err := db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
			if _, err := qtx.RenewExternalLeasesChunk(ctx, dbgen.RenewExternalLeasesChunkParams{
				NewExpiry:        sql.NullInt64{Int64: newExpiry, Valid: true},
				Ids:              string(idsJSON),
				ExternalWorkerID: sql.NullString{String: workerID, Valid: true},
			}); err != nil {
				return err
			}
			rows, err := qtx.HeldExternalClaimsChunk(ctx, dbgen.HeldExternalClaimsChunkParams{
				Ids:              string(idsJSON),
				ExternalWorkerID: sql.NullString{String: workerID, Valid: true},
			})
			if err != nil {
				return err
			}
			for _, r := range rows {
				held[r.ID] = model.Status(r.Status)
			}
			return nil
		}); err != nil {
			return out, err
		}
	}

	// Driven by the REQUESTED ids rather than the rows read back, so an id the query never
	// saw still gets an answer. That is the lost case, and it is the one a worker cannot
	// discover any other way.
	for _, id := range ids {
		switch status, ok := held[id]; {
		case !ok:
			out.Lost = append(out.Lost, id)
		case status == model.StatusCancelling || status == model.StatusCancelled:
			out.Cancelled = append(out.Cancelled, id)
		default:
			out.Renewed = append(out.Renewed, id)
		}
	}
	return out, nil
}

// ReleaseExternalClaim hands a claimed task straight back to the queue rather than waiting out
// its lease -- the nack. It bumps the claim epoch, unlike an expiry: a deliberate hand-back must
// stop the releasing worker's own handle immediately. The holder is verified by claim epoch, so
// a fenced-out worker cannot release the new holder's work.
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

// scanInstanceWithPrevHolder scans instanceColumns plus the trailing prev_holder the Postgres
// claim returns. Kept beside the claim rather than in db_instances.go because that trailing
// column exists only here.
func scanInstanceWithPrevHolder(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, sql.NullString, error) {
	var r dbgen.ProcessInstance
	var prev sql.NullString
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.ErrorMessage,
		&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
		&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorInternal, &r.ExternalData, &r.EngineState, &r.Task,
		&r.ErrorCode, &r.LeaseEpoch, &r.TaskEpoch, &r.ParentTaskEpoch,
		&r.ExternalWorkerID, &r.ExternalLeaseExpiresAt, &r.ExternalClaimEpoch, &r.Objects,
		&r.NextReplayable, &r.ErrorData, &r.RootID,
		&prev,
	)
	return r, prev, err
}

// MarkExternalClaimLost records that an only_once task's holder let its claim lapse without
// answering, INSTEAD of handing the work out again. wake_at moves to now (never later than a
// deadline already set) so the engine's next poll turns the marker into external.lost --
// without it a task with no timeout would sit unclaimable forever, with nothing reporting why.
func (db *DB) MarkExternalClaimLost(ctx context.Context, instanceID string, taskEpoch int64) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		var externalData string
		err := raw.QueryRowContext(ctx,
			`SELECT external_data FROM process_instances WHERE id = ?`+db.forUpdate(), instanceID).
			Scan(&externalData)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("external task: %w", ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("lock instance: %w", err)
		}
		marked, err := withExternalLost(externalData)
		if err != nil {
			return err
		}
		now := nowMillis()
		res, err := raw.ExecContext(ctx,
			`UPDATE process_instances
			   SET external_data = ?, wake_at = ?, updated_at = ?,
			       external_worker_id = NULL, external_lease_expires_at = NULL,
			       external_claim_epoch = external_claim_epoch + 1
			 WHERE id = ? AND task_epoch = ? AND wait_state = 'external'`,
			marked, now, now, instanceID, taskEpoch)
		if err != nil {
			return fmt.Errorf("mark external claim lost: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("task is no longer parked on this arming: %w", ErrConflict)
		}
		return nil
	})
}

// ClaimExternalTaskDirect puts a claim on one row by id, bypassing the queue predicate. It
// exists for tests that need a holder on a task ClaimExternalTasks would not offer — an
// already-due one, for instance, where the point is the holder rather than how it was granted.
// It writes exactly the three claim columns, so it cannot prove a property by touching more.
func (db *DB) ClaimExternalTaskDirect(ctx context.Context, instanceID, workerID string, leaseDur time.Duration) error {
	_, err := db.exec.ExecContext(ctx,
		`UPDATE process_instances
		   SET external_worker_id = ?, external_lease_expires_at = ?,
		       external_claim_epoch = external_claim_epoch + 1
		 WHERE id = ?`,
		workerID, nowMillis()+leaseDur.Milliseconds(), instanceID)
	return err
}
