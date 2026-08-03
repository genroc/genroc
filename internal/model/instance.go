package model

import "time"

// Status represents the lifecycle state of a process instance.
//
// failing is a draining state: the outcome is decided but descendants are still
// settling. A node only becomes failed once all its direct children are terminal,
// so a failed root implies the whole tree has settled — which is what makes failed
// roots retryable.
//
// paused is not an outcome: it means only "does not continue automatically". The
// instance keeps its wait_state, wake_at, retry_count and context verbatim, and its
// timers keep running, so resuming is a status flip rather than a revival. pausing
// is its draining state — a leased instance that lands in paused once the in-flight
// task it is holding finishes.
//
// raised is the third settled outcome, produced by a `raise` clause: an anticipated
// condition a parent may react to by naming the code. Neither completed (it produced no
// output) nor failed (it does not poison ancestors), and not retryable — a raise is a
// conclusion, not an interruption. See specs/child-error-handling.md.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailing   Status = "failing" // doomed by an error, draining descendants
	StatusFailed    Status = "failed"
	StatusRaised    Status = "raised"  // concluded by a `raise` clause; catchable by the parent
	StatusPausing   Status = "pausing" // pause requested, still holding an in-flight task
	StatusPaused    Status = "paused"
)

// Terminal reports whether the status is a settled outcome. paused and pausing are
// not terminal: a paused instance is live work that simply is not being advanced.
//
// raised counts: it is settled work, which is what RetryProcess's wait-state
// reconstruction asks about. Omitting it parks a revived parent in 'waiting' forever,
// waiting on a child that has already concluded. The SQL copies of this predicate are
// separate and must be kept in step by hand — see CountActiveSiblings in queries.sql.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusRaised
}

// WaitState tracks where a parent instance is in the child-process lifecycle.
type WaitState string

const (
	WaitStateNone       WaitState = ""           // not in a child-process wait cycle
	WaitStateWaiting    WaitState = "waiting"    // children spawned, waiting for them
	WaitStateCollecting WaitState = "collecting" // all children terminal, collect their outputs
	WaitStateExternal   WaitState = "external"   // parked on an external task, waiting for a submitted result (or timeout)
)

// Private context_data keys used by the external-task lifecycle. Underscore-prefixed
// like _children / _spawn_* so they are clearly engine-internal bookkeeping.
const (
	// CtxExternal holds the parked external task's metadata: {task_id, token, input}.
	// The queue endpoint reads token+input from here; never exposed as process output.
	CtxExternal = "_external"
	// CtxExternalResult holds a submitted, validated result placed by the resolve API.
	// Its presence is how the engine tells "result arrived" from "first arrival".
	CtxExternalResult = "_external_result"
)

// ProcessInstance is a single running execution of a ProcessDefinition.
// ProcessVersion is pinned at creation — process definition changes
// never affect existing instances.
type ProcessInstance struct {
	ID             string
	ProcessName    string
	ProcessVersion int

	// Task is the id of the instance's current task — its position in the definition's
	// ordered task list. The remaining "queue" is not stored: it is always the
	// definition's tasks from here onward (the list is immutable and version-pinned),
	// and a switch only ever moves this pointer — to the next task or to a goto target.
	// An empty Task means the instance has run off the end (completed / nothing left).
	Task string

	// ContextData is the accumulated key/value state passed between tasks.
	ContextData map[string]any

	// ParentID is set when this instance was started by a child_process task.
	// Empty string means this is a root instance.
	ParentID string

	// SpawnTaskID is the ID of the parent task that spawned this instance.
	// Empty string for root instances. Scopes sibling queries to one spawn batch
	// so consecutive spawn tasks under the same parent never mix.
	SpawnTaskID string

	// CallStack is the ordered list of ancestor instance IDs (root first).
	// Used for O(1) ancestor lookup during error cascade.
	CallStack []string

	RetryCount int
	WakeAt     *time.Time
	Status     Status
	WaitState  WaitState
	Error      string

	// ErrorCode is the machine-readable discriminator for every non-success outcome:
	// the authored code for a `raise` or `panic`, and the engine's own code
	// (http.500, pre.timeout, output.invalid, …) for a failure it detected itself.
	// Empty on a completed instance. It is what makes failures groupable and
	// alertable — the same information exists in Error, but only as prose.
	// Authored codes never contain a dot and engine codes always do, so the two
	// namespaces stay legible side by side without a discriminator column.
	ErrorCode string

	CreatedAt      time.Time
	UpdatedAt      time.Time
	WorkerID       *string
	LeaseExpiresAt *time.Time

	// Config is the configuration namespace resolved from the OS environment at
	// the start of each tick (see ProcessDefinition.ResolveConfig). It is exposed
	// to expressions as "config" but is transient: never persisted to the DB and
	// never returned over the API, so secret values stay out of stored state.
	Config map[string]any `json:"-"`

	// ReclaimedExpired is a transient, non-persisted flag set by ClaimInstances
	// when this instance was reclaimed from an expired lease (its prior worker_id
	// was non-null) rather than picked up at a clean task boundary. It signals that
	// the current task may have been interrupted mid-execution on the previous owner.
	ReclaimedExpired bool

	// LoadedObjectHashes is the set of process_objects hashes the value-slots
	// (input/outputs/output) referenced when this instance was read. The write path
	// diffs it against the slots' current references to dereference objects a slot no
	// longer points at. Transient, never persisted.
	LoadedObjectHashes map[string]struct{} `json:"-"`

	// ResolvedObjects memoises externalized-value lookups for the current advance,
	// keyed by object hash, so a slot referenced by several expressions loads once.
	// Transient, never persisted.
	ResolvedObjects map[string]any `json:"-"`
}

// InstanceSummary is the lightweight projection of a ProcessInstance used by list
// endpoints. It deliberately omits the heavy JSON blobs (context_data, call_stack)
// so listing many instances never fetches or unmarshals a potentially huge context —
// those are only loaded for single-instance detail (GetInstance).
type InstanceSummary struct {
	ID             string
	ProcessName    string
	ProcessVersion int
	RetryCount     int
	Status         Status
	WaitState      WaitState
	// Task is the instance's position in its definition's task list — where it is
	// running, parked or stopped, and on a settled one where it finished. Cheap (a
	// short string on the row) and the one piece of "where is this process" that
	// status and wait_state cannot express between them, so unlike the JSON blobs it
	// belongs in the light projection.
	Task      string
	Error     string
	ErrorCode string
	CreatedAt time.Time
	UpdatedAt time.Time
}
