package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"genroc/internal/db"
	"genroc/internal/logview"
	"genroc/internal/model"
)

const (
	defaultLeaseDuration      = 10 * time.Second
	defaultLeaseRenewInterval = 3 * time.Second
	defaultPayloadBytes       = 2048
)

// LogConfig controls how much the engine persists to each instance's audit log
// and for how long, plus the verbosity of the unified server console.
type LogConfig struct {
	Payloads     bool          // record event payloads (inputs, outputs, request/response bodies) on audit rows
	PayloadBytes int           // inline budget per payload; larger values externalize (<=0 → defaultPayloadBytes)
	Retention    time.Duration // prune audit logs older than this; 0 = keep forever
	Mode         logview.Mode  // console verbosity: basic omits the data body, detail includes it
}

const logPruneInterval = time.Minute

// Engine is the main orchestration loop. It polls the database for pending
// instances and advances each one task at a time.
type Engine struct {
	db                 *db.DB
	pollEvery          time.Duration
	immediateRetries   bool
	leaseDuration      time.Duration // how long a claimed instance is leased to this worker
	leaseRenewInterval time.Duration // how often the renewer re-stamps this worker's leases
	logCfg             LogConfig     // audit-log persistence settings
	log                *slog.Logger
	sem                chan struct{}
	wake               chan struct{} // buffer-1 nudge: "runnable work may exist, re-scan now" (see signalWork)
	workerID           string
	inflight           sync.Map // instance IDs this worker is currently advancing (detects self-reclaim)
	// held is the renewer's set; leaving it is the hand-back (CLAUDE.md). Reach it only via
	// holdLease/dropLease/heldLeases: heldMu is never held across a database call.
	heldMu sync.Mutex
	held   map[string]struct{}
	// lastRenewMs is DB-clock millis: every held lease expires at lastRenewMs+leaseDuration or
	// later, the invariant leaseGate rests on (renewLeases has the fragile half).
	lastRenewMs atomic.Int64
}

// Every engine read of a definition goes through here, so a blip cannot fail an instance.
func (e *Engine) definition(name string, version int) (*model.ProcessDefinition, error) {
	return retryRead(func() (*model.ProcessDefinition, error) {
		return e.db.GetDefinition(name, version)
	})
}

// New creates an Engine. maxConcurrent bounds parallel advances and the claim size;
// immediateRetries is for tests. Zero lease durations default to 10s/3s, and the renew
// interval must be comfortably shorter than the lease.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, immediateRetries bool, leaseDuration, leaseRenewInterval time.Duration, logCfg LogConfig, log *slog.Logger, opts ...Option) *Engine {
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d-%s", hostname, os.Getpid(), randomSuffix())
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	if leaseRenewInterval <= 0 {
		leaseRenewInterval = defaultLeaseRenewInterval
	}
	e := &Engine{
		db:                 database,
		pollEvery:          pollEvery,
		immediateRetries:   immediateRetries,
		leaseDuration:      leaseDuration,
		leaseRenewInterval: leaseRenewInterval,
		logCfg:             logCfg,
		log:                log,
		sem:                make(chan struct{}, maxConcurrent),
		wake:               make(chan struct{}, 1),
		workerID:           workerID,
		held:               make(map[string]struct{}, maxConcurrent),
	}
	for _, opt := range opts {
		opt(e)
	}
	// Seeded here as well as in Run, or a health probe between New and Run reads "no renewal
	// since 1970".
	e.lastRenewMs.Store(db.Now().UnixMilli())
	return e
}

// Option configures an Engine beyond New's positional arguments.
type Option func(*Engine)

// randomSuffix keeps worker ids unique when hostname and pid collide: the fence compares
// worker_id, so a collision would let a stale write pass.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// WithWorkerID overrides the default hostname-pid-random identity. It must be at least as
// unique: two live workers sharing one would each pass the other's lease fence.
func WithWorkerID(id string) Option {
	return func(e *Engine) {
		if id != "" {
			e.workerID = id
		}
	}
}

// WorkerID is this worker's identity in the lease columns — the value an operator
// correlates a stuck instance against.
func (e *Engine) WorkerID() string { return e.workerID }

// LeaseAge is how long ago this worker last renewed its leases. Past the lease duration, the
// instances it claimed are being taken over.
func (e *Engine) LeaseAge() time.Duration {
	return time.Duration(db.Now().UnixMilli()-e.lastRenewMs.Load()) * time.Millisecond
}

// signalWork never blocks: nudges coalesce, and one with no pump waiting is dropped, so the
// ticker stays the idle floor.
func (e *Engine) signalWork() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// NotifyWork tells the engine new runnable work may exist (e.g. a freshly created
// instance), so the pump claims it without waiting for the next poll tick.
func (e *Engine) NotifyWork() { e.signalWork() }

func (e *Engine) holdLease(id string) {
	e.heldMu.Lock()
	defer e.heldMu.Unlock()
	e.held[id] = struct{}{}
}

// dropLease stops renewing an instance's lease, which is how the lease is handed back:
// it lapses on its own with worker_id intact, the evidence ReclaimedExpired derives from.
func (e *Engine) dropLease(id string) {
	e.heldMu.Lock()
	defer e.heldMu.Unlock()
	delete(e.held, id)
}

// heldLeases snapshots the set for a renewal. A snapshot, so the lock is not held across
// the database write that follows.
func (e *Engine) heldLeases() []string {
	e.heldMu.Lock()
	defer e.heldMu.Unlock()
	ids := make([]string, 0, len(e.held))
	for id := range e.held {
		ids = append(ids, id)
	}
	return ids
}

// renewLeases stamps the instant the renewal derived its expiries from, never the post-write
// clock: late evidence overstates the gate's floor by the write's duration.
func (e *Engine) renewLeases() error {
	renewedAt, err := e.db.RenewWorkerLeases(e.workerID, e.heldLeases(), e.leaseDuration)
	if err != nil {
		return err
	}
	e.lastRenewMs.Store(renewedAt.UnixMilli())
	return nil
}

// leaseGate pins its grace to the instant the evidence was read, so a delayed claim cannot
// widen it; each wrong-looking choice is argued in specs/lease-fencing.md. graceUntilMs is
// the pump's own local, so the window belongs to one goroutine.
func (e *Engine) leaseGate(graceUntilMs *int64) db.Takeover {
	now := db.Now()
	nowMs := now.UnixMilli()
	stale := time.Duration(nowMs-e.lastRenewMs.Load()) * time.Millisecond

	// Trip one poll early, while the lease is still alive. Capped at half the lease, or a long
	// poll interval parks the worker in permanent grace.
	margin := e.pollEvery
	if cap := e.leaseDuration / 2; margin > cap {
		margin = cap
	}
	if stale+margin < e.leaseDuration {
		// A grace opened by an earlier trip still applies: peers that froze alongside
		// this worker have not necessarily repaired yet.
		if nowMs < *graceUntilMs {
			return db.SkipTakeover
		}
		return db.TakeoverBefore(now)
	}

	// Once per window, not per poll. Debug: a suspended laptop trips this benignly on every
	// wake, and an unreachable DB still reports its failures at error.
	if nowMs >= *graceUntilMs {
		e.logOnly(logEvent{Level: model.LogDebug,
			Msg: "no successful lease renewal for " + stale.Round(time.Millisecond).String() +
				"; leases this worker holds may have lapsed - renewing them and declining takeovers for " +
				e.leaseDuration.String(),
			Meta: map[string]any{"worker": e.workerID, "stale_for": stale.Round(time.Millisecond).String(), "lease": e.leaseDuration.String()}})
	}
	*graceUntilMs = nowMs + e.leaseDuration.Milliseconds()
	if err := e.renewLeases(); err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "repair worker leases: " + err.Error()})
	}
	return db.SkipTakeover
}

// Run blocks until ctx is cancelled and in-flight work drains. With pollEvery zero it does
// not auto-tick; call Tick. Lease pressure is never fatal: there is no exit path.
func (e *Engine) Run(ctx context.Context) {
	e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine started", Meta: map[string]any{"poll_interval": e.pollEvery, "max_concurrent": cap(e.sem), "worker": e.workerID}})

	// Seed before the renewer's first tick, or a freshly started engine reads as stale.
	e.lastRenewMs.Store(db.Now().UnixMilli())
	go e.leaseRenewer(ctx)

	if e.pollEvery == 0 {
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine in manual tick mode"})
		<-ctx.Done()
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine stopped"})
		return
	}

	e.runPump(ctx)
	e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine stopped"})
}

// runPump never waits for a batch, topping up as slots free. e.sem is both the concurrency
// bound and the idle detector.
func (e *Engine) runPump(ctx context.Context) {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	var wg sync.WaitGroup
	defer wg.Wait() // stop claiming, finish in-flight advances, then return

	// The takeover-grace window, owned by this loop: leaseGate is its only reader and
	// writer, and only this goroutine calls leaseGate.
	var graceUntilMs int64
	// Manual-tick mode never reaches here; Tick prunes for itself.
	nextPruneMs := db.Now().UnixMilli() + logPruneInterval.Milliseconds()

	for {
		if nowMs := db.Now().UnixMilli(); nowMs >= nextPruneMs {
			nextPruneMs = nowMs + logPruneInterval.Milliseconds()
			e.pruneLogs()
			e.collectObjects()
		}

		// Acquire every free slot first so dispatch never blocks: with the claim's
		// phase<>'children' filter, that closes the window a stale snapshot slips through.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		slots := 1
	fill:
		for slots < cap(e.sem) {
			select {
			case e.sem <- struct{}{}:
				slots++
			default:
				break fill
			}
		}

		// Before the claim, never after: the point is to repair lapsed leases while
		// there is still a claim to protect from them.
		takeover := e.leaseGate(&graceUntilMs)

		insts, err := e.db.ClaimInstances(e.workerID, e.leaseDuration, slots, takeover)
		// Into the held set immediately: a renewal in the claim-to-dispatch gap would
		// stamp lastRenewMs past leases it never saw, undermining the gate's floor.
		for _, inst := range insts {
			e.holdLease(inst.ID)
		}
		// Release the slots we acquired but won't use (claimed fewer than slots).
		for i := len(insts); i < slots; i++ {
			<-e.sem
		}
		if err != nil || len(insts) == 0 {
			if err != nil {
				e.logOnly(logEvent{Level: model.LogError, Msg: "claim instances: " + err.Error()})
			}
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-ticker.C:
			}
			continue
		}

		// Before any of them runs: a claim is an only_once task's only evidence that it ran, and
		// below `strict` its commit is not flushed. specs/durability-levels.md s4.
		e.hardenClaims(ctx, insts)

		for _, inst := range insts {
			if !e.dispatch(ctx, &wg, inst, takeover == db.SkipTakeover) {
				<-e.sem // slot reserved for this instance, left to the advance already running it
			}
		}
	}
}

// hardenClaims reads next_replayable off the row, never the definition: hottest path, and
// outside the panic barrier. A failure costs only the only_once.interrupted distinction.
func (e *Engine) hardenClaims(ctx context.Context, insts []*model.ProcessInstance) {
	for _, inst := range insts {
		if inst.NextReplayable {
			continue
		}
		if err := e.db.Flush(ctx); err != nil {
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
				Msg: "could not make an only_once claim durable before running it: " + err.Error()})
		}
		return
	}
}

// dispatch never advances an in-flight instance twice: the claim is left to the running
// advance, whose write the re-claim has already doomed.
func (e *Engine) dispatch(ctx context.Context, wg *sync.WaitGroup, inst *model.ProcessInstance, graced bool) bool {
	// The marker is exact: runAdvance drops it just before the freeing write, so a hit means a
	// lease lapsed under a live advance — true only while advance() writes nothing. Inside a
	// grace window the gate has established this worker could not renew: no capacity verdict.
	if _, busy := e.inflight.LoadOrStore(inst.ID, struct{}{}); busy {
		if graced {
			e.logOnly(logEvent{Level: model.LogWarn, ID: inst.ID,
				Msg:  "re-claimed an instance still being advanced here; its lease lapsed while this worker could not renew, not because renewal fell behind — leaving it to the in-flight advance",
				Meta: map[string]any{"worker": e.workerID, "lease": e.leaseDuration.String()}})
		} else {
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID,
				Msg: fmt.Sprintf("re-claimed an instance still being advanced here; lease renewal cannot keep up (lease=%s, max_concurrent=%d). "+
					"Lower --max-concurrent or increase the lease duration. Leaving it to the in-flight advance, whose write will now be refused (lease_lost)",
					e.leaseDuration, cap(e.sem)),
				Meta: map[string]any{"worker": e.workerID, "lease": e.leaseDuration.String()}})
		}
		return false
	}
	wg.Add(1)
	// No recover() on purpose: the barrier is advanceGuarded, and a persist panic is meant
	// to take the worker down.
	go func() {
		defer wg.Done()
		defer func() { <-e.sem }()
		if err := e.runAdvance(ctx, inst); err != nil {
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: "advance instance: " + err.Error()})
		}
	}()
	return true
}

func (e *Engine) leaseRenewer(ctx context.Context) {
	ticker := time.NewTicker(e.leaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.renewLeases(); err != nil {
				e.logOnly(logEvent{Level: model.LogError, Msg: "renew worker leases: " + err.Error()})
			}
		}
	}
}

// pruneLogs cuts off on the DB clock, so a test clock shift expires logs without a real wait.
func (e *Engine) pruneLogs() {
	if e.logCfg.Retention <= 0 {
		return
	}
	cutoff := db.Now().Add(-e.logCfg.Retention).UnixMilli()
	if n, err := e.db.PruneLogs(cutoff); err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "prune logs: " + err.Error()})
	} else if n > 0 {
		e.logOnly(logEvent{Level: model.LogDebug, Msg: "pruned audit logs", Meta: map[string]any{"count": n, "older_than": e.logCfg.Retention}})
	}
}

// collectObjects is not gated on log retention: ordinary work releases objects, so a run with
// retention disabled would otherwise grow without bound.
func (e *Engine) collectObjects() {
	if n, err := e.db.CollectObjects(db.Now().UnixMilli()); err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "collect objects: " + err.Error()})
	} else if n > 0 {
		e.logOnly(logEvent{Level: model.LogDebug, Msg: "collected objects", Meta: map[string]any{"count": n}})
	}
}

// ManualTick reports pollEvery == 0. Tick must not run otherwise: it would race the pump.
func (e *Engine) ManualTick() bool { return e.pollEvery == 0 }

// Tick claims pending instances and processes each in its own goroutine, blocking until
// all finish so ticks never overlap and the same instance is never advanced twice
// concurrently. Returns the number of instances claimed and processed.
func (e *Engine) Tick(ctx context.Context) (int, error) {
	e.pruneLogs()
	e.collectObjects()
	// No lease gate: a tick waits for every advance it starts, so it cannot re-claim its
	// own in-flight work.
	instances, err := e.db.ClaimInstances(e.workerID, e.leaseDuration, cap(e.sem), db.AllowTakeover())
	if err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "claim instances: " + err.Error()})
		return 0, err
	}
	// Into the held set before dispatching, same as runPump (the renewer runs in
	// manual-tick mode too).
	for _, inst := range instances {
		e.holdLease(inst.ID)
	}
	// Same reason as runPump: a tick claims and runs, so it owes the same guarantee.
	e.hardenClaims(ctx, instances)
	var wg sync.WaitGroup
	for i, inst := range instances {
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			// Never dispatched, so no runAdvance will remove them: drop the rest or
			// their leases are renewed forever.
			for _, undispatched := range instances[i:] {
				e.dropLease(undispatched.ID)
			}
			wg.Wait()
			return 0, ctx.Err()
		}
		wg.Add(1)
		go func(inst *model.ProcessInstance) {
			defer wg.Done()
			defer func() { <-e.sem }()
			if err := e.runAdvance(ctx, inst); err != nil {
				e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: "advance instance: " + err.Error()})
			}
		}(inst)
	}
	wg.Wait()
	return len(instances), nil
}
