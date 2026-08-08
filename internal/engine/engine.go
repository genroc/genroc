package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"genroc/internal/db"
	"genroc/internal/logview"
	"genroc/internal/model"
	"genroc/internal/validation"
)

const (
	defaultLeaseDuration      = 10 * time.Second
	defaultLeaseRenewInterval = 3 * time.Second
	defaultPayloadBytes       = 2048
)

// LogConfig controls how much the engine persists to each instance's audit log
// and for how long, plus the verbosity of the unified server console.
type LogConfig struct {
	Payloads     bool          // capture truncated request/response snippets on task events
	PayloadBytes int           // max bytes per captured snippet (<=0 → defaultPayloadBytes)
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
	// held is the instance IDs the renewer keeps alive: inserted on claim, removed when
	// runAdvance returns, so a lease always outlives the write it protects. A row that
	// leaves the set expires with worker_id intact — the hand-back path. specs/lease-fencing.md.
	// Reach it only through holdLease/dropLease/heldLeases; heldMu is never held across a
	// database call, so a slow renewal cannot stall a claim.
	heldMu sync.Mutex
	held   map[string]struct{}
	// lastRenewMs: DB-clock millis of the last successful renewal — the worker's only
	// evidence its leases are alive. Written by the renewer, read by the pump; every held
	// lease expires at lastRenewMs+leaseDuration or later, the invariant leaseGate rests on
	// (renewLeases has the fragile half).
	lastRenewMs atomic.Int64
	// schemaCache memoises inference so logged payloads can be schema-redacted (secret
	// fields → "***") without re-running the solver on every log line.
	schemaCache validation.SchemaCache
}

// schemaFile returns the inferred schemas for the instance's process (cached),
// used to redact secret-derived fields from logged payloads.
func (e *Engine) schemaFile(inst *model.ProcessInstance) (validation.SchemaFile, bool) {
	return e.schemaCache.Get(inst.ProcessName, inst.ProcessVersion, func() (*model.ProcessDefinition, error) {
		return e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	})
}

// New creates an Engine. maxConcurrent bounds parallel advances and the per-tick claim
// size. immediateRetries disables backoff (tests only). leaseDuration/leaseRenewInterval
// default to 10s/3s when 0; the renew interval must be comfortably shorter than the lease
// so the renewer can re-stamp leases before they expire.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, immediateRetries bool, leaseDuration, leaseRenewInterval time.Duration, logCfg LogConfig, log *slog.Logger, opts ...Option) *Engine {
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	if leaseRenewInterval <= 0 {
		leaseRenewInterval = defaultLeaseRenewInterval
	}
	// Dereferenced objects survive on the same horizon as audit logs, so a log that
	// references an object stays resolvable for as long as the log itself lives.
	database.SetObjectRetention(logCfg.Retention)
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
	// Seeded here as well as in Run: LeaseAge is served from it, and a health probe that
	// arrives in the gap between New and Run would otherwise read the zero value as
	// "no renewal since 1970".
	e.lastRenewMs.Store(db.Now().UnixMilli())
	return e
}

// Option configures an Engine beyond New's positional arguments.
type Option func(*Engine)

// WithWorkerID overrides the identity this worker stamps on the rows it leases. The
// default is hostname-pid: unique per process, NOT per Engine. Two engines sharing one
// process must therefore be given distinct ids, or every lease predicate reads them as
// the same worker — and `lease_epoch` cannot restore a distinction `worker_id` already
// lost, since it is precisely the self-reclaim-vs-takeover question the epoch defers to
// `worker_id` to answer (specs/lease-fencing.md).
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

// LeaseAge is how long ago this worker last proved the leases it holds are still alive.
// Growing past the lease duration is the same evidence leaseGate acts on: this worker has
// been unable to reach the database, and the instances it claimed are being taken over.
func (e *Engine) LeaseAge() time.Duration {
	return time.Duration(db.Now().UnixMilli()-e.lastRenewMs.Load()) * time.Millisecond
}

// signalWork nudges the pump to re-scan immediately. Non-blocking on a buffer-1 channel:
// concurrent nudges coalesce and a nudge with no pump parked on it is dropped, so the
// ticker remains the idle floor.
func (e *Engine) signalWork() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// NotifyWork tells the engine new runnable work may exist (e.g. a freshly created
// instance), so the pump claims it without waiting for the next poll tick.
func (e *Engine) NotifyWork() { e.signalWork() }

// holdLease puts an instance in the renewer's set, from the claim that granted it.
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

// renewLeases re-stamps the held set and records when that last succeeded (the renewer
// and the gate's repair both come through here). The stamp is the instant the renewal
// derived its expiries from, never the post-write clock — late evidence overstates the
// gate's floor by the write's duration.
func (e *Engine) renewLeases() error {
	renewedAt, err := e.db.RenewWorkerLeases(e.workerID, e.heldLeases(), e.leaseDuration)
	if err != nil {
		return err
	}
	e.lastRenewMs.Store(renewedAt.UnixMilli())
	return nil
}

// leaseGate, before every claim: on stale renewal evidence it repairs its own leases and
// declines takeovers for one lease period, as a cutoff pinned to the instant the evidence
// was read (a delayed claim cannot widen it). It reads the RENEWAL gap, in the CLAIMANT —
// each wrong-looking choice here is argued in specs/lease-fencing.md and CLAUDE.md.
//
// graceUntilMs is the caller's own state, read and extended here: the pump owns it as a
// local, so the window belongs to one goroutine by construction rather than by convention.
func (e *Engine) leaseGate(graceUntilMs *int64) db.Takeover {
	now := db.Now()
	nowMs := now.UnixMilli()
	stale := time.Duration(nowMs-e.lastRenewMs.Load()) * time.Millisecond

	// Trip one poll early, so a lease is repaired while it is still alive rather than after
	// a peer has had a poll's worth of chances to take the row. Capped at half the lease so
	// a poll interval longer than the lease cannot park the worker in a permanent grace,
	// never recovering dead workers' rows.
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

	// Once per window, not once per poll: the window is extended below while the
	// condition persists. Debug, not warn: a suspended laptop trips this benignly on every
	// wake, and an unreachable DB still reports the renewal and claim failures at error.
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

// Run starts the engine loop and blocks until ctx is cancelled; in-flight work drains
// before it returns. When pollEvery is zero the engine does not auto-tick; call Tick
// explicitly. Lease pressure is never fatal: the gate repairs it or the fence refuses
// the stale write (lease_lost) — there is no exit path.
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

// runPump is the continuous claim/dispatch loop used when pollEvery > 0. Unlike Tick it
// never waits for a batch to finish, topping up work as slots free, so a slow instance
// never stalls the others. e.sem is both the concurrency bound and the idle detector.
func (e *Engine) runPump(ctx context.Context) {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	var wg sync.WaitGroup
	defer wg.Wait() // stop claiming, finish in-flight advances, then return

	// The takeover-grace window, owned by this loop: leaseGate is its only reader and
	// writer, and only this goroutine calls leaseGate.
	var graceUntilMs int64
	// Log/object pruning rides the pump rather than a goroutine of its own — it is a
	// once-a-minute janitor, and the poll ticker already wakes this loop far more often.
	// Manual-tick mode never reaches here; Tick prunes for itself.
	nextPruneMs := db.Now().UnixMilli() + logPruneInterval.Milliseconds()

	for {
		if nowMs := db.Now().UnixMilli(); nowMs >= nextPruneMs {
			nextPruneMs = nowMs + logPruneInterval.Milliseconds()
			e.pruneLogs()
		}

		// Acquire every free slot up front so the dispatch loop below never blocks:
		// with the claim's wait_state<>'waiting' filter, that closes the window where an
		// in-flight advance finishes between claim and dispatch and lets a stale snapshot
		// through. slots is the exact claim limit, so in-flight never exceeds maxConcurrent.
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
			// Nothing claimable right now: wait for the next tick, or wake early when
			// signalWork reports freshly-runnable work (a self-requeued loop, spawned
			// children, an un-parked parent, or a newly created instance).
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-ticker.C:
			}
			continue
		}

		// Each dispatch consumes one pre-acquired slot (released when the advance
		// finishes).
		for _, inst := range insts {
			if !e.dispatch(ctx, &wg, inst, takeover == db.SkipTakeover) {
				<-e.sem // slot reserved for this instance, left to the advance already running it
			}
		}
	}
}

// dispatch runs one advance in its own goroutine, releasing the caller's e.sem slot when
// done. It reports whether it started one: an in-flight instance is never advanced twice —
// the claim is left to the running advance, whose write the re-claim has already doomed.
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
	// No recover() here on purpose: the barrier is one level down in advanceGuarded, where
	// the panicking instance is still in hand and can be failed. What reaches this
	// goroutine is a panic from the persist path, which is meant to take the worker down.
	go func() {
		defer wg.Done()
		defer func() { <-e.sem }()
		// runAdvance drops the inflight marker (stored above) before persisting.
		if err := e.runAdvance(ctx, inst); err != nil {
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: "advance instance: " + err.Error()})
		}
	}()
	return true
}

// leaseRenewer renews this worker's leases every leaseRenewInterval, in its own goroutine
// so renewals are never blocked by a long tick.
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

// pruneLogs deletes audit logs past the retention window. No-op when retention is
// disabled. Best-effort: a failure is logged and otherwise ignored. The cutoff uses the
// DB clock, so a test clock shift expires logs without a real wait.
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
	// Objects share the horizon: their expiry was stamped to now+retention on release.
	if n, err := e.db.DeleteExpiredObjects(db.Now().UnixMilli()); err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "prune objects: " + err.Error()})
	} else if n > 0 {
		e.logOnly(logEvent{Level: model.LogDebug, Msg: "pruned objects", Meta: map[string]any{"count": n}})
	}
}

// ManualTick reports whether the engine runs in manual-tick mode (pollEvery == 0). The
// /tick endpoint is only meaningful then: with the continuous pump running, an out-of-band
// Tick would race it, so the endpoint refuses.
func (e *Engine) ManualTick() bool { return e.pollEvery == 0 }

// Tick claims pending instances and processes each in its own goroutine, blocking until
// all finish so ticks never overlap and the same instance is never advanced twice
// concurrently. Returns the number of instances claimed and processed.
func (e *Engine) Tick(ctx context.Context) (int, error) {
	e.pruneLogs()
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
