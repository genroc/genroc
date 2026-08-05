package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// TestGracefulShutdown_ReleasesLeases verifies that a clean shutdown (ctx cancel)
// drains in-flight work and releases the worker's leases. That invariant is what
// keeps a healthy restart quiet: a released lease (worker_id NULL) is reclaimed as
// a clean claim, so the engine never logs a "reclaimed instance with expired lease"
// takeover warning. Such warnings should only appear after a hard crash, where the
// lease is left held.
func TestGracefulShutdown_ReleasesLeases(t *testing.T) {
	database := openTestDB(t)

	// Signals when the task is hit, then blocks until the test releases it, so the
	// task stays in-flight (lease held) right up to shutdown. On shutdown the engine
	// aborts its request client-side (transport.Send returns), so the assertions run
	// without needing the handler to unblock; we close release at the end purely so
	// srv.Close() doesn't wait on the leaked handler goroutine.
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release
	}))
	defer srv.Close()
	defer close(release) // LIFO: runs before srv.Close() so the handler can exit

	processName := fmt.Sprintf("graceful-%d", time.Now().UnixNano())
	tasks := []*model.Task{{
		ID:     "work",
		Action: &model.Action{Type: model.ActionTypeFetch, URL: srv.URL},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: processName, Tasks: tasks}, 1, nil, "graceful-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	instID := fmt.Sprintf("gi-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID:             instID,
		ProcessName:    processName,
		ProcessVersion: 1,
		Task:           tasks[0].ID,
		ContextData:    map[string]any{},
		Status:         model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, time.Millisecond, 10, true /* immediateRetries */, 0, 0 /* default lease */, LogConfig{}, log)

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	// Wait until the engine has claimed the instance and the task is in-flight.
	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("task never went in-flight")
	}

	// Sanity: an in-flight instance holds a lease.
	if got, err := database.GetInstance(instID); err != nil {
		t.Fatalf("GetInstance (in-flight): %v", err)
	} else if got.WorkerID == nil {
		t.Fatal("expected the in-flight instance to hold a lease (worker_id set)")
	}

	// Graceful shutdown: cancel and wait for Run to drain + return.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not shut down within 10s")
	}

	// The drain must have released the lease — otherwise a restart would treat the
	// instance as a takeover and log a spurious warning.
	got, err := database.GetInstance(instID)
	if err != nil {
		t.Fatalf("GetInstance (after shutdown): %v", err)
	}
	if got.WorkerID != nil {
		t.Fatalf("graceful shutdown left a held lease (worker_id=%q); a restart would log a spurious takeover warning", *got.WorkerID)
	}
	if got.LeaseExpiresAt != nil {
		t.Fatal("graceful shutdown left lease_expires_at set; expected it cleared")
	}
}

// TestLeaseGate_RepairsInsteadOfExiting sets up the exact conditions that used to end the
// process: renewal is not happening (the renewer is parked a minute out), the lease is
// tiny, and a task blocks well past it, so the instance's lease expires under an advance
// that is still running. maxConcurrent=2 leaves a free slot for the pump to re-claim it on.
//
// The lease gate turns that into a non-event. It notices the renewal evidence going stale
// one poll before the lease actually dies, repairs the lease itself, and declines
// takeovers meanwhile — so the row is never claimable, the advance is never disturbed, and
// Run keeps pumping to a clean shutdown.
//
// This is also the case a wall-clock-vs-monotonic freeze detector cannot see: no clock is
// shifted here and the host never suspends, the renewer simply is not running.
func TestLeaseGate_RepairsInsteadOfExiting(t *testing.T) {
	database := openTestDB(t)

	const blockFor = time.Second
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(blockFor) // keep the advance in-flight well past the lease
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	id := seedInstance(t, database, "leasegate", srv.URL)

	// 100ms lease, renew interval a minute out (so the renewer never re-stamps it),
	// maxConcurrent=2 (a free slot to re-claim on), fast poll.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, 10*time.Millisecond, 2, true /* immediateRetries */, 100*time.Millisecond, time.Minute, LogConfig{}, log)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	waitTerminal(t, database, id, 10*time.Second)

	if inst, err := database.GetInstance(id); err != nil {
		t.Fatalf("GetInstance: %v", err)
	} else if inst.Status != model.StatusCompleted {
		t.Fatalf("expected the in-flight instance to complete, got %q (%s: %s)", inst.Status, inst.ErrorCode, inst.Error)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("expected the task to run exactly once, got %d executions (the advance was re-claimed)", got)
	}

	select {
	case <-done:
		t.Fatal("engine stopped while its leases were unrenewable instead of repairing them")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not shut down within 10s")
	}
}

// TestLeaseGate_SurvivesFrozenHost is the sleeping-laptop case. The host is suspended
// mid-task, which in DB terms means wall-clock time passes while nothing in this process
// runs: the lease expires and no renewal happens, because the renewer's ticker is frozen
// too. On wake the pump used to win the race against the renewer and re-claim the very
// instance it was still advancing.
//
// Shifting the DB clock — the clock leases are stamped against — reproduces that exactly.
// The gate must repair the lease before the claim, leaving the in-flight advance
// undisturbed: one execution of the task, one completion, no reclaim, no shutdown.
func TestLeaseGate_SurvivesFrozenHost(t *testing.T) {
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

	id := seedInstance(t, database, "frozen", srv.URL)

	// Default-ish 10s lease with the renewer an hour out, so nothing but the gate can
	// save the lease. A 50ms poll keeps the pump parked on its ticker for all but a
	// sliver of each cycle, so the clock jump below lands between claims, as a real
	// suspend does.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, 50*time.Millisecond, 2, true /* immediateRetries */, 10*time.Second, time.Hour, LogConfig{}, log)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("task never went in-flight")
	}

	// The host sleeps for two hours: far past the 10s lease and past any renewal that
	// would have happened. AdvanceClock only moves forward and every DB timestamp goes
	// through it, so this leaks nothing into later tests but a later "now".
	db.AdvanceClock(2 * time.Hour)

	// Give the pump several cycles to notice and repair before letting the task finish.
	time.Sleep(300 * time.Millisecond)

	if inst, err := database.GetInstance(id); err != nil {
		t.Fatalf("GetInstance (post-freeze): %v", err)
	} else if inst.WorkerID == nil {
		t.Fatal("the frozen worker lost its lease on the instance it was still advancing")
	} else if inst.LeaseExpiresAt == nil || !inst.LeaseExpiresAt.After(db.Now()) {
		t.Fatalf("lease was not repaired: expires %v, now %v", inst.LeaseExpiresAt, db.Now())
	}

	close(release)
	waitTerminal(t, database, id, 10*time.Second)

	if inst, err := database.GetInstance(id); err != nil {
		t.Fatalf("GetInstance: %v", err)
	} else if inst.Status != model.StatusCompleted {
		t.Fatalf("expected the instance to complete, got %q (%s: %s)", inst.Status, inst.ErrorCode, inst.Error)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("expected the task to run exactly once across the freeze, got %d executions", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not shut down within 10s")
	}
}

// TestLeaseGate_SaturatedPumpDoesNotTrip guards the choice of signal. The gate reads how
// long ago a renewal last succeeded, not how long ago the pump last claimed — because a
// pump whose slots are all busy parks on the semaphore for as long as its slowest advance,
// which on a claim-gap detector would look identical to a freeze. Here maxConcurrent=1 and
// the only task blocks for a second, so the pump is parked for ten lease durations while
// the renewer keeps ticking normally. Nothing should trip.
func TestLeaseGate_SaturatedPumpDoesNotTrip(t *testing.T) {
	database := openTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	id := seedInstance(t, database, "saturated", srv.URL)

	// 200ms lease renewed every 20ms — healthy — with a single slot, so the pump blocks
	// on e.sem for the whole 1s task.
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	eng := New(database, 10*time.Millisecond, 1, true /* immediateRetries */, 200*time.Millisecond, 20*time.Millisecond, LogConfig{}, log)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	waitTerminal(t, database, id, 10*time.Second)
	cancel()
	<-done

	if s := logs.String(); strings.Contains(s, "no successful lease renewal") {
		t.Errorf("a saturated pump tripped the lease gate; the renewer was healthy throughout:\n%s", s)
	}
}

// TestLeaseGate_VerdictOutlivesTheDelayBeforeTheClaim covers what separates a gate verdict
// from a flag: the pump decides it may take over expired leases at one instant and the
// claim runs at another, and everything the gate knows — how stale its evidence is, and so
// which rows are its own — was true only at the first. Nothing bounds the gap between them
// (a stop-the-world pause, a descheduled goroutine, a slow claim ahead of this one), so the
// verdict has to carry the instant it was decided at rather than let the claim re-derive
// one. Here the whole lease elapses in the gap, which without that is the losing case: the
// worker re-claims a row it is still advancing, dooming the in-flight advance's write for
// nothing (the fence refuses it as lease_lost).
func TestLeaseGate_VerdictOutlivesTheDelayBeforeTheClaim(t *testing.T) {
	database := openTestDB(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, 10*time.Millisecond, 2, true /* immediateRetries */, 100*time.Millisecond, time.Minute, LogConfig{}, log)

	// No server: the pump never runs, so this worker's lease on the row is the only thing
	// under test. seedInstance's URL is never fetched. The claim is manual, so the held
	// set is seeded manually too — as the pump does on every claim — or the scoped
	// renewal below would skip the row and the floor this test rests on would not hold.
	id := seedInstance(t, database, "pinned", "http://127.0.0.1:1")
	if claimed, err := database.ClaimInstances(eng.WorkerID(), eng.leaseDuration, 1, db.AllowTakeover()); err != nil || len(claimed) != 1 {
		t.Fatalf("setup claim: err=%v, count=%d", err, len(claimed))
	}
	eng.held.Store(id, struct{}{})

	// Renew rather than rely on New's seed: the setup above is several writes, and on a
	// loaded machine they can outlast the lease, leaving the gate stale before it is asked.
	if err := eng.renewLeases(); err != nil {
		t.Fatalf("renewLeases: %v", err)
	}
	takeover := eng.leaseGate() // evidence is fresh here, so this is a takeover verdict
	if takeover == db.SkipTakeover {
		t.Fatal("the gate declined a takeover on evidence renewed a moment ago; this test needs the takeover verdict")
	}
	time.Sleep(2 * eng.leaseDuration) // the claim is delayed past the lease it holds

	got, err := database.ClaimInstances(eng.WorkerID(), eng.leaseDuration, 1, takeover)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the claim re-took instance %s, which this worker still held when the gate approved the takeover", id)
	}
}

// TestDispatch_SelfReclaim covers the backstop directly, because the gate makes it
// unreachable end-to-end: a re-claim of an instance this worker is still advancing never
// starts a second advance, whatever the grace verdict. The two verdicts differ only in
// how the skip reads — an error-level capacity warning outside a grace window (renewal
// genuinely cannot keep up), a warn-level note inside one (the worker has just
// established it was not in a position to renew at all). Neither kills the worker: the
// doomed advance's write is the fence's problem now, not the process's.
func TestDispatch_SelfReclaim(t *testing.T) {
	logs := &syncBuffer{}
	eng := New(openTestDB(t), time.Millisecond, 2, true, 0, 0, LogConfig{}, slog.New(slog.NewTextHandler(logs, nil)))
	inst := &model.ProcessInstance{ID: "already-advancing"}
	eng.inflight.Store(inst.ID, struct{}{})

	var wg sync.WaitGroup
	if eng.dispatch(context.Background(), &wg, inst, false /* graced */) {
		t.Error("dispatched a second advance for an instance already in flight")
	}
	if s := logs.String(); !strings.Contains(s, "lease renewal cannot keep up") {
		t.Errorf("an ungraced self-reclaim must warn the operator about capacity; logs:\n%s", s)
	}

	if eng.dispatch(context.Background(), &wg, inst, true /* graced */) {
		t.Error("dispatched a second advance for an instance already in flight")
	}
	if s := logs.String(); !strings.Contains(s, "could not renew") {
		t.Errorf("a graced self-reclaim must read as a freeze note, not a capacity verdict; logs:\n%s", s)
	}
}

// syncBuffer is a concurrency-safe io.Writer for capturing engine logs: slog handlers are
// written to from the pump, the renewer and every advance goroutine at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// seedInstance registers a one-task definition that fetches url and creates a running
// instance parked on that task, returning the instance id.
func seedInstance(t *testing.T, database *db.DB, prefix, url string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	tasks := []*model.Task{{
		ID:     "slow",
		Action: &model.Action{Type: model.ActionTypeFetch, URL: url},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
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

// waitTerminal blocks until the instance reaches a terminal status, so a test can settle
// the work before cancelling the engine (a cancel mid-request aborts it client-side).
func waitTerminal(t *testing.T, database *db.DB, id string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		inst, err := database.GetInstance(id)
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		if inst.Status.Terminal() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s never settled; last status %q", id, inst.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp("", "genroc-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	path := f.Name()
	t.Cleanup(func() { os.Remove(path) })
	database, err := db.OpenSQLite(path, "")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}
