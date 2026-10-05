package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// claimableWhere reads NONE of the engine's lease columns, which keeps the two claims
// independent. The wake_at clause skips a fired deadline, whose answer could no longer be
// accepted. specs/external-task-queue.md.
const claimableWhere = `phase = 'external' AND status = 'running'
		  AND (external_worker_id IS NULL OR external_lease_expires_at <= ?)
		  AND (wake_at IS NULL OR wake_at > ?)`

// ClaimExternalTasks atomically leases up to limit parked external tasks to workerID, oldest park
// first, filtered by name/version/task (empty/0 = any). It bumps external_claim_epoch (so do release
// and lost-marking); it must touch neither task_epoch (every handle out) nor the engine's lease columns. A candidate
// already holding an answer goes to the engine instead, so fewer than limit may come back.
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
		// `unparked` is UnparkAnsweredExternal inline: the candidates are locked only for this statement.
		query := `
			WITH cand AS (
				SELECT id AS cand_id, external_worker_id AS prev_holder,
				       EXISTS (SELECT 1 FROM process_signals s
				                WHERE s.instance_id = process_instances.id
				                  AND s.task_id = process_instances.task) AS answered
				FROM process_instances
				WHERE ` + where + `
				ORDER BY updated_at ASC, id ASC
				LIMIT ? FOR UPDATE SKIP LOCKED
			), unparked AS (
				UPDATE process_instances SET phase = '', wake_at = NULL, updated_at = ?
				FROM cand
				WHERE process_instances.id = cand.cand_id AND cand.answered
			)
			UPDATE process_instances
			SET external_worker_id = ?, external_lease_expires_at = ?,
			    external_claim_epoch = process_instances.external_claim_epoch + 1
			FROM cand
			WHERE process_instances.id = cand.cand_id AND NOT cand.answered
			RETURNING ` + instanceColumns + `, cand.prev_holder`

		rows, err := db.exec.QueryContext(ctx, query, append(args, limit, now, workerID, leaseExpiry)...)
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
	unparked, err := qtx.UnparkAnsweredExternal(ctx, dbgen.UnparkAnsweredExternalParams{UpdatedAt: now, Ids: string(idsJSON)})
	if err != nil {
		return nil, err
	}
	if len(unparked) > 0 {
		result = slices.DeleteFunc(result, func(inst *model.ProcessInstance) bool { return slices.Contains(unparked, inst.ID) })
		ids = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(unparked, id) })
		if len(ids) == 0 {
			return nil, tx.Commit()
		}
		if idsJSON, err = json.Marshal(ids); err != nil {
			return nil, err
		}
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

// ExternalClaim names one grant, as a claim token does.
type ExternalClaim struct {
	ID         string
	TaskEpoch  int64
	ClaimEpoch int64
}

// RenewVerdict answers one claim. Lost: the grant is no longer this worker's; stop and do NOT
// release, or the new holder's epoch is bumped from under it. Cancelled: stop and DO release.
// specs/external-task-queue.md.
type RenewVerdict string

const (
	RenewRenewed   RenewVerdict = "renewed"
	RenewLost      RenewVerdict = "lost"
	RenewCancelled RenewVerdict = "cancelled"
)

// RenewExternalClaims re-stamps each grant this worker holds to now+leaseDur and answers one
// verdict per claim, in order. It must NOT bump external_claim_epoch (fencing the worker out of
// its own answer) nor clear external_worker_id (the hand-back).
func (db *DB) RenewExternalClaims(ctx context.Context, workerID string, claims []ExternalClaim, leaseDur time.Duration) ([]RenewVerdict, error) {
	out := make([]RenewVerdict, len(claims))
	expiry := nullInt64(nowMillis() + leaseDur.Milliseconds())
	worker := sql.NullString{String: workerID, Valid: true}
	for start := 0; start < len(claims); start += renewChunkSize {
		end := min(start+renewChunkSize, len(claims))
		err := db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
			for i := start; i < end; i++ {
				c := claims[i]
				n, err := qtx.RenewExternalClaim(ctx, dbgen.RenewExternalClaimParams{
					NewExpiry: expiry, ID: c.ID, ExternalWorkerID: worker,
					TaskEpoch: c.TaskEpoch, ClaimEpoch: c.ClaimEpoch,
				})
				if err != nil {
					return err
				}
				if n == 1 {
					out[i] = RenewRenewed
					continue
				}
				status, err := qtx.HeldExternalClaimStatus(ctx, dbgen.HeldExternalClaimStatusParams{
					ID: c.ID, ExternalWorkerID: worker, TaskEpoch: c.TaskEpoch, ClaimEpoch: c.ClaimEpoch,
				})
				switch st := model.Status(status); {
				case errors.Is(err, sql.ErrNoRows):
					out[i] = RenewLost
				case err != nil:
					return err
				case st == model.StatusCancelling || st == model.StatusCancelled:
					out[i] = RenewCancelled
				default:
					out[i] = RenewLost
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReleaseExternalClaim hands a claimed task straight back to the queue (the nack). Unlike an
// expiry it bumps the claim epoch, voiding the releaser's handle at once; claimEpoch must name the
// current grant, so a fenced-out worker cannot release the new holder's work.
func (db *DB) ReleaseExternalClaim(ctx context.Context, instanceID string, taskEpoch, claimEpoch int64) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		n, err := qtx.ReleaseExternalClaim(ctx, dbgen.ReleaseExternalClaimParams{
			ID: instanceID, TaskEpoch: taskEpoch, ClaimEpoch: claimEpoch,
		})
		if err != nil {
			return fmt.Errorf("release external claim: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("claim is no longer held (it may have expired and been re-claimed): %w", ErrConflict)
		}
		// A signal that deferred to this claim is now the answer: the engine's, not the queue's.
		idJSON, err := json.Marshal([]string{instanceID})
		if err != nil {
			return err
		}
		if _, err := qtx.UnparkAnsweredExternal(ctx, dbgen.UnparkAnsweredExternalParams{UpdatedAt: nowMillis(), Ids: string(idJSON)}); err != nil {
			return fmt.Errorf("release external claim: %w", err)
		}
		return nil
	})
}

// scanInstanceWithPrevHolder scans instanceColumns plus the prior holder the Postgres claims
// append; its destination list must track scanInstance's.
func scanInstanceWithPrevHolder(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, sql.NullString, error) {
	var r dbgen.ProcessInstance
	var prev sql.NullString
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.ErrorMessage,
		&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.Phase, &r.SpawnTaskID,
		&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorInternal, &r.EngineState, &r.Task,
		&r.ErrorCode, &r.LeaseEpoch, &r.TaskEpoch, &r.ParentTaskEpoch,
		&r.ExternalWorkerID, &r.ExternalLeaseExpiresAt, &r.ExternalClaimEpoch, &r.Objects,
		&r.NextReplayable, &r.ErrorData, &r.RootID, &r.ExternalInput, &r.ExternalLost,
		&prev,
	)
	return r, prev, err
}

// MarkExternalClaimLost records that an only_once task's holder let its claim lapse unanswered,
// INSTEAD of handing the work out again. wake_at moves to now so the next poll raises
// external.lost; without it a task with no timeout would sit unclaimable forever.
func (db *DB) MarkExternalClaimLost(ctx context.Context, instanceID string, taskEpoch int64) error {
	return db.withTx(ctx, func(qtx *dbgen.Queries, raw dbgen.DBTX) error {
		n, err := qtx.MarkExternalClaimLost(ctx, dbgen.MarkExternalClaimLostParams{
			Now: nullInt64(nowMillis()), ID: instanceID, TaskEpoch: taskEpoch,
		})
		if err != nil {
			return fmt.Errorf("mark external claim lost: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("task is no longer parked on this arming: %w", ErrConflict)
		}
		return nil
	})
}

// ClaimExternalTaskDirect claims one row by id, bypassing the queue predicate, for tests needing a
// holder ClaimExternalTasks would not grant. It writes only the three claim columns.
func (db *DB) ClaimExternalTaskDirect(ctx context.Context, instanceID, workerID string, leaseDur time.Duration) error {
	return db.q.ClaimExternalTaskDirect(ctx, dbgen.ClaimExternalTaskDirectParams{
		ExternalWorkerID:       sql.NullString{String: workerID, Valid: true},
		ExternalLeaseExpiresAt: nullInt64(nowMillis() + leaseDur.Milliseconds()),
		ID:                     instanceID,
	})
}
