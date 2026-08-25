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

// Log event kinds emitted by the engine as it advances an instance. These are
// the stable machine-readable identifiers; the human message lives in Message.
const (
	EventInstanceCreated = "inst_created"
	EventWorkStarted     = "work_started"     // a worker picked the instance up and began advancing it
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

// LogEntry is one persisted line of an instance's execution audit trail.
//
// Data carries the single raw payload an event is about — a process/task input,
// output, or request/response/error body — as a JSON snippet that MAY be truncated
// (so it is not guaranteed to parse). Meta carries small, complete, structured
// metadata about the event (e.g. {"url":…} / {"status":200}) and is always valid
// JSON. Message is the human-readable summary; the same fact may appear in both
// Message (prose) and Meta (structured) by design. Small facts with no payload
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
	Objects   []*ObjectRef   `json:"objects,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	// Depth is the instance's distance from the queried subtree root; only set by
	// ListTreeLogs (0 for single-instance queries). Not persisted.
	Depth int `json:"-"`
}
