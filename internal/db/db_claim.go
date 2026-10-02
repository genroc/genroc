package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// renewChunkSize bounds a renewal transaction's lock set, so a row locked by an in-flight
// advance stalls only its chunk; one bulk UPDATE would block every renewal behind it.
const renewChunkSize = 100

// RenewWorkerLeases re-stamps this worker's leases on ids to now+leaseDur; an unlisted row expires
// with worker_id intact (the hand-back). Success, even for no ids, proves the database reachable.
// Record the instant it RETURNS, never the clock after: a renewal can outlast the staleness margin.
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
		// Terminates only because a renewed row (stamped newExpiry) stops matching the predicate.
		if n < renewChunkSize {
			return toTime(renewedAt), nil
		}
	}
}

// Takeover is the instant (db-clock ms) at or before which a held lease must have expired for a
// claim to take it. An instant, not a flag: re-reading the clock here would let a delayed claim
// take rows this worker is still advancing. specs/lease-fencing.md "The stale-lease gate".
type Takeover int64

// SkipTakeover claims only unheld rows: every stamped lease is nowMillis()+leaseDur, never <= 0.
const SkipTakeover Takeover = 0

// AllowTakeover takes any lease expired as of now. A caller holding leases of its own must pin
// the cutoff with TakeoverBefore instead.
func AllowTakeover() Takeover { return TakeoverBefore(Now()) }

// TakeoverBefore claims rows whose lease expired at or before t, alongside unheld rows.
func TakeoverBefore(t time.Time) Takeover { return Takeover(t.UnixMilli()) }

// ClaimInstances atomically leases up to limit runnable instances to workerID. It is the ONLY
// place lease_epoch moves, fencing out the previous holder. specs/lease-fencing.md.
func (db *DB) ClaimInstances(workerID string, leaseDur time.Duration, limit int, takeover Takeover) ([]*model.ProcessInstance, error) {
	now := nowMillis()
	leaseExpiry := now + leaseDur.Milliseconds()

	// A bound value, not a second query: the SQL text and plan stay identical whatever the cutoff.
	leaseCutoff := int64(takeover)

	ctx := context.Background()

	// The wake_at IS NULL branch excludes 'external': a no-timeout wait is the resolve API's.
	// This list and migration 045's partial index are one predicate written twice -- a status
	// in one but not the other is either never scanned or pure index churn.
	const where = `status IN ('running', 'failing', 'pausing', 'cancelling')
			  AND phase <> 'children'
			  AND (status IN ('failing', 'pausing', 'cancelling')
			       OR wake_at <= ?
			       OR (phase <> 'external' AND wake_at IS NULL))
			  AND (worker_id IS NULL OR lease_expires_at <= ?)`

	if db.dialect == "postgres" {
		// The CTE keeps the prior worker_id: it is the ReclaimedExpired evidence.
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

		// Not autocommit, which takes the session's synchronous_commit and so would put this
		// write out of the durability level's reach (specs/durability-levels.md s4).
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
			r, prevWorker, err := scanInstanceWithPrevHolder(rows)
			if err != nil {
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

	// SQLite cannot reference a FROM table in RETURNING, so select-then-update; its single writer
	// makes that atomic.
	tx, qtx, raw, err := db.beginTxAt(ctx, syncStrict, nil)
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
	if err := qtx.GrantLeases(ctx, dbgen.GrantLeasesParams{
		WorkerID:       sql.NullString{String: workerID, Valid: true},
		LeaseExpiresAt: sql.NullInt64{Int64: leaseExpiry, Valid: true},
		Ids:            string(idsJSON),
	}); err != nil {
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
