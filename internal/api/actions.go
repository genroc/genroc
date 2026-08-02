package api

import (
	"encoding/json"
	"genroc/internal/numeric"
	"net/http"
	"strconv"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// actionDef is the single source of truth for one API action.
// It drives HTTP routing (Method + Path) and OpenAPI documentation
// (schemas reflected from Go types).
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

	// Resp is a zero-value of the response data type.
	Resp any

	// Errors are the failure codes this action can produce, documented in the spec as
	// the corresponding statuses. CodeInvalid and CodeInternal are implicit — every
	// action can reject a body and every action can fail — so list only the extras.
	Errors []Code

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
						TimeoutMs: 5000, OnError: []model.ErrorCase{{Retries: 3}},
						Switch: model.SwitchMap{
							{Case: "self.output.charged == true", Goto: "$ship"},
							{Goto: "$refund"},
						},
					},
					{
						ID:        "ship",
						Action:    &model.Action{Type: model.ActionTypeFetch, URL: "http://localhost:9002/ship"},
						Switch:    model.SwitchMap{{Goto: model.GotoEnd}},
						TimeoutMs: 3000, OnError: []model.ErrorCase{{Retries: 2}},
					},
					{
						ID:        "refund",
						Action:    &model.Action{Type: model.ActionTypeFetch, URL: "http://localhost:9003/refund"},
						Switch:    model.SwitchMap{{Goto: model.GotoEnd}},
						TimeoutMs: 3000, OnError: []model.ErrorCase{{Retries: 1}},
					},
				},
			},
			Resp: map[string]any{"name": "order_pipeline", "version": 1, "saved": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putDefinition(env.Payload)
			},
		},
		{
			Name:      "list_definitions",
			Method:    http.MethodGet,
			Path:      "/definitions",
			Summary:   "List all registered process definitions",
			Tags:      []string{"Definitions"},
			PathQuery: struct{ pageQuery }{},
			Resp:      PageResp[DefinitionSummary]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListDefinitionsReq{Pagination: paginationFrom(r)})
				return Envelope{Action: "list_definitions", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listDefinitions(env.Payload)
			},
		},
		{
			Name:    "start_instance",
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
			Method:  http.MethodGet,
			Path:    "/instances",
			Summary: "List process instances",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				Status    string `query:"status" enum:"running,completed,failing,failed,raised,pausing,paused" description:"Filter by status"`
				ErrorCode string `query:"error_code" description:"Filter by exact error code. Authored codes (from a raise or panic clause) are lower_snake_case; engine-produced codes contain a dot, e.g. http.500, pre.timeout, engine.spawn."`
				pageQuery
			}{},
			Resp: PageResp[InstanceSummaryResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListInstancesReq{
					Status:     r.URL.Query().Get("status"),
					ErrorCode:  r.URL.Query().Get("error_code"),
					Pagination: paginationFrom(r),
				})
				return Envelope{Action: "list_instances", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstances(env.Payload)
			},
		},
		{
			Name:    "put_definitions_batch",
			Method:  http.MethodPut,
			Path:    "/definitions/batch",
			Summary: "Apply process definitions to a channel with optional auto-update of dependents",
			Tags:    []string{"Definitions"},
			Req: PutDefinitionsBatchReq{
				Channel:           "latest",
				AutoUpdateParents: false,
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
			Name:    "get_instance",
			Method:  http.MethodGet,
			Path:    "/instances/{id}",
			Summary: "Get status of a process instance",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID      string `path:"id" format:"uuid"`
				Resolve bool   `query:"resolve" description:"Resolve externalized context values inline; default false returns large values as {ref, size} references"`
			}{},
			Resp: InstanceStatusResp{
				InstanceSummaryResp: InstanceSummaryResp{
					ID: "550e8400-e29b-41d4-a716-446655440000", Process: "order_pipeline",
					Version: 1, Status: model.StatusFailed, Task: "charge_card",
					ErrorCode: "only_once.interrupted",
				},
				Context: map[string]any{"order_id": 42, "charged": true},
			},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				resolve, _ := strconv.ParseBool(r.URL.Query().Get("resolve"))
				b, _ := json.Marshal(map[string]bool{"resolve": resolve})
				return Envelope{Action: "get_instance", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				p, err := decodeOptionalBody[struct {
					Resolve bool `json:"resolve"`
				}](env.Payload)
				if err != nil {
					return errReply(err)
				}
				return h.getInstance(env.ID, p.Resolve)
			},
		},
		{
			Name:    "list_instance_logs",
			Method:  http.MethodGet,
			Path:    "/instances/{id}/logs",
			Summary: "Get the execution audit trail for a process instance (oldest first)",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID        string `path:"id" format:"uuid"`
				Level     string `query:"level" enum:"debug,info,warn,error" description:"Filter by log level"`
				Since     int64  `query:"since" description:"Only logs at/after this unix-millis timestamp"`
				Recursive bool   `query:"recursive" description:"Include the whole process subtree, keyed on the root instance"`
				Resolve   bool   `query:"resolve" description:"Inline full externalized payloads; default false returns a preview + data_ref"`
				pageQuery
			}{},
			Resp: PageResp[LogEntryResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				q := r.URL.Query()
				since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
				recursive, _ := strconv.ParseBool(q.Get("recursive"))
				resolve, _ := strconv.ParseBool(q.Get("resolve"))
				b, _ := json.Marshal(ListLogsReq{
					Level:      q.Get("level"),
					Since:      since,
					Recursive:  recursive,
					Resolve:    resolve,
					Pagination: paginationFrom(r),
				})
				return Envelope{Action: "list_instance_logs", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstanceLogs(env.ID, env.Payload)
			},
		},
		{
			Name:    "get_log_object",
			Method:  http.MethodGet,
			Path:    "/instances/{id}/objects/{ref}",
			Summary: "Fetch the full payload of an externalized log entry (referenced by a log's data_ref)",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound},
			PathQuery: struct {
				ID  string `path:"id" format:"uuid"`
				Ref string `path:"ref"`
			}{},
			Resp: map[string]any{"data": ""},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(map[string]string{"ref": r.PathValue("ref")})
				return Envelope{Action: "get_log_object", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				p, err := decodeOptionalBody[struct {
					Ref string `json:"ref"`
				}](env.Payload)
				if err != nil {
					return errReply(err)
				}
				return h.getLogObject(env.ID, p.Ref)
			},
		},
		{
			Name:    "pause_instance",
			Method:  http.MethodPost,
			Path:    "/instances/{id}/pause",
			Summary: "Pause a running root process instance and its entire descendant tree; takes effect at the next task boundary, so a task already executing runs to completion",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: map[string]any{"paused": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.pauseInstance(env.ID)
			},
		},
		{
			Name:    "resume_instance",
			Method:  http.MethodPost,
			Path:    "/instances/{id}/resume",
			Summary: "Resume a paused root process instance and its tree, continuing exactly where it stopped (timers kept running while paused)",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: map[string]any{"resumed": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.resumeInstance(env.ID)
			},
		},
		{
			Name:    "retry_instance",
			Method:  http.MethodPost,
			Path:    "/instances/{id}/retry",
			Summary: "Retry a failed root process instance, reviving its tree where it died and granting the failing task another attempt beyond its on_error budget",
			Tags:    []string{"Instances"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			PathQuery: struct {
				ID    string `path:"id" format:"uuid"`
				Force bool   `query:"force" description:"Override only_once retry protection"`
			}{},
			Resp: map[string]any{"retried": true},
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
			Name:    "list_external_tasks",
			Method:  http.MethodGet,
			Path:    "/external-tasks",
			Summary: "List instances parked on an external task (the external-task queue); never exposes process context",
			Tags:    []string{"External Tasks"},
			PathQuery: struct {
				Process string `query:"process" description:"Filter by process name"`
				Version int    `query:"version" description:"Filter by process version (0 = any)"`
				Task    string `query:"task" description:"Filter by task id"`
				pageQuery
			}{},
			Resp: PageResp[ExternalTaskResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				q := r.URL.Query()
				version, _ := strconv.Atoi(q.Get("version"))
				b, _ := json.Marshal(ListExternalTasksReq{
					Process:    q.Get("process"),
					Version:    version,
					Task:       q.Get("task"),
					Pagination: paginationFrom(r),
				})
				return Envelope{Action: "list_external_tasks", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listExternalTasks(env.Payload)
			},
		},
		{
			Name:    "resolve_external_task",
			Method:  http.MethodPost,
			Path:    "/external-tasks/resolve",
			Summary: "Submit a result for a waiting external task; validates it against the task's result_schema and resumes the process",
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
			Method:  http.MethodPost,
			Path:    "/instances/{id}/signal",
			Summary: "Deliver a signal (result) to an external task by id: resolves it if armed now, else buffers FIFO until the task next arms",
			Tags:    []string{"External Tasks"},
			Errors:  []Code{CodeNotFound, CodeConflict},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Req:  SignalInstanceReq{TaskID: "approval", Result: map[string]any{"approved": true}},
			Resp: map[string]any{"delivered": true, "buffered": false},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.signalInstance(env.ID, env.Payload)
			},
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
