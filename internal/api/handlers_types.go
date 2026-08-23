package api

import (
	"encoding/json"
	"fmt"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// --- Request / Response types ---

// Pagination is the common sort/cursor query surface embedded in every list
// request. Order is "asc"|"desc"|"" (empty = the endpoint's default direction).
// after/before are opaque cursors from a previous page's page object; before pages
// backward. Empty after+before = the first page.
type Pagination struct {
	Sort   string `json:"sort,omitempty"`
	Order  string `json:"order,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	After  string `json:"after,omitempty"`
	Before string `json:"before,omitempty"`
}

// page maps the request surface to a db.PageReq. Order "" leaves Desc nil so the
// listing's default direction applies.
func (p Pagination) page() db.PageReq {
	req := db.PageReq{Sort: p.Sort, Limit: p.Limit, After: p.After, Before: p.Before}
	switch p.Order {
	case "asc":
		desc := false
		req.Desc = &desc
	case "desc":
		desc := true
		req.Desc = &desc
	}
	return req
}

// PageResp is the envelope every list endpoint returns: a page of items plus the
// page object (size, the item counts before and after this page, the effective
// sort/order, and the cursors to page either way).
type PageResp[T any] struct {
	Items []T         `json:"items"`
	Page  db.PageInfo `json:"page"`
}

type PutDefinitionReq struct {
	model.ProcessDefinition
}

type StartInstanceReq struct {
	Process string  `json:"process"`
	Version *int    `json:"version,omitempty"` // explicit version; takes priority over Channel
	Channel *string `json:"channel,omitempty"` // resolve to version via channel; fallback to latest
	Input   *any    `json:"input,omitempty"`
}

type PutDefinitionsBatchReq struct {
	Definitions []model.ProcessDefinition `json:"definitions"`
	Channel     string                    `json:"channel"` // default "latest"
}

type ChannelEntry struct {
	Channel string `json:"channel"`
	Version int    `json:"version"`
}

type PutChannelReq struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Version int    `json:"version"`
}

type DeleteChannelReq struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
}

type ListChannelsReq struct {
	Name string `json:"name"`
	Pagination
}

type PromoteChannelReq struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Process *string `json:"process,omitempty"` // nil = all processes on the channel
}

type ChannelStatusReq struct {
	Channel string `json:"channel"`
}

// VersionRef is one entry's version: a number, or the name of a channel to resolve it
// through. It decodes from either JSON form (3 or "latest") so a selector can pin some
// processes and follow a channel for others without a second field.
type VersionRef struct {
	Version int
	Channel string
}

func (v VersionRef) MarshalJSON() ([]byte, error) {
	if v.Channel != "" {
		return json.Marshal(v.Channel)
	}
	return json.Marshal(v.Version)
}

func (v *VersionRef) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &v.Version); err == nil {
		return nil
	}
	if err := json.Unmarshal(data, &v.Channel); err != nil {
		return fmt.Errorf("version must be a number or a channel name: %w", err)
	}
	return nil
}

// CompatSelector resolves to one version per process name. A triple of
// {process, from, to} cannot name a graph, so each side of a comparison is a selector and
// the two are paired by process name. Exactly one field may be set.
//
// Whatever a selector names is closed over the child versions those definitions were
// registered against, so a parent is never judged without the children it calls. An
// entry named here wins over one a dependency pins.
type CompatSelector struct {
	Channel     string                    `json:"channel,omitempty"      description:"Every process on this channel, at the version the channel points at."`
	Versions    map[string]VersionRef     `json:"versions,omitempty"     description:"Process name → version number, or a channel name to resolve it through."`
	Definitions []model.ProcessDefinition `json:"definitions,omitempty"  description:"Documents that are not stored yet — the ones an apply would take. They have no version, so they report version null."`
}

type CompatReq struct {
	From CompatSelector `json:"from"`
	To   CompatSelector `json:"to"`
	// Process scopes the comparison to one process and the subtree of children it
	// reaches, so a large channel can be asked a small question.
	Process string `json:"process,omitempty"`
	// Ignore excuses a check from the exit code. It changes neither what is compared nor
	// what is reported — an excused break still appears, marked — and the upgrade check
	// cannot be excused at all. specs/compat-command.md §5.
	Ignore []string `json:"ignore,omitempty" description:"Members excused from the exit code. Only \"contract\" is accepted: the upgrade check answers for rows this deployment already owns"`
}

// CompatResp is the whole verdict: one row per process named on either side, whatever
// became of it. Compatible is the conjunction over the rows that were actually compared —
// a process with nothing to compare, or one that is new, cannot break anything — plus any
// version that failed its own inference.
type CompatResp struct {
	Compatible bool `json:"compatible"`
	// Passes is the gated answer — the exit code as a boolean. It equals Compatible when
	// nothing was ignored, and the two are MEANT to disagree when something was: the
	// roll-up is what was found, Passes is what this caller asked about.
	Passes    bool                `json:"passes" description:"False only where a gating check broke. Equals compatible when ignore is empty"`
	Processes []validation.Report `json:"processes"`
}

type StaleRef struct {
	TaskID         string `json:"task_id"`
	ChildName      string `json:"child_name"`
	BakedVersion   int    `json:"baked_version"`
	ChannelVersion int    `json:"channel_version"`
}

type ChannelStatusItem struct {
	Name      string     `json:"name"`
	Version   int        `json:"version"`
	StaleRefs []StaleRef `json:"stale_refs,omitempty"`
}

// HealthResp is the readiness probe's body. Status is the only field a probe should key
// on; the rest is operator context for a worker that is up but behaving oddly.
type HealthResp struct {
	Status     string `json:"status" description:"ok — this worker reached its database; any other outcome is a 503"`
	Worker     string `json:"worker" description:"Worker id stamped on the leases this worker holds"`
	Database   string `json:"database" description:"Storage engine backing this worker: sqlite or postgres"`
	LeaseAgeMs int64  `json:"lease_age_ms" description:"Milliseconds since this worker last renewed its leases. Past --lease-duration means its claimed instances are being taken over by peers."`
	ManualTick bool   `json:"manual_tick" description:"True when started with -poll 0: the engine only advances via POST /tick"`
}

type StartInstanceResp struct {
	ID      string       `json:"id"`
	Process string       `json:"process"`
	Version int          `json:"version"`
	Status  model.Status `json:"status"`
}

// Every list takes its time bounds as {col}_after / {col}_before, in unix millis, forming
// the half-open range [after, before) — see db.Window for why the far end is exclusive.
// Zero means unbounded on that side.

type ListDefinitionsReq struct {
	CreatedAfter  int64 `json:"created_after"`  // only versions registered at/after this timestamp
	CreatedBefore int64 `json:"created_before"` // only versions registered strictly before it
	Pagination
}

type ListInstancesReq struct {
	Status        string `json:"status"`         // optional filter: running, completed, failing, failed, raised, pausing, paused
	ErrorCode     string `json:"error_code"`     // optional filter: exact error code (authored or engine)
	CreatedAfter  int64  `json:"created_after"`  // only instances created at/after this timestamp
	CreatedBefore int64  `json:"created_before"` // only instances created strictly before it
	UpdatedAfter  int64  `json:"updated_after"`  // only instances updated at/after this timestamp
	UpdatedBefore int64  `json:"updated_before"` // only instances updated strictly before it
	Pagination
}

type RetryInstanceReq struct {
	Force bool `json:"force"` // override only_once retry protection
}

type ListExternalTasksReq struct {
	Process string `json:"process"` // optional: filter by process name
	Version int    `json:"version"` // optional: filter by process version (0 = any)
	Task    string `json:"task"`    // optional: filter by task id
	// updated_at is the park time and this list's sort, so there is no created_* pair.
	UpdatedAfter  int64 `json:"updated_after"`  // only tasks parked at/after this timestamp
	UpdatedBefore int64 `json:"updated_before"` // only tasks parked strictly before it
	Pagination
}

// ExternalTaskResp is one entry in the external-task queue. It exposes only the task's
// snapshotted input + the result_schema the resolver must satisfy, plus the resolve
// token — never the process context.
type ExternalTaskResp struct {
	Token        string         `json:"token"` // pass back to /external-tasks/resolve
	Process      string         `json:"process"`
	Version      int            `json:"version"`
	TaskID       string         `json:"task_id"`
	Input        any            `json:"input"`                   // the task's evaluated input snapshot
	ResultSchema *schema.Schema `json:"result_schema,omitempty"` // JSON Schema the submitted result must satisfy
	Raises       model.Raises   `json:"raises,omitempty"`        // code -> JSON Schema the failure payload must satisfy, for /external-tasks/fail
	WaitingSince string         `json:"waiting_since"`           // RFC3339 park time
}

// FailureReq is the error half of a submitted outcome. A pointer field on the two request
// types, so its PRESENCE is the discriminator: `result: null` stays an ordinary success, and
// nothing has to be spelled to say which channel a submission is on.
type FailureReq struct {
	Code    string `json:"code"`           // lower_snake_case, no dots: the code on_error rules match
	Message string `json:"message"`        // human-readable cause; lands on error.message
	Data    any    `json:"data,omitempty"` // payload, validated against the task's raises[code]
}

type ResolveExternalTaskReq struct {
	Token  string      `json:"token"`            // the token from the external-task queue
	Result any         `json:"result,omitempty"` // the result payload, validated against the task's result_schema
	Error  *FailureReq `json:"error,omitempty"`  // set INSTEAD of result to answer on the error channel
}

type SignalInstanceReq struct {
	TaskID string      `json:"task_id"`          // the external task to deliver to (addressed, not by token)
	Result any         `json:"result,omitempty"` // the result, validated against the task's result_schema
	Error  *FailureReq `json:"error,omitempty"`  // set INSTEAD of result to answer on the error channel
}

type ListLogsReq struct {
	Level         string `json:"level"`          // optional filter: debug, info, warn, error
	CreatedAfter  int64  `json:"created_after"`  // only logs at/after this timestamp
	CreatedBefore int64  `json:"created_before"` // only logs strictly before it
	Recursive     bool   `json:"recursive"`      // include the whole process subtree, keyed on the root instance
	Resolve       bool   `json:"resolve"`        // inline full externalized payloads instead of preview + data_ref
	Pagination
}

type TickReq struct {
	AdvanceMs int64 `json:"advance_ms"` // shift the server clock forward (milliseconds) before ticking (testing only)
}

type DefinitionSummary struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"` // RFC3339 registration time; the default listing sort
	// Raises is the set of error codes this definition can raise, derived by scanning
	// its raise clauses. There is no `errors:` declaration block to read, so this is the
	// answer to "what can this process raise?" and therefore to "what may a parent write
	// on_error rules against?". Panic codes are excluded: nothing can catch a panic.
	Raises []string `json:"raises,omitempty"`
}

type BatchApplyResult struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Saved   bool   `json:"saved"`
}

// InstanceSummaryResp is the per-row shape returned by the instance list. Listing
// many instances should stay light, so it omits the (potentially large) context; it
// is embedded in InstanceStatusResp, which adds the context for a single-instance fetch.
type InstanceSummaryResp struct {
	ID        string          `json:"id"`
	Process   string          `json:"process"`
	Version   int             `json:"version"`
	Status    model.Status    `json:"status"`
	WaitState model.WaitState `json:"wait_state,omitempty"`
	// Task is where the instance sits in its definition: the task it is running, is
	// parked on, or stopped at — and on a settled instance, the one it finished,
	// failed or raised at. Status says what is happening to the process and
	// wait_state says what it is waiting for; this says where.
	Task       string `json:"task,omitempty"`
	RetryCount int    `json:"retry_count"`
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"` // machine-readable discriminator for every non-success outcome; see model.ProcessInstance.ErrorCode
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type InstanceStatusResp struct {
	InstanceSummaryResp
	Context map[string]any `json:"context"`
}

type LogEntryResp struct {
	Time     string         `json:"time"`
	Instance string         `json:"instance"`
	Depth    int            `json:"depth"` // distance from the queried subtree root (0 = the queried node)
	Level    model.LogLevel `json:"level"`
	Event    string         `json:"event"`
	Task     string         `json:"task,omitempty"`
	Message  string         `json:"message,omitempty"`
	Code     string         `json:"code,omitempty"`
	Data     string         `json:"data,omitempty"`     // inline payload (input/output/request/response body); empty when externalized — see DataRef — unless ?resolve=true inlines the full value
	DataRef  *LogDataRef    `json:"data_ref,omitempty"` // set when the full payload was externalized to an object; fetch via /instances/{id}/objects/{ref} or pass ?resolve=true
	Meta     map[string]any `json:"meta,omitempty"`     // small, complete, parseable metadata (e.g. {"url":…}, {"status":200})
}

// LogDataRef points at an externalized log payload; the full (pre-redacted) value is
// retrievable from the log-object endpoint.
type LogDataRef struct {
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}
