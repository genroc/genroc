package engine

// Engine-side behaviour of the lease fence (specs/lease-fencing.md, Testing §4–§5):
// what a worker does when its write is refused, how a self-reclaimed row hands back,
// and that the only_once verdict survives a genuine takeover. The epoch mechanics and
// the per-entry-point fence live in internal/db/dbtest/lease_epoch_test.go.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/errcode"
	"genroc/internal/model"
)

func hasLogEvent(t *testing.T, database *db.DB, instID, event string) bool {
	t.Helper()
	logs, _, err := database.ListLogs(instID, db.LogQuery{})
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	for _, l := range logs {
		if l.Event == event {
			return true
		}
	}
	return false
}

// §4.1–§4.3, §4.9 — a fence loss drops the outcome: no failInstance (writing a failure
// is the clobber under another name), no worker error, and a lease_lost entry on the
// instance as the only trace. Driven through runAdvance directly, which is also the
// manual-tick path (§4.9: no pump, no marker assumptions).
func TestRunAdvance_LeaseLostDropsOutcome(t *testing.T) {
	database := openTestDB(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	id := seedInstance(t, database, "fenceloss", srv.URL)
	eng := New(database, 0 /* manual */, 2, true, time.Minute, time.Minute, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	stale, err := database.ClaimInstances(eng.WorkerID(), 20*time.Millisecond, 1, db.AllowTakeover())
	if err != nil || len(stale) != 1 {
		t.Fatalf("claim: err=%v count=%d", err, len(stale))
	}
	db.AdvanceClock(time.Second) // the lease lapses...
	if thief, err := database.ClaimInstances("thief", time.Hour, 1, db.AllowTakeover()); err != nil || len(thief) != 1 {
		t.Fatalf("takeover claim: err=%v count=%d", err, len(thief)) // ...and the row is re-granted
	}

	// The stale advance completes its task and tries to write the outcome.
	if err := eng.runAdvance(context.Background(), stale[0]); err != nil {
		t.Fatalf("runAdvance must swallow a lost lease, not report a worker error: %v", err)
	}

	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status.Terminal() {
		t.Fatalf("the dropped outcome landed anyway: status %q", got.Status)
	}
	if got.ErrorCode != "" || got.Error != "" {
		t.Fatalf("a lost lease must not be converted into an instance failure: code=%q error=%q", got.ErrorCode, got.Error)
	}
	if !hasLogEvent(t, database, id, model.EventLeaseLost) {
		t.Fatal("no lease_lost entry; the abandoned attempt left no trace at all")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected the stale advance to have executed once, got %d", hits.Load())
	}
}

// §4.4, §4.7, §3.4 end to end in one worker — the hand-back path. A self-reclaim skips
// the dispatch; the doomed advance finishes and is fenced out; the row leaves the held
// set, expires with its worker_id intact, and the NEXT claim advances it from its last
// checkpoint to completion. This is the test that fails if the held set keeps renewing
// rows it no longer advances (a stuck row) or if anything clears worker_id on the way.
func TestSelfReclaim_RowHandsBackAndCompletes(t *testing.T) {
	database := openTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	id := seedInstance(t, database, "handback", srv.URL)
	eng := New(database, 0 /* manual */, 2, true, time.Minute, time.Minute, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A claim with an advance still in flight (marker + held set, as dispatch leaves them).
	first, err := database.ClaimInstances(eng.WorkerID(), 20*time.Millisecond, 1, db.AllowTakeover())
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: err=%v count=%d", err, len(first))
	}
	eng.held.Store(id, struct{}{})
	eng.inflight.Store(id, struct{}{})

	// The lease lapses under it and the pump re-claims the row (a self-reclaim):
	// dispatch must skip — no second advance while the first still runs.
	db.AdvanceClock(time.Second)
	reclaimed, err := database.ClaimInstances(eng.WorkerID(), time.Minute, 1, db.AllowTakeover())
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("self-reclaim: err=%v count=%d", err, len(reclaimed))
	}
	var wg sync.WaitGroup
	if eng.dispatch(context.Background(), &wg, reclaimed[0], false) {
		t.Fatal("dispatched a second advance over an instance still in flight")
	}

	// The doomed advance finishes: fenced out, outcome dropped, held entry gone.
	if err := eng.runAdvance(context.Background(), first[0]); err != nil {
		t.Fatalf("doomed advance: %v", err)
	}
	if _, still := eng.held.Load(id); still {
		t.Fatal("the doomed advance left the row in the held set; the renewer would keep it alive forever")
	}

	// Renewals no longer cover the row, so the reclaim's lease expires on its own —
	// with the takeover evidence intact.
	if err := eng.renewLeases(); err != nil {
		t.Fatalf("renewLeases: %v", err)
	}
	db.AdvanceClock(2 * time.Minute)

	next, err := database.ClaimInstances(eng.WorkerID(), time.Minute, 1, db.AllowTakeover())
	if err != nil || len(next) != 1 {
		t.Fatalf("hand-back claim: err=%v count=%d", err, len(next))
	}
	if !next[0].ReclaimedExpired {
		t.Fatal("the hand-back claim did not observe the takeover; an only_once task here would silently re-run")
	}
	if err := eng.runAdvance(context.Background(), next[0]); err != nil {
		t.Fatalf("final advance: %v", err)
	}
	if got, _ := database.GetInstance(id); got.Status != model.StatusCompleted {
		t.Fatalf("the handed-back row never completed: %q (%s: %s) — a stuck row", got.Status, got.ErrorCode, got.Error)
	}
}

// seedOnlyOnceInstance is seedInstance with the task marked only_once.
func seedOnlyOnceInstance(t *testing.T, database *db.DB, prefix, url string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	yes := true
	tasks := []*model.Task{{
		ID:       "charge",
		OnlyOnce: &yes,
		Action:   &model.Action{Type: model.ActionTypeFetch, URL: url},
		Switch:   model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: name, Tasks: tasks}, 1, nil, name+"-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("%s-i-%d", prefix, time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: name, ProcessVersion: 1,
		Task: tasks[0].ID, ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return id
}

// §4.8, §5.1 — the multi-worker hazard the fence exists for, on the task class where it
// matters most. Worker A freezes mid-only_once call (renewer parked, pump saturated so
// the gate never runs); worker B takes the row over, observes the interruption, and
// fails the instance with only_once.interrupted — the endpoint is hit exactly once. When
// A wakes and finishes, its "completed" is refused: the final row is B's verdict, not
// A's stale success for a charge whose fate was already adjudicated.
func TestFence_TakeoverVerdictOutlivesTheFrozenWorker(t *testing.T) {
	database := openTestDB(t)

	var hits atomic.Int32
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	defer close(release)

	id := seedOnlyOnceInstance(t, database, "frozen-oo", srv.URL)

	// A: 200ms lease, renewer parked an hour out, one slot — saturated, so neither the
	// renewer nor the gate can save the lease while the call blocks.
	engA := New(database, 10*time.Millisecond, 1, true, 200*time.Millisecond, time.Hour, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctxA, cancelA := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelA()
	doneA := make(chan struct{})
	go func() { engA.Run(ctxA); close(doneA) }()

	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		t.Fatal("task never went in-flight")
	}

	// The lease lapses under A; B takes over and adjudicates the interruption.
	db.AdvanceClock(time.Second)
	engB := New(database, 0 /* manual */, 1, true, time.Minute, time.Minute, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := engB.Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("B's tick: n=%d err=%v", n, err)
	}

	got, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Status != model.StatusFailed || got.ErrorCode != string(errcode.OnlyOnceInterrupted) {
		t.Fatalf("B's verdict: status=%q code=%q, want failed/%s", got.Status, got.ErrorCode, errcode.OnlyOnceInterrupted)
	}
	if hits.Load() != 1 {
		t.Fatalf("only_once endpoint hit %d times; B must adjudicate, never re-run", hits.Load())
	}

	// A wakes, finishes the call, and tries to write "completed" over B's verdict.
	select {
	case release <- struct{}{}:
	default:
	}
	deadline := time.Now().Add(10 * time.Second)
	for !hasLogEvent(t, database, id, model.EventLeaseLost) {
		if time.Now().After(deadline) {
			t.Fatal("A's refused write never surfaced as a lease_lost entry")
		}
		time.Sleep(20 * time.Millisecond)
	}

	final, err := database.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if final.Status != model.StatusFailed || final.ErrorCode != string(errcode.OnlyOnceInterrupted) {
		t.Fatalf("the frozen worker's stale write clobbered the takeover verdict: status=%q code=%q", final.Status, final.ErrorCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("endpoint hit %d times across the whole incident, want exactly 1", hits.Load())
	}

	cancelA()
	select {
	case <-doneA:
	case <-time.After(10 * time.Second):
		t.Fatal("engine A did not shut down")
	}
}

// §5.5 — the payoff case, pairing the takeover test above: the same freeze on a single
// worker, but nobody takes the row, so the gate's repair keeps the lease alive and the
// only_once task completes — one execution, no interrupted verdict to adjudicate.
func TestLeaseGate_RepairSavesOnlyOnceThroughFreeze(t *testing.T) {
	database := openTestDB(t)

	var hits atomic.Int32
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	defer close(release)

	id := seedOnlyOnceInstance(t, database, "repair-oo", srv.URL)

	// Renewer an hour out but a free slot, so the pump keeps polling and the gate can
	// repair — the same setup as TestLeaseGate_SurvivesFrozenHost, on the task class
	// where losing the lease would not merely re-run work but fail the process.
	eng := New(database, 50*time.Millisecond, 2, true, 10*time.Second, time.Hour, LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		t.Fatal("task never went in-flight")
	}

	db.AdvanceClock(2 * time.Hour)     // the host "sleeps" past every lease
	time.Sleep(300 * time.Millisecond) // give the gate a few cycles to repair
	select {
	case release <- struct{}{}:
	default:
	}

	waitTerminal(t, database, id, 10*time.Second)
	if got, _ := database.GetInstance(id); got.Status != model.StatusCompleted {
		t.Fatalf("with the repair the lease was never lost, so the instance must complete: %q (%s: %s)", got.Status, got.ErrorCode, got.Error)
	}
	if hits.Load() != 1 {
		t.Fatalf("only_once endpoint hit %d times across the freeze, want exactly 1", hits.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not shut down")
	}
}
