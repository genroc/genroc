package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// PruneConcludedTrees deletes up to maxTrees process trees whose root concluded before `before`,
// with their signals and logs, and reports how many went. What they held in the object store is
// released by the next CollectObjects. specs/instance-retention.md.
func (db *DB) PruneConcludedTrees(ctx context.Context, before int64, maxTrees int) (int, error) {
	// A buffered log row inserted after its instance is gone would outlive the prune.
	if err := db.flushLogs(); err != nil {
		return 0, err
	}
	candidates, err := db.q.ConcludedRootsBefore(ctx, dbgen.ConcludedRootsBeforeParams{
		Before: before, MaxTrees: int64(maxTrees),
	})
	if err != nil || len(candidates) == 0 {
		return 0, err
	}
	return db.pruneTrees(ctx, candidates, before)
}

// pruneTrees deletes the candidate trees that still qualify once locked.
func (db *DB) pruneTrees(ctx context.Context, candidates []string, before int64) (int, error) {
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return 0, err
	}
	var pruned []string
	// syncStrict: a deletion lost to a power cut is redone by the next sweep.
	err = db.withTxAt(ctx, syncStrict, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		concluded, err := db.lockConcludedTrees(ctx, exec, string(candidatesJSON), before)
		if err != nil {
			return err
		}
		pruned = concluded
		if len(pruned) == 0 {
			return nil
		}
		roots, err := json.Marshal(pruned)
		if err != nil {
			return err
		}
		if err := qtx.DeleteSignalsOfTrees(ctx, string(roots)); err != nil {
			return fmt.Errorf("delete signals: %w", err)
		}
		if err := qtx.DeleteLogsOfTrees(ctx, string(roots)); err != nil {
			return fmt.Errorf("delete logs: %w", err)
		}
		if _, err := qtx.DeleteTrees(ctx, string(roots)); err != nil {
			return fmt.Errorf("delete instances: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("prune concluded trees: %w", err)
	}
	return len(pruned), nil
}

// lockConcludedTrees locks every row of the candidate trees in id order, lockTree's global order,
// and keeps the trees that still qualify under the lock. The candidates were read unlocked, and a
// retry may have revived one since: a single DELETE would re-check only each row, not the tree.
func (db *DB) lockConcludedTrees(ctx context.Context, exec dbgen.DBTX, candidatesJSON string, before int64) ([]string, error) {
	rows, err := exec.QueryContext(ctx, `SELECT id, root_id, status, updated_at FROM process_instances
		WHERE root_id IN (SELECT value FROM json_each(?)) ORDER BY id`+db.forUpdate(), candidatesJSON)
	if err != nil {
		return nil, fmt.Errorf("lock trees: %w", err)
	}
	defer rows.Close()
	type tree struct{ rootOld, settled bool }
	trees := map[string]*tree{}
	var order []string
	for rows.Next() {
		var id, root, status string
		var updatedAt sql.NullInt64
		if err := rows.Scan(&id, &root, &status, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan tree row: %w", err)
		}
		t := trees[root]
		if t == nil {
			t = &tree{settled: true}
			trees[root] = t
			order = append(order, root)
		}
		terminal := model.Status(status).Terminal()
		t.settled = t.settled && terminal
		if id == root {
			t.rootOld = terminal && updatedAt.Valid && updatedAt.Int64 < before
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var concluded []string
	for _, root := range order {
		if t := trees[root]; t.rootOld && t.settled {
			concluded = append(concluded, root)
		}
	}
	return concluded, nil
}
