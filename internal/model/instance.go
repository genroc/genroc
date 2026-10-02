package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Status is an instance's lifecycle state. failing, pausing and cancelling drain descendants or
// an in-flight task, so a failed root implies a settled tree. paused is not an outcome: wait
// state and timers are kept verbatim, so resuming is a status flip. specs/child-error-handling.md.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailing   Status = "failing" // doomed by an error, draining descendants
	StatusFailed    Status = "failed"
	StatusRaised    Status = "raised"  // concluded by a `raise` clause; catchable by the parent
	StatusPausing   Status = "pausing" // pause requested, still holding an in-flight task
	StatusPaused    Status = "paused"

	// cancelled is the one settled outcome an operator produces; unlike pause, it has no way
	// back. specs/pause-resume.md.
	StatusCancelling Status = "cancelling" // cancel requested, still holding an in-flight task
	StatusCancelled  Status = "cancelled"
)

// Terminal reports whether the status is a settled outcome. raised must count, or RetryProcess
// parks a revived parent in 'children' forever. The SQL copies (CountActiveSiblings in
// queries.sql) are kept in step by hand.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusRaised || s == StatusCancelled
}

// Outcome is what a lifecycle assertion (pause, resume) did, decided under the lock that read
// the tree; re-reading afterwards may see a tree that moved. specs/id-list-commands.md.
type Outcome string

const (
	// OutcomeApplied — the assertion holds, and this call is what made it hold.
	OutcomeApplied Outcome = "applied"
	// OutcomeAccepted — recorded, not yet in effect: a pause left rows 'pausing',
	// because a worker holds a task that runs to its next boundary.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeUnchanged — the assertion already held; nothing was written. Not an error: it is
	// what makes re-running a partially applied sweep converge.
	OutcomeUnchanged Outcome = "unchanged"
)

// AcceptsExternalOutcome: a pause suspends execution, not delivery — the claim side refuses a
// paused tree instead. Refusing the cancel states is what tells the worker to stop.
// specs/external-task-queue.md §Pause.
func (s Status) AcceptsExternalOutcome() bool {
	return s == StatusRunning || s == StatusPaused || s == StatusPausing
}

// StateErrorData holds the payload a `raise` or `panic` attached. Never `last_error`, which is
// the instance's own state. specs/error-extensions.md.
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

// Phase says why a running instance is not simply executing its task. The phases are not one
// kind: Children is blocked (the claim skips it), Collecting is RUNNABLE, External is parked.
type Phase string

const (
	PhaseNone       Phase = ""           // running a task; not in a child or external cycle
	PhaseChildren   Phase = "children"   // children spawned, blocked until they settle
	PhaseCollecting Phase = "collecting" // all children terminal, their outputs still to merge
	PhaseExternal   Phase = "external"   // parked on an external task, awaiting a result (or timeout)
)

// ExternalToken answers an external task: the instance plus the arming's epoch. Derived, never
// stored, not a secret; a row under a live claim refuses it and accepts only ClaimToken.
func ExternalToken(instanceID string, taskEpoch int64) string {
	return fmt.Sprintf("%s.%d", instanceID, taskEpoch)
}

// ClaimToken is ExternalToken plus the claim epoch, which keeps an expired holder's late
// answer out: successive claims of one arming share a task_epoch.
func ClaimToken(instanceID string, taskEpoch, claimEpoch int64) string {
	return fmt.Sprintf("%s.%d.%d", instanceID, taskEpoch, claimEpoch)
}

// ParseExternalToken accepts both forms. The first dot is the instance boundary because
// internal/idgen's alphabet has no '.'.
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

// StateExternalInput holds the parked external task's evaluated input, unwrapped. One spelling
// at every layer (column, context key, objects path root, API field), or an objects path cannot
// be placed on read. specs/object-store.md.
const StateExternalInput = "external_input"

// ProcessInstance is one execution of a ProcessDefinition, pinned to ProcessVersion.
type ProcessInstance struct {
	ID             string
	ProcessName    string
	ProcessVersion int

	// Task is the current task; the rest of the queue is the pinned definition's tasks from here,
	// never stored. Empty means the instance ran off the end.
	Task string

	// The external-task CLAIM. Not WorkerID/LeaseExpiresAt/LeaseEpoch, which mean a worker is
	// advancing this instance. specs/external-task-queue.md.
	ExternalWorkerID       *string
	ExternalLeaseExpiresAt *time.Time
	ExternalClaimEpoch     int64
	// ExternalLost marks an only_once arming whose claim lapsed unanswered; the engine reports it
	// as errcode.ExternalLost. A marker, not derived: external_worker_id cannot say whether the
	// lapse was already reported.
	ExternalLost bool

	// State is the definition's slots plus engine bookkeeping, nothing derivable. The key set is
	// CLOSED: storage drops any key it does not name. Not "context", which adds config and self.
	State map[string]any

	// ParentID is set when this instance was started by a child_process task.
	// Empty string means this is a root instance.
	ParentID string

	// SpawnTaskID is the parent task that spawned this instance, "" for a root. With
	// ParentTaskEpoch it scopes sibling queries to one spawn batch.
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
	Phase      Phase

	// ErrorMessage, ErrorCode and _error_data in State are the error this instance REPORTS, each
	// named for its column so one concept has one spelling.
	ErrorMessage string

	// ErrorCode is the discriminator for every non-success outcome, "" when completed. Authored
	// codes never contain a dot and engine codes always do.
	ErrorCode string

	CreatedAt      time.Time
	UpdatedAt      time.Time
	WorkerID       *string
	LeaseExpiresAt *time.Time

	// NextReplayable: Task is not only_once, denormalised so the claim path need not resolve a
	// definition. Stated this way round so the unset false is the safe value.
	// specs/durability-levels.md s4.
	NextReplayable bool

	// LeaseEpoch fences every lease-holding write: a superseded grant's is refused
	// (db.ErrLeaseLost) instead of clobbering. specs/lease-fencing.md.
	LeaseEpoch int64

	// TaskEpoch numbers task ENTRIES: it moves on every transition, even a goto to the same task,
	// and not while parked, so a child task's spawn and collect share one.
	TaskEpoch int64

	// ParentTaskEpoch is the parent's TaskEpoch at spawn, zero for a root: (parent_id,
	// spawn_task_id) alone repeats every time a loop re-enters the task.
	ParentTaskEpoch int64

	// Config is resolved from the OS environment each tick (ProcessDefinition.ResolveConfig) and
	// never persisted or returned over the API, so secrets stay out of stored state.
	Config map[string]any `json:"-"`

	// ReclaimedExpired is set by ClaimInstances when a prior lease expired rather than ending at a
	// task boundary, so the current task may have been interrupted. Transient.
	ReclaimedExpired bool

	// ExternalReclaimed is ReclaimedExpired for the external-task claim. Transient; on an only_once
	// task it is what stops the work being handed out a second time.
	ExternalReclaimed bool

	// LoadedObjectHashes is what the value slots referenced at read; the write path diffs against
	// it to dereference objects no slot points at any more. Transient.
	LoadedObjectHashes map[string]struct{} `json:"-"`

	// ConsumedSignalID is deleted in the SAME transaction as the state it produced: popping first
	// loses it if the write is refused, popping after applies it twice. Transient.
	// specs/external-outcome-as-signal.md.
	ConsumedSignalID string `json:"-"`

	// ResolvedObjects memoises object lookups by hash for the current advance. Transient.
	ResolvedObjects map[string]any `json:"-"`
}

// InstanceSummary is the list projection: it omits the heavy JSON blobs (context_data,
// call_stack), which only GetInstance loads.
type InstanceSummary struct {
	ID string
	// ParentID is "" for a root; nothing else on the row says whether it is a child.
	ParentID       string
	ProcessName    string
	ProcessVersion int
	RetryCount     int
	Status         Status
	Phase          Phase
	// Task is where the instance runs, parks or finished — the one fact status and phase cannot
	// express between them.
	Task      string
	Error     string
	ErrorCode string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Holds is what an action leaves persisted across advances — the state an instance SITS in,
// beyond its entry context. Shared by the advance switch, the version comparison and Phase.
type Holds struct {
	// Wait is the state the instance parks in, or PhaseNone for an action that does not.
	Wait Phase
	// Timer is true where the action leaves a wake_at the engine will claim on.
	Timer bool
	// Result is a VALUE the entry context does not describe (an external result, children's
	// outputs): what makes a result schema part of the upgrade question.
	Result bool
}

// Anything reports whether an instance can be sitting in this action at all.
func (h Holds) Anything() bool { return h != Holds{} }

// Holds must decide every ActionType: falling through to the zero value claims it never holds
// an instance, which silently stops the version comparison reporting a type change under it.
func (t ActionType) Holds() Holds {
	switch t {
	case ActionTypeExternal:
		return Holds{Wait: PhaseExternal, Timer: true, Result: true}
	case ActionTypeChild, ActionTypeChildMap, ActionTypeChildList:
		return Holds{Wait: PhaseChildren, Result: true}
	case ActionTypeDelay:
		// Holds a live instance but no data, which is why it is in some rules and not others.
		return Holds{Timer: true}
	case ActionTypeFetch:
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
