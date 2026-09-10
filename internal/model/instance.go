package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Status represents the lifecycle state of a process instance. failing and pausing are
// draining states: the outcome is decided but descendants (or an in-flight task) are still
// settling, so a failed root implies the whole tree has settled. paused is not an outcome —
// the instance keeps its wait state and timers verbatim, so resuming is a status flip.
// raised is the third settled outcome: a concluded, non-poisoning, non-retryable condition a
// parent may catch by code. specs/child-error-handling.md.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailing   Status = "failing" // doomed by an error, draining descendants
	StatusFailed    Status = "failed"
	StatusRaised    Status = "raised"  // concluded by a `raise` clause; catchable by the parent
	StatusPausing   Status = "pausing" // pause requested, still holding an in-flight task
	StatusPaused    Status = "paused"

	// cancelled is the terminal stop, and the one settled outcome an operator produces
	// rather than the definition. It is deliberately NOT reachable from `paused` in the
	// other direction: pause has a way back and this does not. specs/pause-resume.md.
	StatusCancelling Status = "cancelling" // cancel requested, still holding an in-flight task
	StatusCancelled  Status = "cancelled"
)

// Terminal reports whether the status is a settled outcome. paused is live work that simply
// is not being advanced; raised counts, or RetryProcess parks a revived parent in 'waiting'
// forever. The SQL copies of this predicate must be kept in step by hand — see
// CountActiveSiblings in queries.sql.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusRaised || s == StatusCancelled
}

// Outcome is what a lifecycle assertion (pause, resume) did. It sits beside Status
// because it is a domain fact the db layer decides under the lock that read the tree —
// a caller re-reading afterwards would be answering from a tree that may have moved.
// specs/id-list-commands.md.
type Outcome string

const (
	// OutcomeApplied — the assertion holds, and this call is what made it hold.
	OutcomeApplied Outcome = "applied"
	// OutcomeAccepted — recorded, not yet in effect: a pause left rows 'pausing',
	// because a worker holds a task that runs to its next boundary.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeUnchanged — the assertion already held; nothing was written. Not an error:
	// re-asserting a state a tree is already in is what makes a partially applied sweep
	// converge when it is run again.
	OutcomeUnchanged Outcome = "unchanged"
)

// AcceptsExternalOutcome reports whether a submitted result or failure may be delivered to an
// instance in this status. A pause suspends execution, not delivery — the claim side refuses a
// suspended tree instead. The cancel states are absent because that answer would never be
// read, and refusing is what tells the worker to stop. specs/external-task-queue.md §Pause.
func (s Status) AcceptsExternalOutcome() bool {
	return s == StatusRunning || s == StatusPaused || s == StatusPausing
}

// ErrorDataKey is the slot holding the payload a `raise` or `panic` attached; its code and
// message are plain columns beside it. It is never the `last_error` slot, which belongs to the
// instance's state — a concluding fault editing that leaves a context no layer describes.
// specs/error-extensions.md.
const StateErrorData = "_error_data"

// The two failures a task's expressions can name, kept apart because they are different
// errors: one routed control here, the other is being handled right now. specs/task-scopes.md.
const (
	// StateLastError is persisted, and dropped on the next ordinary transition.
	StateLastError = "last_error"
	// StateError is bound for the evaluation of the rule handling it and never written —
	// engineStateKeys drops it, so a binding that outlives its rule cannot reach a column.
	StateError = "error"
)

// WaitState tracks where a parent instance is in the child-process lifecycle.
type WaitState string

const (
	WaitStateNone       WaitState = ""           // not in a child-process wait cycle
	WaitStateWaiting    WaitState = "waiting"    // children spawned, waiting for them
	WaitStateCollecting WaitState = "collecting" // all children terminal, collect their outputs
	WaitStateExternal   WaitState = "external"   // parked on an external task, waiting for a submitted result (or timeout)
)

// ExternalToken is the handle a caller submits to answer an external task: the instance plus
// the epoch of the arming the answer belongs to. Derived on demand, never stored, and not a
// secret — it discriminates occurrences rather than granting anything. This unclaimed form is
// refused by a row under a live claim, which accepts only the three-part ClaimToken.
func ExternalToken(instanceID string, taskEpoch int64) string {
	return fmt.Sprintf("%s.%d", instanceID, taskEpoch)
}

// ClaimToken is the handle ClaimExternalTasks grants: ExternalToken plus the claim epoch.
// Two workers can claim the same arming in sequence without task_epoch moving, so the claim
// epoch is what keeps the expired holder's late answer out.
func ClaimToken(instanceID string, taskEpoch, claimEpoch int64) string {
	return fmt.Sprintf("%s.%d.%d", instanceID, taskEpoch, claimEpoch)
}

// ParseExternalToken accepts both forms; hasClaim=false is a caller answering unclaimed work,
// true a claim holder naming its grant. An id carries no '.' (internal/idgen's alphabet
// excludes it), so the first dot is the instance boundary.
func ParseExternalToken(token string) (instanceID string, taskEpoch, claimEpoch int64, hasClaim, ok bool) {
	id, rest, found := strings.Cut(token, ".")
	if !found || id == "" {
		return "", 0, 0, false, false
	}
	epochStr, claimStr, hasClaim := strings.Cut(rest, ".")
	n, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil || n < 0 {
		return "", 0, 0, false, false
	}
	if !hasClaim {
		return id, n, 0, false, true
	}
	c, err := strconv.ParseInt(claimStr, 10, 64)
	if err != nil || c < 0 {
		return "", 0, 0, false, false
	}
	return id, n, c, true, true
}

// Engine-owned STATE keys for the external-task lifecycle. Underscore-prefixed like
// _spawn_* so they are clearly bookkeeping and not a definition's to read.
const (
	// StateExternal holds the parked external task's metadata: {task_id, input}. The queue
	// endpoint reads input from here and derives the token from the row's task_epoch;
	// never exposed as process output.
	StateExternal = "_external"
	// StateExternalLost, inside _external, marks an arming whose holder's claim lapsed without an
	// answer on an only_once task. It is written INSTEAD of handing the work out again, and the
	// engine turns it into errcode.ExternalLost on its next claim. A marker rather than a
	// derivation: external_worker_id alone cannot say whether the lapse was already reported.
	StateExternalLost = "lost"
)

// ProcessInstance is a single running execution of a ProcessDefinition.
// ProcessVersion is pinned at creation — process definition changes
// never affect existing instances.
type ProcessInstance struct {
	ID             string
	ProcessName    string
	ProcessVersion int

	// Task is the id of the instance's current task. The remaining queue is not stored: it is
	// the definition's tasks from here onward (immutable and version-pinned), and a switch only
	// moves this pointer. Empty means the instance ran off the end.
	Task string

	// The external-task CLAIM: a worker holding a parked task for a visibility timeout.
	// Deliberately not the engine's WorkerID/LeaseExpiresAt/LeaseEpoch, which mean the
	// opposite -- that a worker is advancing this instance. specs/external-task-queue.md.
	ExternalWorkerID       *string
	ExternalLeaseExpiresAt *time.Time
	ExternalClaimEpoch     int64

	// State is everything this instance holds: the slots a definition reads plus the engine's
	// bookkeeping (_error_data, _external, _spawn_*). The set is CLOSED -- storage names these
	// keys and drops the rest -- and nothing derivable belongs here. Not "context", which is
	// the expression scope: these slots plus config and self.
	State map[string]any

	// ParentID is set when this instance was started by a child_process task.
	// Empty string means this is a root instance.
	ParentID string

	// SpawnTaskID is the ID of the parent task that spawned this instance.
	// Empty string for root instances. Scopes sibling queries to one spawn batch
	// so consecutive spawn tasks under the same parent never mix.
	SpawnTaskID string

	// RootID is the tree this instance belongs to -- its own id when it is a root. Derived
	// from parent_id by the INSERT, so a tree is an indexed lookup rather than a walk.
	RootID string

	// CallStack is the ordered list of ancestor instance IDs (root first).
	// Used for O(1) ancestor lookup during error cascade.
	CallStack []string

	RetryCount int
	WakeAt     *time.Time
	Status     Status
	WaitState  WaitState

	// ErrorMessage is the human half of the error this instance REPORTS; ErrorCode is the
	// machine half and _error_data in State is the payload. Named for its column, like the
	// other two, so one concept does not answer to three spellings.
	ErrorMessage string

	// ErrorCode is the machine-readable discriminator for every non-success outcome: an
	// authored raise/panic code, or the engine's own. Empty when completed. Authored codes
	// never contain a dot and engine codes always do, so the namespaces stay legible.
	ErrorCode string

	CreatedAt      time.Time
	UpdatedAt      time.Time
	WorkerID       *string
	LeaseExpiresAt *time.Time

	// NextReplayable is whether the task Task names may simply be re-run after a crash (i.e.
	// it is NOT only_once), denormalised so the claim path can decide to harden without
	// resolving a definition. Stated in the replayable direction so that false -- what a
	// caller that never set it gets -- is the safe value. specs/durability-levels.md s4.
	NextReplayable bool

	// LeaseEpoch is the fencing token this instance was granted under: bound into every
	// lease-holding write, so a superseded grant's write is refused (db.ErrLeaseLost)
	// instead of clobbering. specs/lease-fencing.md.
	LeaseEpoch int64

	// TaskEpoch numbers this instance's task ENTRIES. It moves on a transition (next, a
	// goto, including one back to the same task) and stays put while the instance is parked
	// on a task -- so a child task's spawn and its collect are the same epoch, which is what
	// lets a batch be addressed. Distinct from LeaseEpoch, which is a worker grant.
	TaskEpoch int64

	// ParentTaskEpoch is the parent's TaskEpoch this instance was spawned under; zero for a
	// root. It is what makes one batch of children addressable, since (parent_id,
	// spawn_task_id) repeats every time a loop re-enters the task.
	ParentTaskEpoch int64

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

	// ExternalReclaimed is ReclaimedExpired for the external-task claim: set by
	// ClaimExternalTasks when this row already carried an external_worker_id, i.e. a previous
	// holder's claim lapsed without an answer. Transient and never persisted. On an only_once
	// task it is what stops the work being handed out a second time.
	ExternalReclaimed bool

	// LoadedObjectHashes is the set of object hashes the value-slots
	// (input/outputs/output) referenced when this instance was read. The write path
	// diffs it against the slots' current references to dereference objects a slot no
	// longer points at. Transient, never persisted.
	LoadedObjectHashes map[string]struct{} `json:"-"`

	// ConsumedSignalID is the buffered signal this advance decided to act on. persist deletes
	// it in the SAME transaction as the state it produced: popping first loses the answer if
	// the write is refused, popping after applies it twice if the delete is. Transient, never
	// persisted. specs/external-outcome-as-signal.md.
	ConsumedSignalID string `json:"-"`

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
	ID string
	// ParentID is "" for a root. Carried in the light projection because a listing that
	// includes children is otherwise uninterpretable -- nothing else on the row says
	// whether it is one.
	ParentID       string
	ProcessName    string
	ProcessVersion int
	RetryCount     int
	Status         Status
	WaitState      WaitState
	// Task is the instance's position in its task list — where it is running, parked, or where
	// it finished. Cheap, and the one "where is this process" fact status and wait_state cannot
	// express between them, so unlike the JSON blobs it belongs in the light projection.
	Task      string
	Error     string
	ErrorCode string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Holds is what an action leaves persisted when it does not finish inside one advance — the
// state an instance is SITTING in, as opposed to the entry context every task has. One
// declaration shared by the advance switch, the version comparison and WaitState. The zero
// value means the action finishes inside one advance, so the instance is always at ENTRY.
type Holds struct {
	// Wait is the state the instance parks in, or WaitStateNone for an action that does not.
	Wait WaitState
	// Timer is true where the action leaves a wake_at the engine will claim on.
	Timer bool
	// Result is true where the action leaves a VALUE the entry context does not describe —
	// a submitted external result, or children's outputs to collect. This is the half that
	// makes a result schema part of the upgrade question and not only the contract one.
	Result bool
}

// Anything reports whether an instance can be sitting in this action at all.
func (h Holds) Anything() bool { return h != Holds{} }

// Holds answers for one action type. Every ActionType must appear: a new one that falls
// through to the zero value is claiming it can never hold an instance, which is the
// dangerous direction — a version comparison would stop reporting a type change under it,
// silently. TestHolds_EveryActionTypeIsDecided is what makes the omission loud.
func (t ActionType) Holds() Holds {
	switch t {
	case ActionTypeExternal:
		// Parks until an outside caller submits, and the result is stored on the row.
		return Holds{Wait: WaitStateExternal, Timer: true, Result: true}
	case ActionTypeChild, ActionTypeChildMap, ActionTypeChildList:
		// Spawns children and waits for them; their outputs are collected afterwards.
		return Holds{Wait: WaitStateWaiting, Result: true}
	case ActionTypeDelay:
		// A timer and nothing else — WaitStateNone with a wake_at. It holds a live instance
		// without holding any data, which is why it is in some rules and not others.
		return Holds{Timer: true}
	case ActionTypeFetch:
		// Request and response happen inside one advance, with nothing persisted between.
		return Holds{}
	}
	return Holds{}
}

// AllActionTypes is every action type, for the tests that must enumerate them. The decoder
// rejects anything not in this list, so a new type reaches here or it reaches nothing.
var AllActionTypes = []ActionType{
	ActionTypeFetch, ActionTypeChild, ActionTypeChildMap,
	ActionTypeChildList, ActionTypeDelay, ActionTypeExternal,
}
