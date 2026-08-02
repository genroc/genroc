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
	"genroc/internal/transport"
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

// OverwhelmError is returned by Run when the engine re-claimed an instance it was
// still advancing: the in-flight advance outlived its lease, so lease renewal can't
// keep up. There is no safe recovery — in a multi-worker deployment another worker
// would already have stolen and double-executed the instance — so the pump stops
// claiming, in-flight work is drained, and Run returns this. The binary should log it
// and exit non-zero so the worker is restarted; lowering --max-concurrent or raising
// the lease duration prevents recurrence.
//
// The lease gate (see leaseGate) catches the same fault one step earlier and repairs it,
// so this is now the backstop for what the gate cannot see — chiefly a stall that lands
// inside the claim itself. docs/lease-fencing.md specs the rest: fencing every write on a
// per-grant token, after which a re-claim never has to be fatal at all.
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
	// lastRenewMs is the DB-clock instant (unix millis) of the last renewal pass that
	// completed successfully — stamped by the renewer, read by the pump. It is the
	// worker's only evidence that the leases it holds are still alive; see leaseGate.
	lastRenewMs atomic.Int64
	// graceUntilMs is the DB-clock instant until which this worker declines lease
	// takeovers, set when leaseGate finds the evidence stale. Touched only by the pump
	// goroutine (runPump and the dispatch calls it makes), so it needs no synchronisation.
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
	return &Engine{
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
}

// signalWork nudges the pump to re-scan for runnable work immediately. The send is
// non-blocking on a buffer-1 channel, so concurrent nudges coalesce into one pending wake
// and a nudge with no pump parked on it is harmlessly dropped — the ticker is the idle
// floor.
func (e *Engine) signalWork() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// NotifyWork tells the engine new runnable work may exist (e.g. a freshly created
// instance), so the pump claims it without waiting for the next poll tick.
func (e *Engine) NotifyWork() { e.signalWork() }

func (e *Engine) retryDelay(attempt int) time.Duration {
	if e.immediateRetries {
		return 0
	}
	return transport.RetryDelay(attempt)
}

// renewLeases re-stamps this worker's leases and records when that last succeeded. Both
// the renewer's ticker and the pump's repair path go through here, so lastRenewMs means
// exactly one thing: the last moment this worker could prove its leases were alive.
func (e *Engine) renewLeases() error {
	if err := e.db.RenewWorkerLeases(e.workerID, e.leaseDuration); err != nil {
		return err
	}
	e.lastRenewMs.Store(db.Now().UnixMilli())
	return nil
}

// leaseGate runs immediately before every claim and answers one question: can this worker
// still prove the leases it holds are alive? The evidence is lastRenewMs; older than one
// lease duration and the answer is no — either the process was not running (a suspended
// host, a frozen container, a stop-the-world stall) or it was running but could not reach
// the database. For leases those are the same fact, and both end the same way: rows this
// worker holds have expired on paper while it was in no position to notice.
//
// Left alone, the next claim then re-claims work this worker is still advancing, which the
// engine reads as being overwhelmed and dies over — the failure this gate exists to
// prevent. So on stale evidence it does two things and returns whether takeovers are
// currently allowed:
//
//  1. Repairs: renews synchronously, right now, before claiming. Renewal matches on
//     worker_id, so it re-stamps only rows nobody took while this worker was away; a row
//     another worker claimed meanwhile stays theirs. In the ordinary single-worker case
//     this restores every lease and the claim below simply does not match them, so an
//     in-flight advance is never disturbed.
//  2. Opens a grace window: for one lease duration the pump claims only unowned rows. A
//     host that suspends suspends every worker on it, and on wake they all race; this
//     worker cannot repair a peer's leases, but it can decline to take them while the peer
//     repairs its own. Unowned work keeps flowing, so a graced worker is not idle.
//
// The check lives here, in the claimant, rather than in the renewer, because on resume
// every frozen ticker fires at once and the two race. Reading the evidence at claim time
// makes both orderings correct: if the renewer got there first the stamp is fresh and this
// is a no-op; if it did not, the repair happens here.
//
// A stale stamp repairs every time it is seen, with no "once per window" bound. The only
// way to see one twice is for the repair itself to keep failing, which means the database
// is unreachable — and in that state the pump's own claim is failing once per poll
// already, so a second failing query changes nothing worth a special case.
func (e *Engine) leaseGate() db.Takeover {
	nowMs := db.Now().UnixMilli()
	stale := time.Duration(nowMs-e.lastRenewMs.Load()) * time.Millisecond

	// A lease stamped at lastRenew dies at lastRenew+leaseDuration, so staleness reaching
	// the lease duration is exactly the moment the evidence runs out. Trip one poll early:
	// the gate is only consulted once per cycle, and without that margin the evidence
	// expires between two checks and the claim in between is the one that re-claims this
	// worker's own in-flight rows. The margin is capped at half the lease so a poll
	// interval longer than the lease — a broken config — cannot park the worker in a
	// permanent grace, which would stop it recovering genuinely dead workers' rows.
	margin := e.pollEvery
	if cap := e.leaseDuration / 2; margin > cap {
		margin = cap
	}
	if stale+margin < e.leaseDuration {
		// Evidence is good. A grace opened by an earlier trip still applies until it
		// runs out — peers that froze alongside us have not necessarily repaired yet.
		if nowMs < e.graceUntilMs {
			return db.SkipTakeover
		}
		return db.AllowTakeover
	}

	// Log once per window rather than once per poll: while the condition persists the
	// window is extended below, and the operator learns nothing from the repetition.
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
// shutdown or an *OverwhelmError if the pump could not keep up with its leases (in-flight
// work is drained before it returns either way). When pollEvery is zero the engine does
// not auto-tick; call Tick explicitly.
func (e *Engine) Run(ctx context.Context) error {
	e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine started", Meta: map[string]any{"poll_interval": e.pollEvery, "max_concurrent": cap(e.sem), "worker": e.workerID}})

	// Seed the renewal evidence before the renewer's first tick, so a freshly started
	// engine — which holds no leases yet — is not read as stale by its first claim.
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

// runPump is the continuous claim/dispatch loop used when pollEvery > 0. Unlike Tick (a
// synchronous batch with a wg.Wait barrier), the pump never waits for a batch to finish:
// it tops up work as worker slots free, so a slow instance never stalls the others.
//
// e.sem doubles as the idle detector and the concurrency bound: reserving a slot blocks
// exactly when all workers are busy and wakes the instant one frees, giving backpressure
// and immediate top-up. When a claim finds nothing the pump waits on the ticker. A
// WaitGroup drains in-flight advances on shutdown.
func (e *Engine) runPump(ctx context.Context) error {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	var wg sync.WaitGroup
	defer wg.Wait() // stop claiming, finish in-flight advances, then return

	for {
		// Block for one slot (wakes the instant a worker frees), then grab any
		// other free slots without blocking. Acquiring all slots up front means the
		// dispatch loop below never blocks: combined with the claim's
		// wait_state<>'waiting' filter, that closes the window where an in-flight
		// advance could finish between the claim and the dispatch guard and let a
		// stale snapshot through. slots is the exact claim limit, so in-flight never
		// exceeds maxConcurrent.
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

		// Check the lease evidence before claiming, never after: the whole point is to
		// repair lapsed leases while there is still a claim to protect from them.
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
// when done (the caller must have already reserved it). It reports whether it started an
// advance: an instance already in-flight on this worker is never advanced twice, and
// returns either an *OverwhelmError (the caller stops the pump and drains) or false with
// no error when a grace window explains the re-claim (the caller releases the slot and
// carries on). graced is the lease gate's verdict for this claim.
func (e *Engine) dispatch(ctx context.Context, wg *sync.WaitGroup, inst *model.ProcessInstance, graced bool) (bool, error) {
	// If we just re-claimed an instance this worker is still advancing, its lease
	// expired before the advance finished: lease renewal can't keep up, so the
	// engine is overwhelmed. This is inherent to a lease-based design — in a
	// multi-worker deployment another worker would already have stolen and
	// double-executed the instance. There is no reliable way to recover, so we
	// stop the pump and surface the error rather than silently corrupting state.
	// The operator should lower --max-concurrent or increase the lease duration.
	//
	// The marker is exact about the in-flight part: runAdvance drops it only just before
	// the write that frees the instance, so an in-flight instance is claimable only once
	// its lease has actually expired. A re-claim that still finds the marker is therefore
	// a real lapsed lease, never an advance that already finished.
	//
	// Inside a grace window it is not a verdict about capacity, though. The gate has just
	// established that this worker was not in a position to renew — it was frozen, or cut
	// off from the DB — and repaired what it could; a re-claim that slips through anyway
	// (a stall that landed inside the claim itself, say) is that same event, not a
	// misconfigured --max-concurrent. Leave the instance to the advance already running
	// it and keep pumping: killing the worker over a resumed laptop would throw away every
	// other in-flight advance with it.
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
	// No recover() here on purpose: the barrier sits one level down, around advance()
	// itself (advanceGuarded), where the panicking instance is still in hand and can be
	// failed with errcode.EnginePanic instead of merely logged. What reaches this
	// goroutine is therefore a panic from the persist path, which is not attributable to
	// any one definition — and that one is still meant to take the worker down.
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

// leaseRenewer renews all leases held by this worker in a single query every
// leaseRenewInterval, in its own goroutine so renewals are never blocked by a long tick.
// Each success re-stamps lastRenewMs, which is what the pump's lease gate reads.
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

// logPruner periodically deletes audit-log rows older than the retention window (only
// started when retention > 0). The cutoff uses the DB clock so a clock shift (e.g. via
// /tick in tests) expires logs without a real wait.
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
	// Sweep dereferenced/expired objects (log payloads and dropped context values) on
	// the same horizon — their expiration was stamped to now+retention when released.
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
	// No lease gate here: a manual tick is a synchronous batch that waits for every
	// advance it starts, so it cannot re-claim its own in-flight work, and there is no
	// pump whose evidence could go stale between ticks.
	instances, err := e.db.ClaimInstances(e.workerID, e.leaseDuration, cap(e.sem), db.AllowTakeover)
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
