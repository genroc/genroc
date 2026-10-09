package api

import (
	"encoding/json"
	"fmt"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

// --- Request / Response types ---

// Pagination is embedded in every list request. After/Before are opaque cursors from a
// previous page's page object; Order "" is the endpoint's default direction.
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

// PageResp is the envelope every list endpoint returns.
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
	// UpdatedAt and Actor are when this pointer last moved and who moved it — current state,
	// not history: the previous answer is gone once it moves again. specs/api-auth.md §7.
	UpdatedAt string `json:"updated_at,omitempty"`
	Actor     string `json:"actor,omitempty"`
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

// VersionRef decodes from 3 or "latest", so a selector can pin some processes and follow
// a channel for others.
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

// CompatSelector resolves to one version per process name; the two sides pair by name. Exactly
// one field may be set. It closes over pinned child versions, and an entry named here wins over
// one a dependency pins.
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
	// Ignore excuses a check from the exit code only: an excused break is still reported.
	// specs/compat-command.md §5.
	Ignore []string `json:"ignore,omitempty" description:"Members excused from the exit code. Only \"contract\" is accepted: the upgrade check answers for rows this deployment already owns"`
}

// CompatResp has one row per process named on either side. Compatible is the conjunction over
// the rows actually compared, plus any version that failed its own inference.
type CompatResp struct {
	Compatible bool `json:"compatible"`
	// Passes is the exit code as a boolean; it and Compatible are MEANT to disagree when
	// something was ignored.
	Passes    bool                `json:"passes" description:"False only where a gating check broke. Equals compatible when ignore is empty"`
	Processes []validation.Report `json:"processes"`
}

type StaleRef struct {
	TaskID         string `json:"task"`
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
	Worker     string `json:"worker_id" description:"Worker id stamped on the leases this worker holds"`
	Database   string `json:"database" description:"Storage engine backing this worker: sqlite or postgres"`
	LeaseAgeMs int64  `json:"lease_age_ms" description:"Milliseconds since this worker last renewed its leases. Past --lease-duration means its claimed instances are being taken over by peers."`
	ManualTick bool   `json:"manual_tick" description:"True when started with --poll 0: the engine only advances via POST /tick"`
}

type StartInstanceResp struct {
	ID      string       `json:"id"`
	Process string       `json:"process"`
	Version int          `json:"version"`
	Status  model.Status `json:"status"`
}

// Time bounds are {col}_after / {col}_before in unix millis, half-open [after, before)
// (db.Window); zero is unbounded.

type ListDefinitionsReq struct {
	CreatedAfter  int64 `json:"created_after"`  // only versions registered at/after this timestamp
	CreatedBefore int64 `json:"created_before"` // only versions registered strictly before it
	Pagination
}

type ListInstancesReq struct {
	Status        string `json:"status"`         // optional filter: one status, or several comma-separated
	Phase         string `json:"phase"`          // optional filter: children, collecting, external — why it is not executing a task
	Task          string `json:"task"`           // optional filter: exact task id the instance sits on
	ErrorCode     string `json:"error_code"`     // optional filter: exact error code (authored or engine)
	Process       string `json:"process"`        // optional filter: exact process name (all versions)
	Version       int    `json:"version"`        // optional filter: exact process version (0 = any)
	Children      bool   `json:"children"`       // include child instances; roots only when false (the default)
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

// ExternalTaskResp exposes the input snapshot and token, never the process context. The answer's
// contract (result_schema, raises) is the definition's: fixed per version, enforced at resolve.
type ExternalTaskResp struct {
	Token        string        `json:"token"` // pass back to /external-tasks/resolve
	Process      string        `json:"process"`
	Version      int           `json:"version"`
	TaskID       string        `json:"task"`
	Input        any           `json:"external_input"`           // the task's evaluated input snapshot, under the one name every view spells it
	WaitingSince string        `json:"waiting_since"`            // RFC3339 park time
	Objects      []ObjectEntry `json:"objects,omitempty"`        // this entry's externalized values, rooted at the entry (e.g. ["external_input"])
	Deadline     string        `json:"deadline,omitempty"`       // RFC3339 task timeout; absent = waits forever. Past it the engine raises external.timeout whatever a claim holds
	DeadlineInMs *int64        `json:"deadline_in_ms,omitempty"` // the same deadline as ms from now, 0 once past: what a worker budgets by, free of clock skew and second rounding
	ClaimedBy    string        `json:"claimed_by,omitempty"`     // worker holding a live claim; absent = claimable
	ClaimExpires string        `json:"claim_expires,omitempty"`  // RFC3339 visibility timeout of that claim
}

// FailureReq is a pointer field, so its PRESENCE discriminates: `result: null` stays an
// ordinary success.
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
	// InstanceID addresses the target BY NAME, where resolve addresses it by token. Both
	// deliver the same ExternalOutcome; the split is who holds what, not what arrives.
	InstanceID string      `json:"instance_id"`
	TaskID     string      `json:"task"`             // the external task to deliver to
	Result     any         `json:"result,omitempty"` // the result, validated against the task's result_schema
	Error      *FailureReq `json:"error,omitempty"`  // set INSTEAD of result to answer on the error channel
}

type ListLogsReq struct {
	Level         string `json:"level"`          // optional FLOOR: this level and everything above it
	CreatedAfter  int64  `json:"created_after"`  // only logs at/after this timestamp
	CreatedBefore int64  `json:"created_before"` // only logs strictly before it
	Flat          bool   `json:"flat"`           // this instance's own rows only, where a root would answer with its tree
	Pagination
}

type TickReq struct {
	AdvanceMs int64 `json:"advance_ms"` // shift the server clock forward (milliseconds) before ticking (testing only)
}

type DefinitionSummary struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"` // RFC3339 registration time; the default listing sort
	// Raises is what a parent may write on_error rules against, scanned from raise clauses
	// (there is no `errors:` block). Panic codes are excluded: nothing can catch a panic.
	Raises []string `json:"raises,omitempty"`
	// Actor is who deployed this version; permanently absent for versions applied before
	// attribution. specs/api-auth.md section 7.
	Actor string `json:"actor,omitempty"`
}

type BatchApplyResult struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Saved   bool   `json:"saved"`
	// Previous is the REQUESTED channel's prior pointer, 0 for none: what tells a rollback
	// (nothing saved, pointer moved back) from a no-op.
	Previous int `json:"previous"`
}

// InstanceSummaryResp is the instance list row; it omits the context so a listing stays light.
type InstanceSummaryResp struct {
	ID string `json:"id"`
	// ParentID is "" for a root. Present on every row, not only when children were asked
	// for: a caller that filtered them in has nothing else to tell the two apart.
	ParentID string       `json:"parent_id,omitempty"`
	Process  string       `json:"process"`
	Version  int          `json:"version"`
	Status   model.Status `json:"status"`
	Phase    model.Phase  `json:"phase,omitempty"`
	// Task is where the instance runs, parks, or settled. Status says what is happening and
	// phase why it is not executing; this says where.
	Task       string `json:"task,omitempty"`
	RetryCount int    `json:"retry_count"`
	// The error the instance REPORTS, under its column names so ?error_code= filters the field it
	// reads back. The error it CAUGHT is `error` in `context`.
	ErrorCode    string `json:"error_code,omitempty"` // machine-readable discriminator for every non-success outcome; see model.ProcessInstance.ErrorCode
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// InstanceStatusResp is the single-instance shape. It embeds nothing: it and the list row must
// agree on names and TYPES, and an embedded struct with overridden fields is how they diverged.
type InstanceStatusResp struct {
	ID         string       `json:"id"`
	Process    string       `json:"process"`
	Version    int          `json:"version"`
	Status     model.Status `json:"status"`
	Phase      model.Phase  `json:"phase,omitempty"`
	Task       string       `json:"task,omitempty"`
	RetryCount int          `json:"retry_count"`
	ErrorCode  string       `json:"error_code,omitempty"`
	// ErrorData is absent where the clause attached nothing. A parent reads it only under
	// declared `raises`; here it is for an operator.
	ErrorMessage string `json:"error_message,omitempty"`
	ErrorData    any    `json:"error_data,omitempty"`
	// Output is the declared `output:` -- what the process reports outward, and what a parent
	// collects as its result -- so this answers "what did it produce" without exposing state.
	Output any `json:"output,omitempty"`
	// ExternalInput is the parked external task's `input:` snapshot, present only while parked.
	// Reading it takes no claim. specs/external-task-queue.md.
	ExternalInput any    `json:"external_input,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	// Objects covers error_data, output and external_input: a payload past the inline cutoff is
	// ABSENT above and listed here, never resolved for you -- a payload has no size limit.
	// specs/object-store.md §The wire.
	Objects []ObjectEntry `json:"objects,omitempty"`
}

// InstanceDetailResp is the whole row, bookkeeping slots included: the debugging and upgrade
// view. Config is absent on purpose -- never persisted, and where secrets live.
type InstanceDetailResp struct {
	ID          string   `json:"id"`
	Process     string   `json:"process"`
	Version     int      `json:"version"`
	ParentID    string   `json:"parent_id,omitempty"`
	SpawnTaskID string   `json:"spawn_task_id,omitempty"`
	CallStack   []string `json:"call_stack,omitempty"`

	Status     model.Status `json:"status"`
	Phase      model.Phase  `json:"phase,omitempty"`
	Task       string       `json:"task,omitempty"`
	RetryCount int          `json:"retry_count"`
	WakeAt     string       `json:"wake_at,omitempty"`
	CreatedAt  string       `json:"created_at"`
	UpdatedAt  string       `json:"updated_at"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	// Fields of their own so this is a strict SUPERSET of the status response; MOVED out of
	// State, not copied, so each value and its objects path appear once.
	ErrorData     any `json:"error_data,omitempty"`
	Output        any `json:"output,omitempty"`
	ExternalInput any `json:"external_input,omitempty"`

	// Children is keyed by spawning task: an id (child), an object by entry (child_map), an array
	// in spawn order (child_list). DERIVED from the child rows on read, never stored on the
	// parent, so a child_list that spawned nothing names no task here.
	Children map[string]any `json:"children,omitempty"`

	// State is the stored state minus the three fields above. The stored key set is CLOSED, so
	// State plus those fields is all the instance holds.
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

	// Objects lists values too large to inline, at their path in this response; the slot is
	// ABSENT, not a marker. Omitted when empty, like every section. specs/object-store.md.
	Objects []ObjectEntry `json:"objects,omitempty"`
}

type LogEntryResp struct {
	// created_at and instance_id, spelled as every other resource spells them: a trail row is
	// still a row with a timestamp and a foreign reference on it.
	Time     string         `json:"created_at"`
	Instance string         `json:"instance_id"`
	Level    model.LogLevel `json:"level"`
	Event    string         `json:"event"`
	Task     string         `json:"task,omitempty"`
	Message  string         `json:"message,omitempty"`
	Code     string         `json:"code,omitempty"`
	// Actor is who caused this entry, as `source:subject` -- an operator, or `engine:self` for
	// the engine's own advance. specs/api-auth.md section 7.
	Actor string         `json:"actor,omitempty"`
	Data  any            `json:"data,omitempty"` // payload (input/output/request/response body) as a value; parts the cut moved out are absent here and listed in Objects
	Meta  map[string]any `json:"meta,omitempty"` // small, complete, parseable metadata (e.g. {"url":…}, {"status":200})
	// Objects paths are rooted at the ENTRY (["data"]), not the page, so the section travels
	// with its owner. specs/object-store.md §The wire.
	Objects []ObjectEntry `json:"objects,omitempty"`
}

// ── tokens ───────────────────────────────────────────────────────────────────────

type CreateTokenReq struct {
	Label string   `json:"label"` // optional: shown in listings and recorded as the actor
	Perms []string `json:"perms"` // required: admin, deploy, operate, read, worker
}

// CreateTokenResp is the ONE response that carries a secret. Every later read of the row
// cannot produce it, because only its hash was stored.
type CreateTokenResp struct {
	ID    string   `json:"id"`
	Token string   `json:"token"` // shown once; there is no way to retrieve it again
	Label string   `json:"label,omitempty"`
	Perms []string `json:"perms"`
}

// TokenResp is a token as a listing shows it — deliberately without the secret.
type TokenResp struct {
	ID         string   `json:"id"`
	Label      string   `json:"label,omitempty"`
	Perms      []string `json:"perms"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
	// ExpiresAt is absent for a token that never expires, which every machine credential is —
	// nothing sets it since the session exchange went (auth-two-credentials.md s6).
	ExpiresAt string `json:"expires_at,omitempty"`
	// Actor minted it; RevokedBy killed it. Empty means unattributed, which only a row
	// predating migration 043 is. specs/api-auth.md §7.
	Actor     string `json:"actor,omitempty"`
	RevokedBy string `json:"revoked_by,omitempty"`
}
