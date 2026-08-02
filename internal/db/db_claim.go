package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// renewChunkSize bounds how many leases a single renewal transaction touches.
// Small chunks keep each transaction's lock set tiny, so a row locked by an
// in-flight advance stalls only its chunk rather than every lease at once (a
// single bulk UPDATE would block all renewals behind one contended row).
const renewChunkSize = 100

// RenewWorkerLeases re-stamps all of this worker's leases to now+leaseDur, in
// small chunks (soonest-to-expire first). Each chunk is its own transaction, so
// renewals make progress even while in-flight advances hold row locks.
func (db *DB) RenewWorkerLeases(workerID string, leaseDur time.Duration) error {
	newExpiry := sql.NullInt64{Int64: nowMillis() + leaseDur.Milliseconds(), Valid: true}
	worker := sql.NullString{String: workerID, Valid: true}
	for {
		n, err := db.q.RenewWorkerLeasesChunk(context.Background(), dbgen.RenewWorkerLeasesChunkParams{
			NewExpiry: newExpiry,
			WorkerID:  worker,
			ChunkSize: renewChunkSize,
		})
		if err != nil {
			return err
		}
		// Fewer than a full chunk renewed → no eligible leases remain. Renewed rows
		// are stamped to newExpiry, so they no longer match the chunk's predicate;
		// the eligible set shrinks each pass, guaranteeing termination.
		if n < renewChunkSize {
			return nil
		}
	}
}

// Takeover selects whether a claim may pick up rows whose lease expired under some
// worker, or only rows nobody holds at all. A worker that has just discovered it was not
// running — a suspended host, a stalled process — passes SkipTakeover for a while: its own
// leases have lapsed on paper, so have those of any co-resident worker that froze with it,
// and every one of them is about to be repaired by its owner. Declining to take them for
// that window costs nothing (unowned work still flows) and avoids stealing rows whose
// owner is alive and one renewal away from proving it. See Engine.leaseGate.
type Takeover bool

const (
	AllowTakeover Takeover = true  // ordinary claiming: an expired lease is fair game
	SkipTakeover  Takeover = false // claim only rows with no worker_id at all
)

// ClaimInstances atomically leases up to limit runnable instances to workerID.
// PostgreSQL appends FOR UPDATE SKIP LOCKED so concurrent workers never block;
// SQLite's single-writer model needs no such clause. wait_state <> 'waiting'
// excludes parents suspended for children; both ” (none) and 'collecting' are claimable.
func (db *DB) ClaimInstances(workerID string, leaseDur time.Duration, limit int, takeover Takeover) ([]*model.ProcessInstance, error) {
	now := nowMillis()
	leaseExpiry := now + leaseDur.Milliseconds()

	// SkipTakeover is a bound value, not a second query: the lease cutoff drops to 0,
	// which no stamped lease can be at or below (they are all nowMillis()+leaseDur), so
	// `lease_expires_at <= ?` never fires and only the `worker_id IS NULL` branch of the
	// predicate can match. The SQL text, its placeholder count and its plan stay identical
	// in both modes — the partial runnable index is walked exactly as before, just with a
	// more selective filter.
	leaseCutoff := now
	if takeover == SkipTakeover {
		leaseCutoff = 0
	}

	ctx := context.Background()

	// Shared claimable predicate. The two `?` are `now` (retry/delay/timeout timer) and
	// leaseCutoff (lease expiry — `now`, or 0 under SkipTakeover).
	//
	// 'paused' is absent: a paused instance is live work that is simply not advanced
	// automatically, so it is never claimed until ResumeProcess puts it back to
	// 'running'. It keeps its wake_at meanwhile — timers run while paused, so a delay
	// that elapsed during the pause fires on the next tick after resuming.
	//
	// A doomed instance ('failing') is drained immediately, ignoring wake_at: it will
	// never run its pending task again, so there is no point waiting out a delay or
	// retry-backoff timer before settling it. 'pausing' gets the same treatment, but
	// only as crash recovery: a pause normally lands when the worker holding the
	// instance writes its task (the CASE in UpdateInstance), and a claim is reached
	// only if that worker died first, leaving the row draining with an expired lease.
	//
	// wait_state='external' (parked on an external task) is claimable only when its
	// timeout timer is due (`wake_at <= ?`): a no-timeout external wait has wake_at NULL
	// and must NOT be claimed (it waits for the resolve API, which un-parks it by setting
	// wait_state='' + wake_at=NULL — caught by the last branch). Normal runnable rows
	// (any non-external wait_state) with no timer are always claimable.
	const where = `status IN ('running', 'failing', 'pausing')
			  AND wait_state <> 'waiting'
			  AND (status IN ('failing', 'pausing')
			       OR wake_at <= ?
			       OR (wait_state <> 'external' AND wake_at IS NULL))
			  AND (worker_id IS NULL OR lease_expires_at <= ?)`

	if db.dialect == "postgres" {
		// One statement: a CTE captures the prior worker_id (to flag lease takeovers)
		// and FOR UPDATE SKIP LOCKED lets concurrent workers avoid blocking.
		query := `
			WITH cand AS (
				SELECT id AS cand_id, worker_id AS prev_worker
				FROM process_instances
				WHERE ` + where + `
				ORDER BY created_at ASC, id ASC
				LIMIT ? FOR UPDATE SKIP LOCKED
			)
			UPDATE process_instances
			SET worker_id = ?, lease_expires_at = ?
			FROM cand
			WHERE process_instances.id = cand.cand_id
			RETURNING ` + instanceColumns + `, cand.prev_worker`

		rows, err := db.exec.QueryContext(ctx, query, now, leaseCutoff, limit, workerID, leaseExpiry)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []*model.ProcessInstance
		for rows.Next() {
			var r dbgen.ProcessInstance
			var prevWorker sql.NullString
			if err := rows.Scan(
				&r.ID, &r.ProcessName, &r.ProcessVersion, &r.ParentID,
				&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.Error,
				&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
				&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorData, &r.ExternalData, &r.EngineState, &r.Task,
				&r.ErrorCode,
				&prevWorker,
			); err != nil {
				return nil, err
			}
			inst, err := toInstance(r)
			if err != nil {
				return nil, err
			}
			inst.ReclaimedExpired = prevWorker.Valid && prevWorker.String != ""
			result = append(result, inst)
		}
		return result, rows.Err()
	}

	// SQLite can't reference a FROM table in RETURNING, so it selects-then-updates
	// in one transaction. Its single-writer model makes that atomic (no FOR UPDATE);
	// the selected worker_id is the prior owner, before we overwrite it.
	tx, _, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	selectQ := `SELECT ` + instanceColumns + `
		FROM process_instances
		WHERE ` + where + `
		ORDER BY created_at ASC, id ASC
		LIMIT ?`
	rows, err := raw.QueryContext(ctx, selectQ, now, leaseCutoff, limit)
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
		inst.ReclaimedExpired = inst.WorkerID != nil // prior worker present => takeover
		result = append(result, inst)
		ids = append(ids, inst.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // must close the cursor before the UPDATE on the single connection
	if len(result) == 0 {
		return nil, tx.Commit()
	}

	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE process_instances SET worker_id = ?, lease_expires_at = ?
		 WHERE id IN (SELECT value FROM json_each(?))`,
		workerID, leaseExpiry, string(idsJSON)); err != nil {
		return nil, err
	}

	// Reflect the new lease state on the returned instances.
	newLease := toTime(leaseExpiry)
	w := workerID
	for _, inst := range result {
		inst.WorkerID = &w
		inst.LeaseExpiresAt = &newLease
	}
	return result, tx.Commit()
}
