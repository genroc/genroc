package api

import (
	"encoding/json"
	"genroc/internal/numeric"
	"net/http"
	"strconv"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// LifecycleResp is what pause/resume/retry return on a 200 or 202. There is no body on
// the 204 (unchanged) — HTTP forbids one — so a client that needs to know WHICH
// already-state it hit reads the instance. specs/id-list-commands.md.
type LifecycleResp struct {
	Outcome   model.Outcome `json:"outcome" description:"What this call did: applied (the assertion holds and this call made it hold), or accepted (recorded, not yet in effect — a pause left rows draining a task already in flight). A 204 with no body means unchanged: the assertion already held."`
	Status    model.Status  `json:"status"    description:"The root instance's status once the write committed"`
	Instances int           `json:"instances" description:"How many instances in the tree this call wrote"`
}

// altResp is one extra success status for an action; see actionDef.AltSuccess.
type altResp struct {
	Status int
	Body   any
}

// actionDef is the single source of truth for one API action.
// It drives HTTP routing (Method + Path) and OpenAPI documentation
// (schemas reflected from Go types).
// apiPrefix namespaces every action except the probes. It exists so a deployment can route
// humans and machines apart by path on ONE hostname — the API under this prefix reached
// directly, everything else through an SSO proxy — which is what keeps a browser hitting the
// bare domain from receiving a 401 body instead of a login page. specs/api-auth.md §1, §5.1.
//
// It is NOT repeated in Path: the registry holds the logical path and the spec declares the
// prefix once in `servers`, which is where OpenAPI puts a base path. Duplicating it into 28
// literals would put it in two places that must agree.
const apiPrefix = "/api"

// mountPath is where this action is actually served.
func (a actionDef) mountPath() string {
	if a.Root {
		return a.Path
	}
	return apiPrefix + a.Path
}

type actionDef struct {
	Name    string
	Method  string
	Path    string
	Summary string
	Tags    []string

	// Req is a zero-value of the request body type (nil = no body).
	Req any

	// PathQuery is a struct with path/query tagged fields for OpenAPI parameter generation.
	PathQuery any

	// Resp is a zero-value of the response data type, documented at 200.
	Resp any

	// AltSuccess documents the success statuses an action can return besides 200. Only
	// the lifecycle assertions need it: their status IS the answer (statusOfOutcome), so
	// a spec listing 200 alone would hide two of the three outcomes. A nil Body documents
	// a response with no content, which is what 204 requires.
	AltSuccess []altResp

	// Errors are the failure codes this action can produce, documented in the spec as
	// the corresponding statuses. CodeInvalid and CodeInternal are implicit — every
	// action can reject a body and every action can fail — so list only the extras.
	Errors []Code

	// Allow lists the permissions that admit this action; ANY one of them suffices, and
	// PermAdmin always does. EMPTY means admin-only — the fail-closed default, so an endpoint
	// added without thinking is closed rather than open. specs/api-auth.md §3.
	Allow []Perm

	// Open exempts this action from authorization entirely. `/healthz` is its only user and
	// the bar for a second is high: a probe has to answer before any identity exists, and a
	// supervisor cannot hold a credential. It reveals only whether the database is reachable.
	Open bool

	// Root mounts this action at the server root instead of under apiPrefix. Probes only:
	// a liveness check must not depend on how the API namespace is routed, and a proxy
	// splitting humans from machines by path must be able to leave it alone. See
	// specs/api-auth.md §1.
	Root bool

	// fromHTTP extracts an Envelope from an HTTP request.
	// nil = default: decode body as JSON payload.
	fromHTTP func(r *http.Request) (Envelope, error)

	// handle is the actual handler, shared by HTTP, TCP, and UDS.
	handle func(h *Handlers, env Envelope) Reply
}

// pageQuery is the common sort/cursor query-parameter surface embedded in every
// list action's PathQuery, so the OpenAPI spec documents them uniformly.
type pageQuery struct {
	Sort   string `query:"sort" description:"Sort key (per-endpoint whitelist; omit for the default)"`
	Order  string `query:"order" enum:"asc,desc" description:"Sort direction (omit for the endpoint default)"`
	Limit  int    `query:"limit" description:"Page size (default 20, cap 100)"`
	After  string `query:"after" description:"Cursor from a previous page's page.next_cursor — fetch the next page"`
	Before string `query:"before" description:"Cursor from a previous page's page.previous_cursor — fetch the previous page"`
}

// millisQuery reads a unix-millis time bound. An absent or unparseable value is 0, which
// every list reads as "unbounded on that side" — the same as omitting the parameter.
// intQuery reads an optional integer query parameter; absent or unparseable is 0, which
// every caller treats as "no filter".
func intQuery(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return n
}

func millisQuery(r *http.Request, key string) int64 {
	ms, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return ms
}

func paginationFrom(r *http.Request) Pagination {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return Pagination{
		Sort:   q.Get("sort"),
		Order:  q.Get("order"),
		Limit:  limit,
		After:  q.Get("after"),
		Before: q.Get("before"),
	}
}

func (a actionDef) envelope(r *http.Request) (Envelope, error) {
	if a.fromHTTP != nil {
		return a.fromHTTP(r)
	}
	var payload json.RawMessage
	if r.ContentLength != 0 {
		if err := numeric.DecodeReader(r.Body, &payload); err != nil {
			return Envelope{}, err
		}
	}
	// PathValue("id") is "" when the route has no {id} segment, so this is harmless
	// for body-only endpoints and spares id-based actions a custom fromHTTP.
	return Envelope{Action: a.Name, Payload: payload, ID: r.PathValue("id")}, nil
}

func schemaPtr(s schema.Schema) *schema.Schema { return &s }

// registry is the authoritative list of all actions.
// Order here determines order in Swagger.
var registry = func() []actionDef {
	v1 := 1
	return []actionDef{
		{
			Name:    "put_definition",
			Allow:   []Perm{PermDeploy},
			Method:  http.MethodPut,
			Path:    "/definitions",
			Summary: "Register or update a process definition",
			Tags:    []string{"Definitions"},
			Req: model.ProcessDefinition{
				Name: "order_pipeline",
				InputSchema: schemaPtr(schema.Object().
					WithProperty("order_id", schema.Type("integer"), true)),
				Tasks: []*model.Task{
					{
						ID: "charge",
						Action: &model.Action{
							Type: model.ActionTypeFetch,
							URL:  "http://localhost:9001/charge",
							ResultSchema: schemaPtr(schema.Object().
								WithProperty("charged", schema.Type("boolean"), false)),
						},
						Timeout: model.TimeoutFor("5s"), OnError: []model.ErrorCase{{Retry: model.RetryAttempts(3)}},
						Switch: model.SwitchMap{
							{Case: "self.output.charged == true", Goto: "$ship"},
							{Goto: "$refund"},
						},
					},
					{
						ID:      "ship",
						Action:  &model.Action{Type: model.ActionTypeFetch, URL: "http://localhost:9002/ship"},
						Switch:  model.SwitchMap{{Goto: model.GotoEnd}},
						Timeout: model.TimeoutFor("3s"), OnError: []model.ErrorCase{{Retry: model.RetryAttempts(2)}},
					},
					{
						ID:      "refund",
						Action:  &model.Action{Type: model.ActionTypeFetch, URL: "http://localhost:9003/refund"},
						Switch:  model.SwitchMap{{Goto: model.GotoEnd}},
						Timeout: model.TimeoutFor("3s"), OnError: []model.ErrorCase{{Retry: model.RetryAttempts(1)}},
					},
				},
			},
			Resp: map[string]any{"name": "order_pipeline", "version": 1, "saved": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putDefinition(env.Payload)
			},
		},
		{
			Name:    "list_definitions",
			Allow:   []Perm{PermRead},
			Method:  http.MethodGet,
			Path:    "/definitions",
			Summary: "List all registered process definitions (newest registered first)",
			Tags:    []string{"Definitions"},
			PathQuery: struct {
				CreatedAfter  int64 `query:"created_after" description:"Only versions registered at/after this unix-millis timestamp"`
				CreatedBefore int64 `query:"created_before" description:"Only versions registered strictly before this unix-millis timestamp"`
				pageQuery
			}{},
			Resp: PageResp[DefinitionSummary]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListDefinitionsReq{
					CreatedAfter:  millisQuery(r, "created_after"),
					CreatedBefore: millisQuery(r, "created_before"),
					Pagination:    paginationFrom(r),
				})
				return Envelope{Action: "list_definitions", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listDefinitions(env.Payload)
			},
		},
		{
			Name:    "start_instance",
			Allow:   []Perm{PermOperate},
			Method:  http.MethodPost,
			Path:    "/instances",
			Summary: "Start a new process instance (omit version to use latest)",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			Req: func() StartInstanceReq {
				input := any(map[string]any{"order_id": 42})
				return StartInstanceReq{Process: "order_pipeline", Version: &v1, Input: &input}
			}(),
			Resp: StartInstanceResp{
				ID: "550e8400-e29b-41d4-a716-446655440000", Process: "order_pipeline",
				Version: 1, Status: model.StatusRunning,
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.startInstance(env.Payload)
			},
		},
		{
			Name:    "list_instances",
			Allow:   []Perm{PermRead},
			Method:  http.MethodGet,
			Path:    "/instances",
			Summary: "List process instances - roots only unless children=true, so one row per tree",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				Status        string `query:"status" enum:"running,completed,failing,failed,raised,pausing,paused" description:"Filter by status"`
				ErrorCode     string `query:"error_code" description:"Filter by exact error code. Authored codes (from a raise or panic clause) are lower_snake_case; engine-produced codes contain a dot, e.g. http.500, pre.timeout, engine.spawn."`
				Process       string `query:"process" description:"Filter by exact process name, across every version"`
				Version       int    `query:"version" description:"Filter by exact process version (0 = any)"`
				Children      bool   `query:"children" description:"Include child instances. Omitted, the listing is ROOTS ONLY - one row per tree, which is the unit an upgrade or a pause acts on; a child_list fan-out would otherwise bury the roots it belongs to. Every row carries parent_id, so the two are still told apart when children are included"`
				CreatedAfter  int64  `query:"created_after" description:"Only instances created at/after this unix-millis timestamp"`
				CreatedBefore int64  `query:"created_before" description:"Only instances created strictly before this unix-millis timestamp"`
				UpdatedAfter  int64  `query:"updated_after" description:"Only instances updated at/after this unix-millis timestamp"`
				UpdatedBefore int64  `query:"updated_before" description:"Only instances updated strictly before this unix-millis timestamp"`
				pageQuery
			}{},
			Resp: PageResp[InstanceSummaryResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListInstancesReq{
					Status:        r.URL.Query().Get("status"),
					ErrorCode:     r.URL.Query().Get("error_code"),
					Process:       r.URL.Query().Get("process"),
					Version:       intQuery(r, "version"),
					Children:      r.URL.Query().Get("children") == "true",
					CreatedAfter:  millisQuery(r, "created_after"),
					CreatedBefore: millisQuery(r, "created_before"),
					UpdatedAfter:  millisQuery(r, "updated_after"),
					UpdatedBefore: millisQuery(r, "updated_before"),
					Pagination:    paginationFrom(r),
				})
				return Envelope{Action: "list_instances", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstances(env.Payload)
			},
		},
		{
			Name:    "put_definitions_batch",
			Allow:   []Perm{PermDeploy},
			Method:  http.MethodPut,
			Path:    "/definitions/batch",
			Summary: "Apply process definitions to a channel, atomically",
			Tags:    []string{"Definitions"},
			Req: PutDefinitionsBatchReq{
				Channel: "latest",
				Definitions: []model.ProcessDefinition{
					{
						Name:  "child_process",
						Tasks: []*model.Task{{ID: "run", Action: &model.Action{Type: model.ActionTypeFetch, URL: "http://localhost:9001/run"}}},
					},
				},
			},
			Resp: []BatchApplyResult{{Name: "child_process", Version: 1, Saved: true}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putDefinitions(env.Payload)
			},
		},
		{
			Name:    "put_channel",
			Allow:   []Perm{PermDeploy},
			Method:  http.MethodPut,
			Path:    "/channels",
			Summary: "Set a channel pointer to a specific process version",
			Tags:    []string{"Channels"},
			Errors:  []Code{CodeNotFound},
			Req:     PutChannelReq{Name: "order_pipeline", Channel: "stable", Version: 3},
			Resp:    map[string]any{"name": "order_pipeline", "channel": "stable", "version": 3},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putChannel(env.Payload)
			},
		},
		{
			Name:    "delete_channel",
			Allow:   []Perm{PermDeploy},
			Method:  http.MethodDelete,
			Path:    "/channels",
			Summary: "Remove a channel pointer from a process",
			Tags:    []string{"Channels"},
			Req:     DeleteChannelReq{Name: "order_pipeline", Channel: "stable"},
			Resp:    map[string]any{"deleted": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.deleteChannel(env.Payload)
			},
		},
		{
			Name:    "list_channels",
			Allow:   []Perm{PermRead},
			Method:  http.MethodGet,
			Path:    "/channels",
			Summary: "List all channels for a process",
			Tags:    []string{"Channels"},
			PathQuery: struct {
				Name string `query:"name" description:"Process name"`
				pageQuery
			}{},
			Resp: PageResp[ChannelEntry]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListChannelsReq{
					Name:       r.URL.Query().Get("name"),
					Pagination: paginationFrom(r),
				})
				return Envelope{Action: "list_channels", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listChannels(env.Payload)
			},
		},
		{
			Name:    "promote_channel",
			Allow:   []Perm{PermDeploy},
			Method:  http.MethodPost,
			Path:    "/channels/promote",
			Summary: "Copy all channel pointers from one channel to another (optionally scoped to a process subtree)",
			Tags:    []string{"Channels"},
			Errors:  []Code{CodeNotFound},
			Req:     PromoteChannelReq{From: "staging", To: "latest"},
			Resp:    map[string]any{"from": "staging", "to": "latest", "promoted": []any{}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.promoteChannel(env.Payload)
			},
		},
		{
			Name:    "channel_status",
			Allow:   []Perm{PermRead},
			Method:  http.MethodPost,
			Path:    "/channels/status",
			Summary: "Report stale child references within a channel",
			Tags:    []string{"Channels"},
			Errors:  []Code{CodeNotFound},
			Req:     ChannelStatusReq{Channel: "latest"},
			Resp:    []ChannelStatusItem{},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.channelStatus(env.Payload)
			},
		},
		{
			Name:    "validate_definitions",
			Allow:   []Perm{PermRead},
			Method:  http.MethodPost,
			Path:    "/definitions/validate",
			Summary: "Validate process definitions and return inferred JSON schemas (no save)",
			Tags:    []string{"Definitions"},
			Req: []model.ProcessDefinition{
				{
					Name:  "order_pipeline",
					Tasks: []*model.Task{{ID: "charge", Action: &model.Action{Type: model.ActionTypeFetch, URL: "http://localhost:9001/charge"}}},
				},
			},
			Resp: []map[string]any{{"process": "order_pipeline", "version": 1}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.validateDefinitions(env.Payload)
			},
		},
		{
			Name:    "compare_definitions",
			Allow:   []Perm{PermRead},
			Method:  http.MethodPost,
			Path:    "/definitions/compat",
			Summary: "Compare two sets of process versions: could an instance running one continue under the other, and does the newer one still honour the output contract consumers were written against. It is a shape check — it cannot see a change of meaning such as dollars becoming cents",
			Tags:    []string{"Definitions"},
			Errors:  []Code{CodeNotFound},
			Req: CompatReq{
				From:    CompatSelector{Channel: "latest"},
				To:      CompatSelector{Versions: map[string]VersionRef{"order_pipeline": {Version: 3}}},
				Process: "order_pipeline",
			},
			Resp: CompatResp{
				Compatible: true,
				Processes: []validation.Report{{
					Name: "order_pipeline", Status: validation.StatusCompared,
					FromVersion: 1, ToVersion: 3,
					Upgrade:  validation.Verdict{Compatible: true},
					Contract: validation.Verdict{},
					Changed:  []validation.SlotChange{{Address: "ship:fetch.url", Task: "ship"}},
					Issues: []validation.Issue{{
						Member: validation.MemberContract, Address: "output",
						Path: "fee", Message: "number → string", Gating: true,
					}},
				}},
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.definitionsCompat(env.Payload)
			},
		},
		{
			Name:    "get_instance_detail",
			Allow:   []Perm{PermRead},
			Method:  http.MethodGet,
			Path:    "/instances/{id}/detail",
			Summary: "Get everything stored on a process instance: its state verbatim, plus the columns around it",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID      string `path:"id" format:"uuid"`
				Resolve bool   `query:"resolve" description:"Splice externalized values into the state where they fit; anything over the per-object limit stays listed under objects for the caller to fetch"`
			}{},
			Resp: InstanceDetailResp{
				ID: "550e8400-e29b-41d4-a716-446655440000", Process: "order_pipeline",
				Version: 1, Status: model.StatusRunning, Task: "charge_card",
				State: map[string]any{
					"input":     map[string]any{"order_id": 42},
					"outputs":   map[string]any{"reserve": map[string]any{"ok": true}},
					"_children": map[string]any{"charge_card": "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"},
				},
				TaskEpoch: 3,
				Objects:   []ObjectEntry{{Path: []any{"state", "outputs", "render"}, Ref: "9f2ac1b4e7d05f38", Size: 221110}},
			},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				resolve, _ := strconv.ParseBool(r.URL.Query().Get("resolve"))
				b, _ := json.Marshal(map[string]bool{"resolve": resolve})
				return Envelope{Action: "get_instance_detail", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				p, err := decodeOptionalBody[struct {
					Resolve bool `json:"resolve"`
				}](env.Payload)
				if err != nil {
					return errReply(err)
				}
				return h.getInstanceDetail(env.ID, p.Resolve)
			},
		},
		{
			Name:    "get_instance",
			Allow:   []Perm{PermRead},
			Method:  http.MethodGet,
			Path:    "/instances/{id}",
			Summary: "Get status of a process instance",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: InstanceStatusResp{
				ID: "550e8400-e29b-41d4-a716-446655440000", Process: "order_pipeline",
				Version: 1, Status: model.StatusFailed, Task: "charge_card",
				ErrorCode: "only_once.interrupted", ErrorMessage: "the task may have already run",
			},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				return Envelope{Action: "get_instance", ID: r.PathValue("id")}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.getInstance(env.ID)
			},
		},
		{
			Name:    "list_instance_logs",
			Allow:   []Perm{PermRead},
			Method:  http.MethodGet,
			Path:    "/instances/{id}/logs",
			Summary: "Get the execution audit trail for a process instance (newest first)",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID            string `path:"id" format:"uuid"`
				Level         string `query:"level" enum:"debug,info,warn,error" description:"Filter by log level"`
				CreatedAfter  int64  `query:"created_after" description:"Only logs at/after this unix-millis timestamp"`
				CreatedBefore int64  `query:"created_before" description:"Only logs strictly before this unix-millis timestamp"`
				Recursive     bool   `query:"recursive" description:"Include the whole process subtree, keyed on the root instance"`
				pageQuery
			}{},
			Resp: PageResp[LogEntryResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				q := r.URL.Query()
				recursive, _ := strconv.ParseBool(q.Get("recursive"))
				b, _ := json.Marshal(ListLogsReq{
					Level:         q.Get("level"),
					CreatedAfter:  millisQuery(r, "created_after"),
					CreatedBefore: millisQuery(r, "created_before"),
					Recursive:     recursive,
					Pagination:    paginationFrom(r),
				})
				return Envelope{Action: "list_instance_logs", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstanceLogs(env.ID, env.Payload)
			},
		},
		{
			Name:    "get_object",
			Allow:   []Perm{PermWorker, PermRead},
			Method:  http.MethodGet,
			Path:    "/objects/{ref}",
			Summary: "Fetch an externalized value by its content hash, as listed in a response's objects section",
			Tags:    []string{"Objects"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				Ref string `path:"ref"`
			}{},
			Resp: map[string]any{"data": ""},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(map[string]string{"ref": r.PathValue("ref")})
				return Envelope{Action: "get_object", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				p, err := decodeOptionalBody[struct {
					Ref string `json:"ref"`
				}](env.Payload)
				if err != nil {
					return errReply(err)
				}
				return h.getObject(p.Ref)
			},
		},
		{
			Name:    "pause_instance",
			Allow:   []Perm{PermOperate},
			Method:  http.MethodPost,
			Path:    "/instances/{id}/pause",
			Summary: "Pause a running root process instance and its entire descendant tree; takes effect at the next task boundary, so a task already executing runs to completion. An assertion: 200 if the tree stopped, 202 if a task already in flight is still draining, 204 if it was not running anyway",
			Tags:    []string{"Instances"},
			// No CodeConflict: a tree that is already stopped satisfies the assertion and
			// comes back 204, not 409. specs/id-list-commands.md.
			Errors: []Code{CodeNotFound},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: LifecycleResp{},
			AltSuccess: []altResp{
				{Status: http.StatusAccepted, Body: LifecycleResp{}},
				{Status: http.StatusNoContent},
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.pauseInstance(env.ID)
			},
		},
		{
			Name:    "resume_instance",
			Allow:   []Perm{PermOperate},
			Method:  http.MethodPost,
			Path:    "/instances/{id}/resume",
			Summary: "Resume a paused root process instance and its tree, continuing exactly where it stopped (timers kept running while paused). An assertion: 200 if it was resumed, 204 if the tree was already advancing; 409 only if it has settled and cannot advance again",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: LifecycleResp{},
			// No 202: a resume is atomic, nothing is left draining (pause-resume.md §7).
			AltSuccess: []altResp{{Status: http.StatusNoContent}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.resumeInstance(env.ID)
			},
		},
		{
			Name:    "retry_instance",
			Allow:   []Perm{PermOperate},
			Method:  http.MethodPost,
			Path:    "/instances/{id}/retry",
			Summary: "Retry a failed root process instance, reviving its tree where it died and granting the failing task another attempt beyond its on_error budget",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			PathQuery: struct {
				ID    string `path:"id" format:"uuid"`
				Force bool   `query:"force" description:"Override only_once retry protection"`
			}{},
			Resp: LifecycleResp{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
				b, _ := json.Marshal(RetryInstanceReq{Force: force})
				return Envelope{Action: "retry_instance", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.retryInstance(env.ID, env.Payload)
			},
		},
		{
			Name:    "upgrade_instance",
			Allow:   []Perm{PermDeploy},
			Method:  http.MethodPost,
			Path:    "/instances/{id}/upgrade",
			Summary: "Move a process instance and every live descendant to another version of their definitions",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound, CodeConflict, CodeInvalid},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Req:  UpgradeInstanceReq{},
			Resp: UpgradeResp{},
			// No fromHTTP: the default envelope already reads the body and takes {id} from
			// the path, which is exactly this endpoint's shape.
			handle: func(h *Handlers, env Envelope) Reply {
				return h.upgradeInstance(env.ID, env.Payload)
			},
		},
		{
			Name:    "claim_external_tasks",
			Allow:   []Perm{PermWorker},
			Method:  http.MethodPost,
			Path:    "/external-tasks/claim",
			Summary: "Lease parked external tasks to a worker (FIFO by park time); the returned token is the only handle accepted while the claim is live",
			Tags:    []string{"External Tasks"},
			Req:     ClaimExternalTasksReq{WorkerID: "worker-1", Limit: 5, LeaseMs: 30000, Process: "expense-approval"},
			Resp:    map[string]any{"items": []ExternalTaskResp{}},
			handle:  func(h *Handlers, env Envelope) Reply { return h.claimExternalTasks(env.Payload) },
		},
		{
			Name:    "renew_external_claims",
			Allow:   []Perm{PermWorker},
			Method:  http.MethodPost,
			Path:    "/external-tasks/renew",
			Summary: "Extend this worker's claims; `renewed` reports how many it still held",
			Tags:    []string{"External Tasks"},
			Req:     RenewExternalClaimsReq{WorkerID: "worker-1", Tokens: []string{"550e8400-e29b-41d4-a716-446655440000.6.1"}, LeaseMs: 30000},
			Resp:    map[string]any{"renewed": 1, "requested": 1},
			handle:  func(h *Handlers, env Envelope) Reply { return h.renewExternalClaims(env.Payload) },
		},
		{
			Name:    "release_external_task",
			Allow:   []Perm{PermWorker},
			Method:  http.MethodPost,
			Path:    "/external-tasks/release",
			Summary: "Hand a claim back to the queue immediately instead of waiting out its lease",
			Tags:    []string{"External Tasks"},
			Errors:  []Code{CodeConflict},
			Req:     ReleaseExternalTaskReq{Token: "550e8400-e29b-41d4-a716-446655440000.6.1"},
			Resp:    map[string]any{"released": true},
			handle:  func(h *Handlers, env Envelope) Reply { return h.releaseExternalTask(env.Payload) },
		},
		{
			Name:    "resolve_external_task",
			Allow:   []Perm{PermWorker},
			Method:  http.MethodPost,
			Path:    "/external-tasks/resolve",
			Summary: "Submit an outcome for a waiting external task — a result, or an `error` routed through the task's on_error rules; validated against what the task declares, then the process resumes",
			Tags:    []string{"External Tasks"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			Req: ResolveExternalTaskReq{
				Token:  "550e8400-e29b-41d4-a716-446655440000.6ba7b810-9dad-11d1-80b4-00c04fd430c8",
				Result: map[string]any{"approved": true},
			},
			Resp: map[string]any{"resolved": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.resolveExternalTask(env.Payload)
			},
		},
		{
			Name:    "signal_instance",
			Allow:   []Perm{PermWorker},
			Method:  http.MethodPost,
			Path:    "/external-tasks/signal",
			Summary: "Deliver an outcome (result or error) to an external task by instance id + task id: resolves it if armed now, else buffers FIFO until the task next arms",
			Tags:    []string{"External Tasks"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			Req: SignalInstanceReq{
				InstanceID: "550e8400-e29b-41d4-a716-446655440000",
				TaskID:     "approval",
				Result:     map[string]any{"approved": true},
			},
			Resp: map[string]any{"delivered": true, "buffered": false},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.signalInstance(env.Payload)
			},
		},
		{
			Name:    "create_token",
			Allow:   []Perm{PermAdmin},
			Method:  http.MethodPost,
			Path:    "/tokens",
			Summary: "Mint an API token. The secret is returned once and never again — only its hash is stored",
			Tags:    []string{"Tokens"},
			Req:     CreateTokenReq{Label: "ci", Perms: []string{"deploy", "read"}},
			Resp:    CreateTokenResp{ID: "01a0…", Token: "genroc_sk_…", Label: "ci", Perms: []string{"deploy", "read"}},
			handle:  func(h *Handlers, env Envelope) Reply { return h.createToken(env.Payload) },
		},
		{
			Name:    "list_tokens",
			Allow:   []Perm{PermAdmin},
			Method:  http.MethodGet,
			Path:    "/tokens",
			Summary: "List API tokens. Secrets are never included — the row cannot produce one",
			Tags:    []string{"Tokens"},
			Resp:    map[string]any{"items": []TokenResp{}},
			handle:  func(h *Handlers, _ Envelope) Reply { return h.listTokens() },
		},
		{
			Name:    "revoke_token",
			Allow:   []Perm{PermAdmin},
			Method:  http.MethodDelete,
			Path:    "/tokens/{id}",
			Summary: "Revoke an API token. Takes effect on the next request that presents it",
			Tags:    []string{"Tokens"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID string `path:"id"`
			}{},
			Resp: map[string]any{"revoked": true},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				return Envelope{Action: "revoke_token", ID: r.PathValue("id")}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply { return h.revokeToken(env.ID) },
		},
		{
			Name:    "health",
			Open:    true,
			Method:  http.MethodGet,
			Path:    "/healthz",
			Root:    true,
			Summary: "Readiness probe: 200 when this worker can reach its database, 503 when it cannot",
			Tags:    []string{"Debug"},
			Errors:  []Code{CodeUnavailable},
			Resp: HealthResp{
				Status: "ok", Worker: "genroc-7f3c-1", Database: "postgres",
				LeaseAgeMs: 1200, ManualTick: false,
			},
			handle: func(h *Handlers, _ Envelope) Reply { return h.health() },
		},
		{
			Name:    "tick",
			Method:  http.MethodPost,
			Path:    "/tick",
			Summary: "Manually trigger one engine poll cycle (useful when started with -poll 0); optionally shift the server clock forward first to expire leases and retry timers without real waits (testing only)",
			Tags:    []string{"Debug"},
			Errors:  []Code{CodeUnsupported},
			Req:     TickReq{AdvanceMs: 12_000},
			Resp:    map[string]any{"count": 0},
			handle:  func(h *Handlers, env Envelope) Reply { return h.tick(env.Payload) },
		},
	}
}()
