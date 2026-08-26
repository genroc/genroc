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
	Process       string `json:"process"`        // optional filter: exact process name (all versions)
	Version       int    `json:"version"`        // optional filter: exact process version (0 = any)
	Root          bool   `json:"root"`           // optional filter: only instances with no parent
	CreatedAfter  int64  `json:"created_after"`  // only instances created at/after this timestamp
	CreatedBefore int64  `json:"created_before"` // only instances created strictly before it
	UpdatedAfter  int64  `json:"updated_after"`  // only instances updated at/after this timestamp
	UpdatedBefore int64  `json:"updated_before"` // only instances updated strictly before it
	Pagination
}

type UpgradeInstanceReq struct {
	FromVersion int `json:"from_version"` // asserted, not read: 0 skips the assertion
	ToVersion   int `json:"to_version"`   // the version the ROOT moves to; children are derived
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
	Raises       model.Raises   `json:"raises,omitempty"`        // the codes this task accepts on the error channel -> the payload each carries (null = none)
	WaitingSince string         `json:"waiting_since"`           // RFC3339 park time
	Objects      []ObjectEntry  `json:"objects,omitempty"`       // this entry's externalized values, rooted at the entry (e.g. ["input"])
	Deadline     string         `json:"deadline,omitempty"`      // RFC3339 task timeout; absent = waits forever. Past it the engine raises external.timeout whatever a claim holds
	ClaimedBy    string         `json:"claimed_by,omitempty"`    // worker holding a live claim; absent = claimable
	ClaimExpires string         `json:"claim_expires,omitempty"` // RFC3339 visibility timeout of that claim
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

type ClaimExternalTasksReq struct {
	WorkerID string `json:"worker_id"`          // who is claiming; recorded as the holder and required to renew
	Limit    int    `json:"limit,omitempty"`    // max tasks to claim (default 1, cap 100)
	LeaseMs  int64  `json:"lease_ms,omitempty"` // visibility timeout in ms (default 30000)
	Process  string `json:"process,omitempty"`  // filter: process name
	Version  int    `json:"version,omitempty"`  // filter: process version (0 = any)
	Task     string `json:"task,omitempty"`     // filter: task id
}

type RenewExternalClaimsReq struct {
	WorkerID string   `json:"worker_id"`          // the holder; a claim it no longer holds is not renewed
	Tokens   []string `json:"tokens"`             // the claim tokens to extend
	LeaseMs  int64    `json:"lease_ms,omitempty"` // new visibility timeout in ms (default 30000)
}

type ReleaseExternalTaskReq struct {
	Token string `json:"token"` // the claim token to hand back
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
	// The error the instance REPORTS, flat and under the column names it is stored by, so the
	// field a caller filters on (?error_code=) is the field it reads back. Its payload is
	// error_data, on the single-instance shape only -- a list row stays light. The error the
	// instance CAUGHT is neither of these; it lives in `context` under `error`.
	ErrorCode    string `json:"error_code,omitempty"` // machine-readable discriminator for every non-success outcome; see model.ProcessInstance.ErrorCode
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// InstanceStatusResp is the single-instance shape: the summary's fields plus the context and
// the reported error's payload. It embeds nothing, so every field it puts on the wire is
// declared here — the list and this one must agree on names and TYPES, and an embedded struct
// whose fields one of them overrides is how they stopped agreeing before.
type InstanceStatusResp struct {
	ID         string          `json:"id"`
	Process    string          `json:"process"`
	Version    int             `json:"version"`
	Status     model.Status    `json:"status"`
	WaitState  model.WaitState `json:"wait_state,omitempty"`
	Task       string          `json:"task,omitempty"`
	RetryCount int             `json:"retry_count"`
	ErrorCode  string          `json:"error_code,omitempty"`
	// ErrorMessage and ErrorData are the rest of the same error. Data is what the clause
	// attached, absent where it attached nothing — a parent reads it only where its call
	// declares the code under `raises`; here it is for an operator.
	ErrorMessage string `json:"error_message,omitempty"`
	ErrorData    any    `json:"error_data,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	// Objects is error_data's, and only error_data's: a payload past the inline cutoff is ABSENT
	// above and listed here at the path it belongs to, so nothing in the data can be mistaken for
	// a reference. Fetch one with GET /objects/{ref} and put it back. It is not resolved for you
	// -- a payload has no size limit, and inlining it here would put an unbounded response behind
	// no control at all. specs/object-store.md §The wire.
	Objects []ObjectEntry `json:"objects,omitempty"`
}

// InstanceDetailResp is the whole row: the instance's STATE exactly as stored -- bookkeeping
// slots included, nothing hidden or renamed -- plus the columns around it. It is the debugging
// and upgrade view; `context` on the status response is the authoring one, and the difference
// is deliberate. State is internal, and a caller reading it is reading engine internals.
//
// Config is absent on purpose. It is resolved per tick from the environment, never persisted,
// and never returned over the API, because it is where secrets live (model.ProcessInstance).
type InstanceDetailResp struct {
	ID          string   `json:"id"`
	Process     string   `json:"process"`
	Version     int      `json:"version"`
	ParentID    string   `json:"parent_id,omitempty"`
	SpawnTaskID string   `json:"spawn_task_id,omitempty"`
	CallStack   []string `json:"call_stack,omitempty"`

	Status     model.Status    `json:"status"`
	WaitState  model.WaitState `json:"wait_state,omitempty"`
	Task       string          `json:"task,omitempty"`
	RetryCount int             `json:"retry_count"`
	WakeAt     string          `json:"wake_at,omitempty"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// ErrorData is the same value as State["_error_data"], surfaced flat so that this response is
	// a strict SUPERSET of the status one: a caller can move to this endpoint without losing a
	// field. Its externalized pieces are listed once, under the state path they were cut from.
	ErrorData any `json:"error_data,omitempty"`

	// Children is the parent's spawns, keyed by the task that made them: a bare id for a single
	// `child`, an object keyed by entry for a `child_map`, an array in spawn order for a
	// `child_list`. DERIVED from the child rows on every read rather than stored on the parent
	// -- the relation is already in the rows, and a copy on the parent is a second source to
	// keep in step. A `child_list` that spawned nothing therefore names no task here.
	Children map[string]any `json:"children,omitempty"`

	// State is the stored state verbatim: input, outputs, output, error, and the
	// engine's own slots (_error_data, _external, _spawn_*). The set is CLOSED -- a key
	// outside it does not survive a write -- so this is the whole of what the instance holds.
	State map[string]any `json:"state"`

	// The lease is the engine's grant to advance this instance; the external claim is a worker
	// holding a parked task. They are different things and deliberately separate columns.
	WorkerID        string `json:"worker_id,omitempty"`
	LeaseExpiresAt  string `json:"lease_expires_at,omitempty"`
	LeaseEpoch      int64  `json:"lease_epoch"`
	TaskEpoch       int64  `json:"task_epoch"`
	ParentTaskEpoch int64  `json:"parent_task_epoch"`
	NextReplayable  bool   `json:"next_replayable"`

	ExternalWorkerID       string `json:"external_worker_id,omitempty"`
	ExternalLeaseExpiresAt string `json:"external_lease_expires_at,omitempty"`
	ExternalClaimEpoch     int64  `json:"external_claim_epoch"`

	// Objects lists the values too large to be carried inline, each with the path in this
	// response where it belongs. Fetch one with GET /objects/{ref} and put it there; the slot is
	// ABSENT from state rather than holding a marker, so nothing in the data can be mistaken for
	// a reference. Omitted when there are none, like every other section — a recipient checks
	// for the field, so one shape everywhere beats absent-versus-empty. specs/object-store.md.
	Objects []ObjectEntry `json:"objects,omitempty"`
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
	Data     any            `json:"data,omitempty"` // payload (input/output/request/response body) as a value; parts the cut moved out are absent here and listed in Objects
	Meta     map[string]any `json:"meta,omitempty"` // small, complete, parseable metadata (e.g. {"url":…}, {"status":200})
	// Objects lists this ENTRY's externalized values, with paths rooted at the entry —
	// ["data"], not ["items", 3, "data"]. A section belongs to whatever object owns the values
	// it names, which is what keeps it correct in a list: a path containing a position is valid
	// only for one unmodified page, and a client that accumulates pages or reverses rows (as
	// genctl does) has already invalidated it. Rooted at the entry, the section travels with
	// its owner. specs/object-store.md §The wire.
	Objects []ObjectEntry `json:"objects,omitempty"`
}
