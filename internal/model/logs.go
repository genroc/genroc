package model

import "time"

// LogLevel mirrors slog levels for a persisted log entry.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// LogLevelsAtLeast returns min and every level above it, or nil when min names no level.
//
// A level filter is a FLOOR, never an equality: `--level warn` that hid the errors above it
// would answer "is anything wrong here?" with silence. The severity order lives here because
// the stored value is the WORD -- 'error' < 'info' sorts wrong in every collation, so the
// column cannot answer this and a caller must turn the floor into the set.
func LogLevelsAtLeast(min LogLevel) []LogLevel {
	order := []LogLevel{LogDebug, LogInfo, LogWarn, LogError}
	for i, l := range order {
		if l == min {
			return order[i:]
		}
	}
	return nil
}

// Log event kinds emitted by the engine as it advances an instance. These are
// the stable machine-readable identifiers; the human message lives in Message.
//
// The LEVEL an event is written at says who is asking: info is the run's own story (what it was
// asked to do, what it sent, what came back, how it ended), debug is how the engine did it (which
// worker, the per-instance fan-out of a tree-wide call). Verbosity is not the test -- a payload
// too big for a line is the renderer's problem, not the level's. genctl logs floors at info.
const (
	EventInstanceCreated = "inst_created"
	EventWorkStarted     = "work_started"     // debug: one per ADVANCE -- a retry or resume emits it again
	EventActionStarted   = "action_started"   // an action call is about to be sent (request)
	EventActionSucceeded = "action_succeeded" // an action call returned successfully (response)
	EventActionFailed    = "action_failed"    // an action call returned an error (status + error body)
	EventTaskCompleted   = "task_completed"
	EventRetryScheduled  = "retry_scheduled"
	EventErrorRoute      = "error_routed"
	EventErrorCompleted  = "error_handled"
	EventInstanceDone    = "inst_completed"
	EventInstanceRaised  = "inst_raised" // concluded by a `raise` clause; the parent may react to the code
	EventInstanceFailed  = "inst_failed"
	EventInstanceSettled = "inst_settled"
	// EventInstanceUpgraded records a move to another definition version. The whole story is
	// in one entry: an upgrade writes no other trace, and the row it changed no longer says
	// which version it came from. specs/version-compatibility.md s4.
	EventInstanceUpgraded = "inst_upgraded"
	// Pause/resume fan out over a subtree, so per-instance entries are debug. Only pause gets
	// an info root entry, because only its outcome is deferred (meta.pausing counts the
	// drainers). The deferred pausing → paused landing is unlogged — see specs/pause-resume.md.
	EventPauseRequested   = "inst_pause_requested"
	EventPaused           = "inst_paused"
	EventPausing          = "inst_pausing"
	EventResumed          = "inst_resumed"
	EventCancelRequested  = "inst_cancel_requested"
	EventCancelled        = "inst_cancelled"
	EventCancelling       = "inst_cancelling"
	EventChildrenSpawned  = "child_spawned"
	EventChildrenCollect  = "child_collected"
	EventDelayArmed       = "delay_armed"
	EventExternalArmed    = "extern_armed"
	EventExternalResolved = "extern_resolved"
	EventExternalTimeout  = "extern_timeout"
	EventExternalFailed   = "extern_failed"
	EventExternalLost     = "extern_lost"
	// EventLeaseLost marks an advance whose write the fence refused: the row was
	// re-granted mid-flight and the outcome dropped. It explains a work_started with no
	// completion, and a stream of them is what replaced the fatal overwhelm exit.
	EventLeaseLost = "lease_lost"
)

// ActorEngine is the Actor recorded for work genroc does on its own behalf. NOT empty: empty
// means "written before attribution existed" (migration 038), and an engine advance is a known
// actor. It is stored on every such row but not rendered -- see logview.Record.Detail.
const ActorEngine = "engine:self"

// LogEntry is one persisted line of an instance's execution audit trail.
//
// Data carries the single raw payload an event is about — a process/task input,
// output, or request/response/error body — as valid JSON: a value too large to sit
// inline is replaced by a reference listed in Objects, never truncated. Meta carries
// small, complete, structured metadata about the event (e.g. {"url":…} /
// {"status":200}). Message is the human-readable summary; the same fact may appear
// in both Message (prose) and Meta (structured) by design. Small facts with no payload
// (attempt counts, goto target, child counts) live in Message.
type LogEntry struct {
	ID         string   `json:"id"`
	InstanceID string   `json:"instance_id"`
	Level      LogLevel `json:"level"`
	Event      string   `json:"event"`
	TaskID     string   `json:"task_id,omitempty"`
	Message    string   `json:"message,omitempty"`
	Code       string   `json:"code,omitempty"`
	Data       string   `json:"data,omitempty"`
	// Objects lists this entry's externalized pieces, with paths rooted at Data. Beside the
	// payload rather than inside it, the same as every other owner. specs/object-store.md.
	Objects []*ObjectRef   `json:"objects,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
	// Actor is who caused this entry, as `source:subject`: an operator for a verb they asked
	// for, `engine:self` for the engine's own advance. Empty only on a row written before
	// migration 038. specs/api-auth.md section 7.
	Actor string `json:"actor,omitempty"`
	// Seq is the minting counter, stored beside the id (migration 042) because an id does not
	// sort: it is what orders two rows that share a millisecond. Set by the write path.
	Seq       int64     `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}
