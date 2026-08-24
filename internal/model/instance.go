package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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

// AcceptsExternalOutcome reports whether a submitted result or failure may be delivered to
// an instance in this status. A pause suspends execution, not delivery: refusing here would
// discard work an outside caller has already performed, and on an only_once task the
// external.timeout that follows can never be retried. The claim side refuses a suspended
// tree instead. specs/external-task-queue.md §Pause.
func (s Status) AcceptsExternalOutcome() bool {
	return s == StatusRunning || s == StatusPaused || s == StatusPausing
}

// WaitState tracks where a parent instance is in the child-process lifecycle.
type WaitState string

const (
	WaitStateNone       WaitState = ""           // not in a child-process wait cycle
	WaitStateWaiting    WaitState = "waiting"    // children spawned, waiting for them
	WaitStateCollecting WaitState = "collecting" // all children terminal, collect their outputs
	WaitStateExternal   WaitState = "external"   // parked on an external task, waiting for a submitted result (or timeout)
)

// ExternalToken is the handle a caller submits to answer an external task: the instance, plus
// the epoch of the ARMING the answer belongs to. Derived on demand and never stored --
// task_epoch on the instance row is the occurrence, so a copy in external_data would only be a
// second thing that can disagree with it. Not a secret: the queue endpoint hands it to any
// caller, and it is an occurrence discriminator rather than a capability.
//
// This two-part form is the UNCLAIMED handle. A claim grants the three-part ClaimToken, and a
// row under a live claim accepts only that -- see ParseExternalToken.
func ExternalToken(instanceID string, taskEpoch int64) string {
	return fmt.Sprintf("%s.%d", instanceID, taskEpoch)
}

// ClaimToken is the handle ClaimExternalTasks grants: ExternalToken plus the claim epoch.
// The extra field is not decoration. Two workers can claim the same ARMING in sequence -- the
// first claim expires, the second is granted -- and task_epoch does not move for either, so
// without the claim epoch the dead worker's handle stays valid and its late answer would be
// accepted over the live holder's.
func ClaimToken(instanceID string, taskEpoch, claimEpoch int64) string {
	return fmt.Sprintf("%s.%d.%d", instanceID, taskEpoch, claimEpoch)
}

// ParseExternalToken accepts both forms. hasClaim distinguishes them: false is a caller
// answering unclaimed work (the queue-then-resolve path a UI uses), true a claim holder
// naming the grant it is answering under.
//
// Instance ids are UUIDs and carry no '.', so the first dot is the instance boundary.
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

// Private context_data keys used by the external-task lifecycle. Underscore-prefixed
// like _children / _spawn_* so they are clearly engine-internal bookkeeping.
const (
	// CtxExternal holds the parked external task's metadata: {task_id, input}. The queue
	// endpoint reads input from here and derives the token from the row's task_epoch;
	// never exposed as process output.
	CtxExternal = "_external"
	// CtxExternalLost, inside _external, marks an arming whose holder's claim lapsed without an
	// answer on an only_once task. It is written INSTEAD of handing the work out again, and the
	// engine turns it into errcode.ExternalLost on its next claim. A marker rather than a
	// derivation: external_worker_id alone cannot say whether the lapse was already reported.
	CtxExternalLost = "lost"
	// CtxExternalResult holds a submitted, validated result placed by the resolve API.
	// Its presence is how the engine tells "result arrived" from "first arrival".
	CtxExternalResult = "_external_result"
	// CtxExternalError holds a submitted failure placed by the fail API: {code, message,
	// data?}, where `data` is present only if the task declared a shape for the code. The
	// engine routes it through on_error on the next claim -- it cannot be routed where it
	// is submitted, because retry budgeting is a write on the leased row. Checked BEFORE
	// CtxExternalResult, so the two can never both apply to one arming.
	CtxExternalError = "_external_error"
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

	// ExternalWorkerID / ExternalLeaseExpiresAt / ExternalClaimEpoch are the external-task
	// CLAIM: a worker holding a parked task for a visibility timeout. Deliberately not the
	// engine's WorkerID/LeaseExpiresAt/LeaseEpoch, which mean an engine worker is ADVANCING
	// this instance -- a claim means the opposite, that it is parked and no worker is held.
	// specs/external-task-queue.md.
	ExternalWorkerID       *string
	ExternalLeaseExpiresAt *time.Time
	ExternalClaimEpoch     int64

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

	// ErrorCode is the machine-readable discriminator for every non-success outcome: an
	// authored raise/panic code, or the engine's own. Empty when completed. Authored codes
	// never contain a dot and engine codes always do, so the namespaces stay legible.
	ErrorCode string

	CreatedAt      time.Time
	UpdatedAt      time.Time
	WorkerID       *string
	LeaseExpiresAt *time.Time

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

	// LoadedObjectHashes is the set of process_objects hashes the value-slots
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
	ID             string
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

// Holds is what an action leaves persisted when it does not finish inside one advance —
// the state an instance is SITTING in, as opposed to the entry context every task has.
//
// It is one declaration for a fact three places used to encode separately: the engine's
// advance switch, the version comparison's rules about what may change under an instance,
// and this file's own WaitState vocabulary. A replay or a time-travel debugger needs the
// same answer — what must be stored to resume at a point — so it belongs here rather than
// in whichever caller asked first.
//
// The zero value means the action runs to completion inside one advance and leaves nothing:
// an instance at such a task is always at ENTRY.
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
