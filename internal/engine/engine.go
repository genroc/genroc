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

// OverwhelmError is returned by Run when this worker re-claimed an instance it was still
// advancing, meaning lease renewal cannot keep up. The pump stops claiming, in-flight work
// drains, and the binary should exit non-zero to be restarted.
//
// leaseGate catches the same fault a step earlier and repairs it, so this is the backstop
// for what the gate cannot see. specs/lease-fencing.md.
type OverwhelmError struct {
	InstanceID    string
	WorkerID      string
	Lease         time.Duration
	MaxConcurrent int
}

func (e *OverwhelmError) Error() string {
	return fmt.Sprintf("engine overwhelmed: re-claimed instance %s still being advanced by worker %s; "+
		"lease renewal cannot keep up (lease=%s, max_concurrent=%d). Lower --max-concurrent or increase the lease duration.",
		e.InstanceID, e.WorkerID, e.Lease, e.MaxConcurrent)
}

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
	inflight           sync.Map // instance IDs this worker is currently advancing (detects overwhelm via self-reclaim)
	// lastRenewMs is DB-clock millis of the last successful renewal pass: the worker's
	// only evidence that the leases it holds are alive. Written by the renewer, read by
	// the pump. Every lease this worker holds expires at lastRenewMs+leaseDuration or
	// later, which is what lets leaseGate rule out its own rows — see renewLeases for the
	// half of that invariant which is easy to break.
	lastRenewMs atomic.Int64
	// graceUntilMs is touched only by the pump goroutine, so it needs no synchronisation.
	graceUntilMs int64
	// schemaCache caches the inferred SchemaFile per (process,version) so logged
	// payloads can be schema-redacted (secret fields → "***") without re-running
	// inference on every log line. Definitions are immutable per version.
	schemaCache sync.Map
}

type schemaKey struct {
	name    string
	version int
}

// schemaFile returns the inferred schemas for the instance's process (cached),
// used to redact secret-derived fields from logged payloads.
func (e *Engine) schemaFile(inst *model.ProcessInstance) (validation.SchemaFile, bool) {
	key := schemaKey{inst.ProcessName, inst.ProcessVersion}
	if cached, ok := e.schemaCache.Load(key); ok {
		return cached.(validation.SchemaFile), true
	}
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return validation.SchemaFile{}, false
	}
	sf, err := validation.Generate(def)
	if err != nil {
		return validation.SchemaFile{}, false
	}
	e.schemaCache.Store(key, sf)
	return sf, true
}

// New creates an Engine. maxConcurrent bounds parallel advances and the per-tick claim
// size. immediateRetries disables backoff (tests only). leaseDuration/leaseRenewInterval
// default to 10s/3s when 0; the renew interval must be comfortably shorter than the lease
// so the renewer can re-stamp leases before they expire.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, immediateRetries bool, leaseDuration, leaseRenewInterval time.Duration, logCfg LogConfig, log *slog.Logger) *Engine {
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
	}
	// Seeded here as well as in Run: LeaseAge is served from it, and a health probe that
	// arrives in the gap between New and Run would otherwise read the zero value as
	// "no renewal since 1970".
	e.lastRenewMs.Store(db.Now().UnixMilli())
	return e
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

// renewLeases re-stamps this worker's leases and records when that last succeeded. Both
// the renewer and the pump's repair path must go through here, or lastRenewMs stops
// meaning "the last moment this worker could prove its leases were alive".
//
// The stamp is the instant the renewal derived its expiries from, which the renewal
// reports; reading the clock here instead would date the evidence from after a write that
// can outlast the gate's margin, and the gate would then hand out a takeover cutoff past
// leases that had already lapsed.
func (e *Engine) renewLeases() error {
	renewedAt, err := e.db.RenewWorkerLeases(e.workerID, e.leaseDuration)
	if err != nil {
		return err
	}
	e.lastRenewMs.Store(renewedAt.UnixMilli())
	return nil
}

// leaseGate runs before every claim: if no renewal has succeeded within a lease, this
// worker cannot prove the leases it holds are alive, so it renews them synchronously and
// then declines lease takeovers (db.SkipTakeover) for one lease duration.
//
// Its verdict is a cutoff pinned to the instant it read the evidence, never "whatever the
// clock says when the claim runs" — every lease this worker holds outlives that instant by
// construction, so however long the claim is delayed after this returns, it cannot take a
// row this worker is still advancing.
//
// Two more things break if changed. It must read the *renewal* gap, not the claim gap — a
// saturated pump legitimately parks on e.sem for minutes with healthy leases. And it must
// run in the claimant rather than the renewer, because on resume every frozen ticker fires
// at once and the two race; checking here makes both orderings correct.
//
// Why a repair rather than an exit, why the grace covers co-resident workers, and why
// there is no once-per-window bound: specs/lease-fencing.md.
func (e *Engine) leaseGate() db.Takeover {
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
		if nowMs < e.graceUntilMs {
			return db.SkipTakeover
		}
		return db.TakeoverBefore(now)
	}

	// Once per window, not once per poll: the window is extended below while the
	// condition persists.
	if nowMs >= e.graceUntilMs {
		e.logOnly(logEvent{Level: model.LogWarn,
			Msg: "no successful lease renewal for " + stale.Round(time.Millisecond).String() +
				"; leases this worker holds may have lapsed - renewing them and declining takeovers for " +
				e.leaseDuration.String(),
			Meta: map[string]any{"worker": e.workerID, "stale_for": stale.Round(time.Millisecond).String(), "lease": e.leaseDuration.String()}})
	}
	e.graceUntilMs = nowMs + e.leaseDuration.Milliseconds()
	if err := e.renewLeases(); err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "repair worker leases: " + err.Error()})
	}
	return db.SkipTakeover
}

// Run starts the engine loop and blocks until ctx is cancelled, returning nil on a clean
// shutdown or an *OverwhelmError if the pump could not keep up with its leases. In-flight
// work is drained before it returns either way. When pollEvery is zero the engine does not
// auto-tick; call Tick explicitly.
func (e *Engine) Run(ctx context.Context) error {
	e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine started", Meta: map[string]any{"poll_interval": e.pollEvery, "max_concurrent": cap(e.sem), "worker": e.workerID}})

	// Seed before the renewer's first tick, or a freshly started engine reads as stale.
	e.lastRenewMs.Store(db.Now().UnixMilli())
	go e.leaseRenewer(ctx)

	if e.logCfg.Retention > 0 {
		go e.logPruner(ctx)
	}

	if e.pollEvery == 0 {
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine in manual tick mode"})
		<-ctx.Done()
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine stopped"})
		return nil
	}

	err := e.runPump(ctx)
	if err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: err.Error()})
	} else {
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine stopped"})
	}
	return err
}

// runPump is the continuous claim/dispatch loop used when pollEvery > 0. Unlike Tick it
// never waits for a batch to finish, topping up work as slots free, so a slow instance
// never stalls the others. e.sem is both the concurrency bound and the idle detector.
func (e *Engine) runPump(ctx context.Context) error {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	var wg sync.WaitGroup
	defer wg.Wait() // stop claiming, finish in-flight advances, then return

	for {
		// Acquire every free slot up front so the dispatch loop below never blocks:
		// with the claim's wait_state<>'waiting' filter, that closes the window where an
		// in-flight advance finishes between claim and dispatch and lets a stale snapshot
		// through. slots is the exact claim limit, so in-flight never exceeds maxConcurrent.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return nil
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
		takeover := e.leaseGate()

		insts, err := e.db.ClaimInstances(e.workerID, e.leaseDuration, slots, takeover)
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
				return nil
			case <-e.wake:
			case <-ticker.C:
			}
			continue
		}

		// Each dispatch consumes one pre-acquired slot (released when the advance
		// finishes). If dispatch reports overwhelm, stop claiming and return: the
		// deferred wg.Wait drains the advances already in flight first.
		for _, inst := range insts {
			dispatched, err := e.dispatch(ctx, &wg, inst, takeover == db.SkipTakeover)
			if err != nil {
				return err
			}
			if !dispatched {
				<-e.sem // slot reserved for this instance, left to the advance already running it
			}
		}
	}
}

// dispatch runs one instance's advance in its own goroutine and releases its e.sem slot
// when done (the caller must have reserved it). It reports whether it started an advance:
// an instance already in-flight here is never advanced twice, returning an *OverwhelmError
// or, inside a grace window, false with no error so the caller releases the slot and
// carries on. graced is the lease gate's verdict for this claim.
func (e *Engine) dispatch(ctx context.Context, wg *sync.WaitGroup, inst *model.ProcessInstance, graced bool) (bool, error) {
	// The marker is exact: runAdvance drops it only just before the write that frees the
	// instance, so finding it here means a lease genuinely lapsed under a live advance —
	// never an advance that already finished. Outside a grace window that means renewal
	// cannot keep up. Inside one the gate has just established this worker could not renew
	// at all, so the same event is not a verdict about capacity and must not kill a worker
	// full of healthy advances. specs/lease-fencing.md.
	if _, busy := e.inflight.LoadOrStore(inst.ID, struct{}{}); busy {
		if graced {
			e.logOnly(logEvent{Level: model.LogWarn, ID: inst.ID,
				Msg:  "re-claimed an instance still being advanced here; its lease lapsed while this worker could not renew, not because renewal fell behind — leaving it to the in-flight advance",
				Meta: map[string]any{"worker": e.workerID, "lease": e.leaseDuration.String()}})
			return false, nil
		}
		return false, &OverwhelmError{
			InstanceID:    inst.ID,
			WorkerID:      e.workerID,
			Lease:         e.leaseDuration,
			MaxConcurrent: cap(e.sem),
		}
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
	return true, nil
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

// logPruner deletes audit-log rows past the retention window. The cutoff uses the DB
// clock, so a test clock shift expires logs without a real wait.
func (e *Engine) logPruner(ctx context.Context) {
	ticker := time.NewTicker(logPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.pruneLogs()
		}
	}
}

// pruneLogs deletes audit logs past the retention window. No-op when retention
// is disabled. Best-effort: a failure is logged and otherwise ignored.
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
	var wg sync.WaitGroup
	for _, inst := range instances {
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
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
