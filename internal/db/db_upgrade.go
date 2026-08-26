package db

// Moving instances to another version of their definition. specs/version-compatibility.md s4.

import (
	"context"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// NonTerminalSubtree returns the instance and every descendant still live, oldest first.
// This is the unit an upgrade moves: terminal descendants stay put because their outputs are
// frozen and nothing re-runs them. specs/version-compatibility.md s3c.
func (db *DB) NonTerminalSubtree(ctx context.Context, rootID string) ([]*model.ProcessInstance, error) {
	rows, err := db.q.NonTerminalSubtree(ctx, rootID)
	if err != nil {
		return nil, fmt.Errorf("non-terminal subtree of %q: %w", rootID, err)
	}
	out := make([]*model.ProcessInstance, 0, len(rows))
	for _, r := range rows {
		inst, err := toInstance(dbgen.ProcessInstance(r))
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// InstanceUpgrade is one instance's move: the version it is going to, and the state already
// conformed to that version by internal/validation.
type InstanceUpgrade struct {
	Instance   *model.ProcessInstance // carries ID, the version it is on now, and its task
	ToVersion  int
	NewContext map[string]any
}

// ErrUpgradeBlocked reports that the tree cannot be planned at all: a child sits in a slot the
// target version no longer declares, or under a task that no longer spawns. It is a REFUSAL and
// not a failure -- the definition legitimately says this, and the caller wants to be told which
// child and why, the same way every other refusal names one.
var ErrUpgradeBlocked = fmt.Errorf("upgrade blocked")

// ErrUpgradeStale reports that a row moved between the read that produced the migrated
// state and this write -- its version, task, status or lease changed. The migration was
// computed against something that is no longer there, so the whole batch rolls back.
var ErrUpgradeStale = fmt.Errorf("instance changed while its upgrade was being prepared")

// UpgradeInstances moves every instance in one transaction. It writes state someone else
// decided: this package reads rows and writes rows, and what the migrated state should BE
// is internal/validation's question, asked by the caller that owns the operation. All or nothing: a cluster with
// one immovable member does not move, because a half-migrated tree is a tree whose parent
// and children disagree about which version describes their data.
func (db *DB) UpgradeInstances(ctx context.Context, ups []InstanceUpgrade) error {
	if len(ups) == 0 {
		return nil
	}
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		now := nowMillis()
		for _, up := range ups {
			// A copy carrying the migrated state, so persistState externalizes and
			// reference-counts it exactly as any other write would -- the migrated value can
			// cross the inline/object boundary in either direction.
			staged := *up.Instance
			staged.State = up.NewContext
			cols, err := db.persistState(ctx, qtx, &staged, now)
			if err != nil {
				return fmt.Errorf("stage state for %q: %w", up.Instance.ID, err)
			}

			n, err := qtx.UpgradeInstanceVersion(ctx, dbgen.UpgradeInstanceVersionParams{
				ID:            up.Instance.ID,
				ToVersion:     int64(up.ToVersion),
				FromVersion:   int64(up.Instance.ProcessVersion),
				Task:          up.Instance.Task,
				InputData:     cols.InputData,
				OutputsData:   cols.OutputsData,
				OutputData:    cols.OutputData,
				ErrorInternal: cols.ErrorInternal,
				ErrorData:     cols.ErrorData,
				ExternalData:  cols.ExternalData,
				EngineState:   cols.EngineState,
				Objects:       cols.Objects,
				UpdatedAt:     now,
			})
			if err != nil {
				return fmt.Errorf("upgrade %q: %w", up.Instance.ID, err)
			}
			if n == 0 {
				return fmt.Errorf("%q: %w", up.Instance.ID, ErrUpgradeStale)
			}
		}
		return nil
	})
}
