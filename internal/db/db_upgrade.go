package db

// Moving instances to another version of their definition. specs/version-compatibility.md s4.

import (
	"context"
	"fmt"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// NonTerminalSubtree returns the instance and every live descendant, oldest first: the unit an
// upgrade moves, since terminal outputs are frozen. specs/version-compatibility.md s3c.
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

// ErrUpgradeBlocked: a child sits in a slot the target no longer declares, or under a task that
// no longer spawns. A REFUSAL, not a failure: the caller names which child and why.
var ErrUpgradeBlocked = fmt.Errorf("upgrade blocked")

// ErrUpgradeStale: a row's version, task, status or lease moved since the read the migration was
// computed from; the whole batch rolls back.
var ErrUpgradeStale = fmt.Errorf("instance changed while its upgrade was being prepared")

// UpgradeInstances moves every instance in one transaction, all or nothing: a half-migrated tree's
// parent and children disagree on which version describes their data. What the migrated state
// should BE is internal/validation's to decide; this only writes it.
func (db *DB) UpgradeInstances(ctx context.Context, ups []InstanceUpgrade) error {
	if len(ups) == 0 {
		return nil
	}
	return db.withTx(ctx, func(qtx *dbgen.Queries, _ dbgen.DBTX) error {
		now := nowMillis()
		for _, up := range ups {
			// A copy, so persistState cuts and claims the migrated value like any write: it can
			// cross the inline/object boundary either way.
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
				ExternalInput: cols.ExternalInput,
				ExternalLost:  boolToInt(cols.ExternalLost),
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
