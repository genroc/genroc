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

// RenewWorkerLeases re-stamps this worker's leases on the listed instances (its held
// set) to now+leaseDur, in small chunks so an advance's row lock stalls only its chunk.
// An unlisted row expires with worker_id intact — the hand-back path. An empty list
// still runs one no-op chunk, so success always proves the database was reachable.
//
// It returns the instant the expiries were derived from; record that, never the clock
// after the call — the renewal can outlast the margin a staleness check leaves itself.
func (db *DB) RenewWorkerLeases(workerID string, ids []string, leaseDur time.Duration) (time.Time, error) {
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return time.Time{}, err
	}
	if ids == nil {
		idsJSON = []byte("[]") // json_each needs an array, not null
	}
	renewedAt := nowMillis()
	newExpiry := sql.NullInt64{Int64: renewedAt + leaseDur.Milliseconds(), Valid: true}
	worker := sql.NullString{String: workerID, Valid: true}
	for {
		n, err := db.q.RenewWorkerLeasesChunk(context.Background(), dbgen.RenewWorkerLeasesChunkParams{
			NewExpiry: newExpiry,
			Ids:       string(idsJSON),
			WorkerID:  worker,
			ChunkSize: renewChunkSize,
		})
		if err != nil {
			return time.Time{}, err
		}
		// Fewer than a full chunk renewed → no eligible leases remain. Renewed rows
		// are stamped to newExpiry, so they no longer match the chunk's predicate;
		// the eligible set shrinks each pass, guaranteeing termination.
		if n < renewChunkSize {
			return toTime(renewedAt), nil
		}
	}
}

// Takeover is how far back a claim may reach for rows some worker still holds: such a row
// is claimable only if its lease expired at or before this instant (db-clock millis).
// A worker that has just discovered it was not running passes SkipTakeover for a while, so
// it does not steal rows from co-resident workers that froze with it and are about to
// repair their own leases. See Engine.leaseGate.
//
// It is an instant supplied by the caller rather than a flag the claim resolves against its
// own clock, and that is the whole point: the caller decides from evidence that ages (the
// pump pins it to the moment it last proved its own leases alive), so anything scheduled in
// between — a GC pause, a descheduled goroutine — delays the claim without widening what it
// may take. Re-reading the clock here would let that delay re-claim rows this worker is
// still advancing — dooming the in-flight advance's write for nothing (the fence refuses
// it as ErrLeaseLost).
type Takeover int64

// SkipTakeover claims only rows with no worker_id at all: no stamped lease can be at or
// below zero (they are all nowMillis()+leaseDur), so the lease_expires_at branch of the
// claim predicate never fires.
const SkipTakeover Takeover = 0

// AllowTakeover is ordinary claiming from a caller with nothing to protect: any lease
// expired as of now is fair game. Callers holding leases of their own must pin the cutoff
// with TakeoverBefore instead.
func AllowTakeover() Takeover { return TakeoverBefore(Now()) }

// TakeoverBefore claims rows whose lease expired at or before t, alongside unheld rows.
func TakeoverBefore(t time.Time) Takeover { return Takeover(t.UnixMilli()) }

// ClaimInstances atomically leases up to limit runnable instances to workerID.
// PostgreSQL appends FOR UPDATE SKIP LOCKED so concurrent workers never block;
// SQLite's single-writer model needs no such clause. wait_state <> 'waiting'
// excludes parents suspended for children; both ” (none) and 'collecting' are claimable.
//
// The ONLY place lease_epoch moves: a claim is a grant, and the bump fences out
// whoever held the previous one. specs/lease-fencing.md.
func (db *DB) ClaimInstances(workerID string, leaseDur time.Duration, limit int, takeover Takeover) ([]*model.ProcessInstance, error) {
	now := nowMillis()
	leaseExpiry := now + leaseDur.Milliseconds()

	// The takeover mode is a bound value, not a second query: the SQL text, its placeholder
	// count and its plan stay identical whatever the cutoff — the partial runnable index is
	// walked exactly as before, just with a more selective filter.
	leaseCutoff := int64(takeover)

	ctx := context.Background()

	// The two `?` are now (timer) and leaseCutoff (pinned by the caller — see Takeover).
	// 'paused' is live-but-not-advanced and keeps wake_at; 'failing'/'pausing' ignore theirs.
	// The wake_at IS NULL branch excludes 'external': a no-timeout wait is the resolve API's.
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
			SET worker_id = ?, lease_expires_at = ?,
			    lease_epoch = process_instances.lease_epoch + 1
			FROM cand
			WHERE process_instances.id = cand.cand_id
			RETURNING ` + instanceColumns + `, cand.prev_worker`

		// In a transaction rather than autocommit: autocommit would take the session's
		// synchronous_commit and the durability level could never reach this write. A claim
		// is ordinary progress -- what makes it evidence is the only_once bracket, which
		// flushes around the execute (specs/durability-levels.md s4).
		tx, _, raw, err := db.beginTxAt(ctx, syncStrict, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()

		rows, err := raw.QueryContext(ctx, query, now, leaseCutoff, limit, workerID, leaseExpiry)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []*model.ProcessInstance
		for rows.Next() {
			// Destinations must match instanceColumns, plus prev_worker. This list cannot use
			// scanInstance because of that trailing column -- so a column added there has to
			// be added here too.
			var r dbgen.ProcessInstance
			var prevWorker sql.NullString
			if err := rows.Scan(
				&r.ID, &r.ProcessName, &r.ProcessVersion, &r.ParentID,
				&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.Error,
				&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
				&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorData, &r.ExternalData, &r.EngineState, &r.Task,
				&r.ErrorCode, &r.LeaseEpoch, &r.TaskEpoch, &r.ParentTaskEpoch,
				&r.ExternalWorkerID, &r.ExternalLeaseExpiresAt, &r.ExternalClaimEpoch, &r.Objects,
				&r.NextReplayable,
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
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
		return result, tx.Commit()
	}

	// SQLite can't reference a FROM table in RETURNING, so it selects-then-updates
	// in one transaction. Its single-writer model makes that atomic (no FOR UPDATE);
	// the selected worker_id is the prior owner, before we overwrite it.
	tx, _, raw, err := db.beginTxAt(ctx, syncStrict, nil)
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
		`UPDATE process_instances SET worker_id = ?, lease_expires_at = ?,
		    lease_epoch = lease_epoch + 1
		 WHERE id IN (SELECT value FROM json_each(?))`,
		workerID, leaseExpiry, string(idsJSON)); err != nil {
		return nil, err
	}

	// Reflect the new lease state on the returned instances; the epoch was scanned
	// before the UPDATE, so the new grant is old+1 (atomic under the single writer).
	newLease := toTime(leaseExpiry)
	w := workerID
	for _, inst := range result {
		inst.WorkerID = &w
		inst.LeaseExpiresAt = &newLease
		inst.LeaseEpoch++
	}
	return result, tx.Commit()
}
