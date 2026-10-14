package db

import (
	"context"
	"testing"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// The candidates are read unlocked, so a retry can revive a failed root before the prune locks it.
func TestRetention_ARootRevivedAfterTheReadIsKept(t *testing.T) {
	db := openTempSQLite(t, DurabilityStrict)
	ctx := context.Background()
	inst := runningInstance(t, db, "revived")
	inst.Status = model.StatusFailed
	if err := db.UpdateInstance(inst); err != nil {
		t.Fatalf("fail: %v", err)
	}
	before := Now().UnixMilli() + 60_000
	candidates, err := db.q.ConcludedRootsBefore(ctx, dbgen.ConcludedRootsBeforeParams{Before: before, MaxTrees: 10})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}

	inst, err = db.GetInstance("revived")
	if err != nil {
		t.Fatal(err)
	}
	inst.Status = model.StatusRunning // what RetryProcess leaves behind
	if err := db.UpdateInstance(inst); err != nil {
		t.Fatalf("revive: %v", err)
	}

	n, err := db.pruneTrees(ctx, candidates, before)
	if err != nil {
		t.Fatalf("pruneTrees: %v", err)
	}
	if n != 0 {
		t.Fatal("a tree revived after the candidate read was deleted; the prune must re-check under the lock")
	}
	if _, err := db.GetInstance("revived"); err != nil {
		t.Fatalf("revived instance is gone: %v", err)
	}
}
