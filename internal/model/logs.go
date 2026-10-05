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

// LogLevelsAtLeast returns min and every level above it, or nil for an unknown level. The stored
// value is the WORD, which sorts wrong, so a caller must turn the floor into a set.
func LogLevelsAtLeast(min LogLevel) []LogLevel {
	order := []LogLevel{LogDebug, LogInfo, LogWarn, LogError}
	for i, l := range order {
		if l == min {
			return order[i:]
		}
	}
	return nil
}

// Log event kinds; the human text lives in Message. Pick the LEVEL by audience, not verbosity:
// info is the run's own story, debug is how the engine did it. genctl logs floors at info.
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
	EventFaultMessage    = "fault_message" // a raise/panic message that did not render to a string
	// EventInstanceUpgraded must tell the whole story: nothing else records which version
	// the instance came from. specs/version-compatibility.md s4.
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
	// EventLeaseLost marks an advance whose write the fence refused; it explains a
	// work_started with no completion.
	EventLeaseLost = "lease_lost"
)

// ActorEngine is the Actor recorded for work genroc does on its own behalf. NOT empty: empty
// means "written before attribution existed" (migration 038), and an engine advance is a known
// actor. It is stored on every such row but not rendered -- see logview.Record.Detail.
const ActorEngine = "engine:self"

// LogEntry is one line of an instance's audit trail. Data is the event's single payload: an
// oversized value becomes a ref listed in Objects, never truncated. Meta and Message may repeat
// a fact by design; a small fact with no payload lives in Message alone.
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
