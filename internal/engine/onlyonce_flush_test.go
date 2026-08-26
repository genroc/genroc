package engine

// The at-most-once bracket: below `strict` a claim's own commit is not flushed, so the
// engine flushes once per claim batch when something in it is about to run an only_once
// task, and again after the write that records the result.
//
// Keyed on the CLAIMED task, because that is what the row records and the row is all
// recovery can read. specs/durability-levels.md s4.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// flushes reads the in-process counter, not durability_marker: the row only moves on
// SQLite, so a bracket test built on it would assert nothing at all on Postgres.
func flushes(t *testing.T, database *db.DB) int64 {
	t.Helper()
	return database.FlushCount()
}

func TestOnlyOnce_ClaimAndResultAreHardened(t *testing.T) {
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	_, hits := interruptedFixture(t, database, "bracket", model.StatusRunning, nil)

	before := flushes(t, database)
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the only_once action ran %d times, want 1", got)
	}
	// Two: one for the batch before anything in it runs, one after the result is written.
	if got := flushes(t, database) - before; got != 2 {
		t.Fatalf("only_once claim+result caused %d flushes, want 2", got)
	}
}

func TestOnlyOnce_OneFlushPerBatchNotPerInstance(t *testing.T) {
	// The opening half is a property of the batch: prefix durability means one flush
	// hardens every claim behind it, so three only_once instances claimed together must
	// still cost one. Per-instance would multiply the fsync that dominates this workload.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	for i := 0; i < 3; i++ {
		interruptedFixture(t, database, "batch", model.StatusRunning, nil)
	}
	before := flushes(t, database)
	n, err := eng.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 3 {
		t.Fatalf("advanced %d instances, want 3 in one batch", n)
	}
	// 1 for the batch + 1 per instance's result.
	if got := flushes(t, database) - before; got != 4 {
		t.Fatalf("3 only_once instances in one batch caused %d flushes, want 4 (1 batch + 3 results)", got)
	}
}

func TestOnlyOnce_NoFlushWithoutTheFlag(t *testing.T) {
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)
	spawnFixture(t, database, "no-bracket")

	before := flushes(t, database)
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := flushes(t, database) - before; got != 0 {
		t.Fatalf("a batch with no only_once task caused %d flushes, want 0 -- every ordinary advance would pay an fsync", got)
	}
}

func TestOnlyOnce_NoFlushAtStrict(t *testing.T) {
	// At strict the claim's own commit already flushed, so the bracket would be a second
	// fsync for a guarantee already held.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityStrict)
	eng := tickEngine(t, database)
	interruptedFixture(t, database, "bracket-strict", model.StatusRunning, nil)

	before := flushes(t, database)
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := flushes(t, database) - before; got != 0 {
		t.Fatalf("only_once caused %d flushes at strict, want 0", got)
	}
}

func TestOnlyOnce_UnsetFlagFlushes(t *testing.T) {
	// The reason the column stores "replayable" rather than "only_once": Go's zero value
	// and the column default both land on false, and false must mean "flush". An instance
	// built by a caller that never set the flag is the case this protects -- it costs an
	// fsync it may not need, instead of silently losing at-most-once.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	id := spawnFixture(t, database, "unset-flag")
	inst, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	inst.NextReplayable = false // what a create path that forgot leaves behind
	if err := database.UpdateInstance(inst); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}

	before := flushes(t, database)
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := flushes(t, database) - before; got == 0 {
		t.Fatal("an instance whose flag was never set caused no flush; the unsafe direction is the one that loses at-most-once silently")
	}
}

func TestOnlyOnce_FlagIsRederivedOnEveryWrite(t *testing.T) {
	// The denormalisation's one invariant: the flag describes the task the row NAMES. It is
	// recomputed in persist rather than wherever Task is assigned, so a task change cannot
	// leave it behind -- the classic way a denormalised column goes quietly wrong.
	//
	// The instance has to still be RUNNABLE afterwards for this to mean anything: `goto: end`
	// completes at the task it was on, leaving Task naming the only_once task forever, and a
	// completed instance is never claimed again. So the only_once task is followed by a delay,
	// which parks the instance at a task that is replayable.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	onlyOnce := true
	process := fmt.Sprintf("rederive-%d", time.Now().UnixNano())
	def := &model.ProcessDefinition{Name: process, Tasks: []*model.Task{
		{
			ID:       "charge",
			Action:   &model.Action{Type: model.ActionTypeFetch, URL: srv.URL},
			OnlyOnce: &onlyOnce,
			Switch:   model.SwitchMap{{Goto: "$wait"}},
		},
		{
			ID:     "wait",
			Action: &model.Action{Type: model.ActionTypeDelay, DelaySpec: model.DelaySpec{For: "1h"}},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		},
	}}
	if err := database.SaveDefinition(def, 1, nil, process+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("%s-i", process)
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "charge", State: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Task != "wait" {
		t.Fatalf("instance is at %q (status %q), want parked at \"wait\"; this asserts nothing otherwise", got.Task, got.Status)
	}
	if !got.NextReplayable {
		t.Fatal("after moving from an only_once task to a replayable one the row still says not-replayable; " +
			"the flag is stale, and every later claim pays an fsync it does not owe")
	}
}

func TestOnlyOnce_FlagSurvivesSpawnAndRetry(t *testing.T) {
	// Two writes build UpdateInstanceParams by hand instead of going through
	// updateInstanceParams, so they do not get the flag unless someone remembers. Forgetting
	// zeroes it to "needs flush", which is safe and therefore silent: no test fails, the
	// guarantee still holds, and every parked parent and every revived instance quietly pays
	// an fsync forever. That is what this pins.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	// A spawning parent parks in SpawnChildrenAndWait, then is claimed again to collect.
	id := spawnFixture(t, database, "flag-spawn")
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	parked, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if parked.WaitState != model.WaitStateWaiting {
		t.Fatalf("parent is in wait_state %q, want waiting; the spawn write is what this covers", parked.WaitState)
	}
	if !parked.NextReplayable {
		t.Error("a parent parked by SpawnChildrenAndWait lost its replayable flag; its collect claim now pays an fsync it does not owe")
	}

	// RetryProcess revives without moving Task, so the stored flag still describes it.
	// A registered definition is required: revival loads one.
	retryID := spawnFixture(t, database, "flag-retry")
	failed, err := database.GetInstance(retryID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	failed.Status = model.StatusFailed
	failed.ErrorMessage = "boom"
	failed.NextReplayable = true
	if err := database.UpdateInstance(failed); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if err := database.RetryProcess(context.Background(), failed.ID, false); err != nil {
		t.Fatalf("RetryProcess: %v", err)
	}
	revived, err := database.GetInstance(failed.ID)
	if err != nil {
		t.Fatalf("GetInstance (revived): %v", err)
	}
	if !revived.NextReplayable {
		t.Error("a revived instance lost its replayable flag; every retry now pays an fsync it does not owe")
	}
}

// onlyOnceBehindSwitch registers a definition whose only_once action sits behind a
// call-less switch, so reaching it means collapsing a chain. Returns the instance id and a
// counter of how many times the action's endpoint was hit.
func onlyOnceBehindSwitch(t *testing.T, database *db.DB, name string) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	onlyOnce := true
	process := fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	def := &model.ProcessDefinition{Name: process, Tasks: []*model.Task{
		{ID: "gate", Switch: model.SwitchMap{{Goto: "$charge"}}}, // call-less: collapses inline
		{
			ID:       "charge",
			Action:   &model.Action{Type: model.ActionTypeFetch, URL: srv.URL},
			OnlyOnce: &onlyOnce,
			Switch:   model.SwitchMap{{Goto: model.GotoEnd}},
		},
	}}
	if err := database.SaveDefinition(def, 1, nil, process+"-h", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := process + "-i"
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: process, ProcessVersion: 1,
		Task: "gate", State: map[string]any{}, Status: model.StatusRunning,
		NextReplayable: true, // "gate" is a switch
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return id, &hits
}

func TestOnlyOnce_NeverRunsInTheAdvanceThatMovedToIt(t *testing.T) {
	// The row's `task` is all recovery has. If a collapsed chain ran the only_once action
	// while the row still named the switch it started from, a crash mid-request would look
	// exactly like "never started" and prepareAdvance would re-run it -- the one thing
	// at-most-once forbids, and silently, with no error code raised.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	id, hits := onlyOnceBehindSwitch(t, database, "inline-bailout")

	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the only_once action ran %d times in the advance that moved to it; a crash "+
			"mid-request would have been indistinguishable from never starting", n)
	}
	if got.Task != "charge" {
		t.Fatalf("instance checkpointed at %q, want \"charge\" -- the row has to NAME the "+
			"only_once task before it runs, or recovery cannot see it", got.Task)
	}
	if got.NextReplayable {
		t.Error("the checkpoint left next_replayable true, so the next claim will not harden it")
	}

	// The next claim is the protected case: the row names the task, so it runs exactly once.
	if _, err := eng.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the only_once action ran %d times across both ticks, want exactly 1", n)
	}
}

func TestOnlyOnce_InterruptedBehindASwitchDoesNotRerun(t *testing.T) {
	// The end-to-end shape: a worker dies while the row names the switch. With the bail-out,
	// that worker cannot have executed the action -- it would have checkpointed first -- so
	// running it now is correct and must happen exactly once.
	database := openTestDB(t)
	database.SetDurability(db.DurabilityOnlyOnce)
	eng := tickEngine(t, database)

	id, hits := onlyOnceBehindSwitch(t, database, "inline-interrupted")
	interrupt(t, database, id) // a previous owner claimed it and vanished

	for i := 0; i < 3; i++ {
		if _, err := eng.Tick(context.Background()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the only_once action ran %d times, want exactly 1", n)
	}
	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusCompleted {
		t.Errorf("status %q, want completed (error %q / %q)", got.Status, got.ErrorCode, got.ErrorMessage)
	}
}
