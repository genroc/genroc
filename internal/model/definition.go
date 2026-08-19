package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"genroc/internal/schema"
	"genroc/internal/shape"
)

// GotoEnd signals process termination. Stored verbatim in SwitchCase.Goto and
// compared against the goto value at runtime; on the wire it is literally "end".
const GotoEnd = "end"

// GotoNext signals advance to the next task in the sequence. Valid only on
// non-terminal tasks; using it on the last task is a validation error.
const GotoNext = "next"

type ActionType string

const (
	ActionTypeFetch     ActionType = "fetch"
	ActionTypeChild     ActionType = "child"
	ActionTypeChildMap  ActionType = "child_map"
	ActionTypeChildList ActionType = "child_list"
	ActionTypeDelay     ActionType = "delay"
	ActionTypeExternal  ActionType = "external"
)

// ChildEntry describes a single named child process in a "child_map" call.
type ChildEntry struct {
	Name         string         `json:"name"                    description:"Name of the child process to invoke."`
	Version      int            `json:"version,omitempty"       description:"Version to run; 0 means latest published version."`
	Input        *Shape         `json:"input,omitempty"         description:"Templated value (a string expression or nested object of expressions) evaluated against the current context to build the child's input payload."`
	ResultSchema *schema.Schema `json:"result_schema,omitempty" description:"JSON Schema to validate and expose this child's output. Declaring a shape where the child leaves a value untyped ({}, the top type) narrows it — the output is conformed against this schema when collected."`
}

// Action describes how to invoke a task's action. It is a discriminated union on Type.
//   - "fetch":      URL (required), Method (optional, default POST), Headers (optional),
//     Query (optional), AcceptedStatus (optional), Body (optional), Responses (optional) — an HTTP call
//     like fetch(url, {method, headers, body}); every field is an expression/shape, so the
//     whole request can come from the context. The body is sent raw (an object as JSON).
//     A fetch has no ResultSchema: Responses types the body per status instead.
//   - "child":      Name (required), Version (optional), Input (optional), ResultSchema (optional) —
//     runs one named child process and waits for it; the result is that child's output directly
//     (unwrapped), unlike child_map's keyed object. Use it when a task delegates to a single child.
//   - "child_map":  Children (required, keyed map) — concurrent named child processes; the result is
//     an object keyed by child name.
//   - "child_list": Name (required), Over (required), Version (optional), ResultSchema (optional) —
//     runs one child per element of the Over array; each element is that child's input, and the
//     collected result is an array of the children's outputs in the same order as Over.
//   - "delay":      exactly one of For / Until (required), TZ (optional) — pauses the instance
//     without holding a worker, then routes via switch. For is a duration from arm time
//     ("2h30m", "1d 12h"); Until is an instant ("+2d 08:00", "*-*-01 08:00", "*:*:00" for
//     every whole minute, RFC 3339). Both
//     also accept a bare number (milliseconds for For, unix milliseconds for Until) and a
//     "$:" expression inferring to number; a "${ }" interpolation is rejected, because it
//     would produce a string at runtime. See internal/delayspec for the literal grammars.
//   - "external":   Input (optional), ResultSchema (optional) — parks the instance until an
//     outside caller submits a result via the external-tasks API; no worker is held while waiting.
//     An optional Task.Timeout (absent = wait forever) raises a catchable "external.timeout"
//     error. It is the one place `until` is accepted: a deadline for a parked task is a real
//     instant ("approve by Friday 17:00"), which no duration from arm time can express.
//
// Body (fetch) / Input (external): templated value evaluated against the current context —
// the raw HTTP request body (fetch), or the snapshot exposed to the resolver via the
// external-tasks queue (external).
//
// ResultSchema (child/child_list/external): when set, the result is validated before the
// instance resumes (the submitted result, for external). Without it the result is available
// only as "self" in this task's switch.
//
// Responses (fetch only): status pattern -> schema for the body that status carries. A 2xx
// key types self.result AND makes that status accepted; a non-2xx key types error.data and
// leaves the status routing through on_error. A present key with a nil schema ("202": null)
// declares that the status carries no body; an empty schema ({}) declares one of unknown
// shape. See specs/fetch-http-surface.md §2.
//
// A result_schema is also the one place an unknown is narrowed: a slot left as `{}`
// (the top type — carried, never read) becomes readable when a consumer restates its
// shape here, and the value is conformed against that shape at runtime.
// See specs/unknown-type.md.
//
// Query (fetch only): a shape evaluating to a map of scalars, URL-encoded and APPENDED to the
// url (which may already carry its own `?a=1`). A null value omits its parameter, so an
// optional parameter needs no conditional; this is deliberately unlike Headers, where a null
// is an error. Interpolating into the url instead escapes nothing, which is the bug class
// this slot exists to close.
//
// AcceptedStatus (fetch only): a shape evaluating to an array of HTTP status patterns
// treated as non-errors ("2xx".."5xx" or a 3-digit code). Defaults to any 2xx.
type Action struct {
	Type           ActionType                `json:"type"`
	URL            string                    `json:"url,omitempty"`             // fetch: request URL (an expression)
	Method         string                    `json:"method,omitempty"`          // fetch: HTTP method (an expression); defaults to POST
	Headers        *Shape                    `json:"headers,omitempty"`         // fetch: request headers (a shape evaluating to a string map)
	Query          *Shape                    `json:"query,omitempty"`           // fetch: query parameters appended to the url; a null value omits its parameter
	AcceptedStatus *Shape                    `json:"accepted_status,omitempty"` // fetch: a shape evaluating to an array of HTTP status patterns accepted as non-errors
	Responses      map[string]*schema.Schema `json:"responses,omitempty"`       // fetch: status pattern -> body schema; a present key with a nil schema declares "no body"
	ResultSchema   *schema.Schema            `json:"result_schema,omitempty"`   // child/child_list/external: validate & persist output
	Name           string                    `json:"name,omitempty"`            // child/child_list
	Version        int                       `json:"version,omitempty"`         // child/child_list
	Body           *Shape                    `json:"body,omitempty"`            // fetch: templated request body
	Input          *Shape                    `json:"input,omitempty"`           // child/external: templated input payload
	Children       map[string]ChildEntry     `json:"children,omitempty"`        // child_map
	Over           string                    `json:"over,omitempty"`            // child_list: expression evaluating to the input array (one child per element)
	DelaySpec                                // delay: exactly one of for / until, plus tz
}

// DelaySpec is a target instant named by exactly one of two slots: `for` (a duration
// measured from now) or `until` (an instant), both resolved in `tz`. It is the delay
// action's entire payload and the object form of a task's Timeout, so a deadline is
// written the same way wherever it appears. Grammars: internal/delayspec.
//
// Do not give this type an UnmarshalJSON. Action embeds it, so the method would be
// promoted and json would hand it the whole action object instead of the three slots —
// every other action field would decode to nothing. Timeout wraps it precisely because
// the wrapper can carry a decoder without Action inheriting one.
type DelaySpec struct {
	For   any    `json:"for,omitempty"`   // a duration — literal ("2h30m"), bare number of milliseconds, or $: numeric expression
	Until any    `json:"until,omitempty"` // an instant — literal ("+2d 08:00"), bare number of unix milliseconds, or $: numeric expression
	TZ    string `json:"tz,omitempty"`    // IANA name or fixed offset the calendar units of `for` / wall clocks of `until` resolve in
}

// JSONSchemaBytes returns Action's schema as a discriminated union so OpenAPI reflection
// emits a proper oneOf. The headers slot's schema is GENERATED from the runtime target
// via shape.RelaxedSchema ("literal or expression" at every node) so editor and validator
// cannot drift; the slot description is merged onto the generated node.
// queryValueSchema is what one query parameter may evaluate to: a scalar, null to omit it, or
// an ARRAY of scalars, which repeats the parameter once per element (`?tag=a&tag=b`).
// Scalars rather than strings-only because the null-omit is the point of the slot and does not
// compose with `${ }` — interpolating a nullable is refused at registration — so a strings-only
// target would make an optional NUMBER parameter unwritable.
func queryValueSchema() schema.Schema {
	scalarOrNull := schema.Type("string", "number", "boolean", "null")
	// null omits, at either level: as the whole value it drops the parameter, as an element it
	// drops that repetition. Elements may be null because there is no filter builtin — refusing
	// them would leave an author holding an array they cannot send.
	return schema.AnyOf(scalarOrNull, schema.Array(scalarOrNull))
}

func (Action) JSONSchemaBytes() ([]byte, error) {
	headers, err := relaxedHeadersSchema()
	if err != nil {
		return nil, err
	}
	acceptedStatus, err := relaxedAcceptedStatusSchema()
	if err != nil {
		return nil, err
	}
	query, err := relaxedQuerySchema()
	if err != nil {
		return nil, err
	}
	out := strings.Replace(actionSchemaTemplate, headersPlaceholder, string(headers), 1)
	out = strings.Replace(out, queryPlaceholder, string(query), 1)
	out = strings.Replace(out, acceptedStatusPlaceholder, string(acceptedStatus), 1)
	return []byte(out), nil
}

// relaxedHeadersSchema builds the editor schema for fetch headers from its object<string>
// target and merges a property-level description onto the generated node.
func relaxedHeadersSchema() ([]byte, error) {
	raw, err := shape.RelaxedSchema(schema.Map(schema.Type("string")))
	if err != nil {
		return nil, err
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	node["description"] = "Request headers, evaluating to an object of string values. Author it as a literal map (each value a ${ } template or a $: expression yielding a string), or as a single $: expression yielding the whole map."
	return json.Marshal(node)
}

// relaxedQuerySchema builds the editor schema for fetch query from its runtime target — a map
// of scalars, null permitted, which is what makes an optional parameter writable without a
// conditional.
func relaxedQuerySchema() ([]byte, error) {
	raw, err := shape.RelaxedSchema(schema.Map(queryValueSchema()))
	if err != nil {
		return nil, err
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	node["description"] = "Query parameters appended to the url, evaluating to an object of scalar values. A null value omits its parameter. Author it as a literal map (each value a ${ } template or a $: expression) or as a single $: expression yielding the whole map."
	return json.Marshal(node)
}

// relaxedAcceptedStatusSchema builds the editor schema for fetch accepted_status from its
// array<string> target and merges a property-level description onto the generated node.
func relaxedAcceptedStatusSchema() ([]byte, error) {
	raw, err := shape.RelaxedSchema(schema.Array(schema.Type("string")))
	if err != nil {
		return nil, err
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	node["description"] = `HTTP status patterns accepted as non-errors, e.g. "2xx" or "404" (defaults to any 2xx). Author it as a literal array (each element a ${ } template or $: expression yielding a string), or as a single $: expression yielding the whole array.`
	return json.Marshal(node)
}

const headersPlaceholder = "__HEADERS_SCHEMA__"
const queryPlaceholder = "__QUERY_SCHEMA__"
const acceptedStatusPlaceholder = "__ACCEPTED_STATUS_SCHEMA__"

var actionSchemaTemplate = `{
		"oneOf": [
			{
				"type": "object",
				"description": "HTTP call — sends a request to a URL, like a fetch(). URL, method, headers, and body are all expressions/shapes, so the whole request can be driven from the context.",
				"properties": {
					"type":            {"type": "string", "const": "fetch"},
					"url":             {"type": "string", "description": "Request URL. May contain ${ } interpolations evaluated against the current context (e.g. ${ config.server_url }/path)."},
					"method":          {"type": "string", "description": "HTTP method, a template (e.g. GET, POST, ${ input.method }). Defaults to POST."},
					"headers":         __HEADERS_SCHEMA__,
					"query":           __QUERY_SCHEMA__,
					"accepted_status": __ACCEPTED_STATUS_SCHEMA__,
					"body":            {"$ref": "#/$defs/ModelShape", "description": "Templated value (string expression or nested object) evaluated against the current context to build the request body. An object is sent as JSON."},
					"responses": {
						"type": "object",
						"description": "Status pattern -> JSON Schema for the body that status carries. A key is a comma-separated list of exact codes and hundred-ranges (\"200\", \"400, 401\", \"5xx\"). A 2xx key types self.result AND makes that status accepted; a non-2xx key types error.data and still routes through on_error. null declares that the status carries no body; {} declares a body of unknown shape.",
						"propertyNames": {"pattern": "^\\s*[1-5](\\d\\d|xx)(\\s*,\\s*[1-5](\\d\\d|xx))*\\s*$"},
						"additionalProperties": {"type": ["object", "null"], "additionalProperties": true}
					}
				},
				"required": ["type", "url"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Single child-process call — runs one named process as a sub-instance and waits for it to complete. The result is the child's output directly (unwrapped), available as outputs.taskID.",
				"properties": {
					"type":          {"type": "string", "const": "child"},
					"name":          {"type": "string", "description": "Name of the child process to invoke."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value (string expression or nested object) evaluated against the current context to build the child's input payload."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and expose the child's output. Without it the output is available only as self.result in this task's switch. Declaring a shape where the child leaves a value untyped ({}, the top type) narrows it, making it readable here; the collected output is conformed against this schema."}
				},
				"required": ["type", "name"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Keyed child-process call — runs one or more named processes concurrently and waits for all to complete. The result is an object keyed by child name, available as outputs.taskID.childKey.",
				"properties": {
					"type": {"type": "string", "const": "child_map"},
					"children": {
						"type": "object",
						"description": "Keyed map of child processes to run concurrently. Keys become the access names in outputs.taskID.",
						"additionalProperties": {
							"type": "object",
							"properties": {
								"name":          {"type": "string", "description": "Name of the child process to invoke."},
								"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
								"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value (string expression or nested object) evaluated against the current context to build the child's input payload."},
								"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and expose this child's output. Declaring a shape where the child leaves a value untyped ({}, the top type) narrows it; the collected output is conformed against this schema."}
							},
							"required": ["name"],
							"additionalProperties": false
						},
						"minProperties": 1
					}
				},
				"required": ["type", "children"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "List fan-out child call — runs one instance of a single child process per element of the 'over' array, concurrently, and waits for all to complete. Each element is that child's input payload. The result is an array of the children's outputs in the same order as 'over', available as outputs.taskID.",
				"properties": {
					"type":          {"type": "string", "const": "child_list"},
					"name":          {"type": "string", "description": "Name of the child process to invoke for every element."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"over":          {"type": "string", "description": "A $: expression evaluating to an array (e.g. \"$: input.items\"); the engine spawns one child per element, passing the element as that child's input. An empty array spawns no children and yields an empty-array result."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and expose EACH child's output. The collected result is an array of values conforming to this schema. Declaring a shape where the child leaves a value untyped ({}, the top type) narrows it, per element."}
				},
				"required": ["type", "name", "over"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Delay action — parks the instance until a duration elapses (for) or an instant arrives (until), without holding a worker, then routes via switch. Exactly one of for / until.",
				"properties": {
					"type":  {"type": "string", "const": "delay"},
					"for":   {"type": ["string", "number"], "description": "A duration from the moment the task is reached: a literal such as \"2h30m\" or \"1d 12h\" (units ms, s, m, h, d, w, mo, y), a bare number of milliseconds, or a $: expression evaluating to milliseconds such as \"$: outputs.x.retry_after\". A quoted number without a unit is rejected as ambiguous."},
					"until": {"type": ["string", "number"], "description": "An instant: \"2026-09-01T08:00:00+02:00\" (RFC 3339), \"2026-09-01 08:00\" (in tz), \"+2d 08:00\" (two days from now at 08:00), \"*-*-01 08:00\" or \"mon 09:00\" (next match), \"*:*:00\" (every whole minute; any clock field may be * or a base/step — \"*:*:*\" is every second, \"*:*:0/5\" every five seconds, \"*:2/5:00\" every five minutes from :02), a bare number of unix milliseconds, or a $: expression evaluating to unix milliseconds. An instant already in the past resolves immediately."},
					"tz":    {"type": "string", "description": "IANA name (\"Europe/Prague\") or fixed offset (\"+02:00\") that for's calendar units and until's wall clocks resolve in; defaults to UTC. Abbreviations such as \"CET\" are rejected — they are ambiguous across DST."}
				},
				"required": ["type"],
				"oneOf": [
					{"required": ["for"]},
					{"required": ["until"]}
				],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "External task — parks the instance until an outside caller submits a result via the external-tasks API; no worker is held while waiting. An optional task timeout (absent = wait forever) raises a catchable external.timeout error, and is the one place an absolute 'until' deadline is accepted.",
				"properties": {
					"type":          {"type": "string", "const": "external"},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value evaluated against the current context, snapshotted and exposed to the resolver via the queue (the only context the resolver sees)."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema the submitted result is validated against before the instance resumes. Without it any JSON result is accepted, available as self.result."}
				},
				"required": ["type"],
				"additionalProperties": false
			}
		],
		"discriminator": {"propertyName": "type"}
	}`

// Task is a single unit of work in a process definition.
// Every task must have a switch (and optionally a call).
//
//   - Action-only (Action set, Switch present): executes the call, then routes via switch.
//   - Switch-only (Action nil, Switch present): pure routing task with no external call.
//   - Both: executes the call first, then evaluates the switch (with this task's output as "self").
//
// Switch is always required. Use the scalar shorthand ("next", "end", "$task-id") for
// simple linear flow, or an array of cases for conditional branching.
// The last case must always be a catch-all (no "case" expression).
// "end" terminates the instance; "next" advances to the next task in the list
// (invalid on the last task — use "end" instead); "$task-id" jumps to a named task.
type Task struct {
	ID       string      `json:"id"                 validate:"required" description:"Unique task identifier. 'end' and 'next' are reserved and cannot be used."`
	Action   *Action     `json:"action,omitempty"                        description:"Describes the action to perform. Omit for switch-only (routing) tasks."`
	Timeout  Timeout     `json:"timeout,omitempty,omitzero"            description:"Maximum execution time, honoured by fetch and external tasks. Either a duration shorthand (\"30s\", \"2h30m\", a bare number of milliseconds, or a $: expression evaluating to milliseconds) resolved in UTC, or an object naming exactly one of 'for' / 'until' plus an optional 'tz'. 'until' is an absolute deadline and is accepted only on an external task. Omit for no timeout of its own: a fetch falls back to the engine default, an external waits indefinitely."`
	OnlyOnce *bool       `json:"only_once,omitempty"                   description:"When true, the engine guarantees at-most-once execution: retries are only allowed for pre.* errors (remote never reached) or on_error rules with not_reached:true. A rule that is not restricted to pre.* needs not_reached:true and must name exact codes; errors where the request left and nothing came back (http.timeout, external.timeout, only_once.interrupted) can never be retried at all. An attempt cut short by a crash raises only_once.interrupted, which on_error can catch to check the system of record and then continue. Defaults to false (retryable)."`
	OnError  []ErrorCase `json:"on_error,omitempty"                    description:"Ordered error-routing rules evaluated when the call fails. First match wins."`
	Output   *Shape      `json:"output,omitempty"                      description:"Templated value that remaps this task's output. Evaluated against the context plus self.result (the action's raw result) and self.previous (this task's prior output). When set, this value is stored as outputs.taskID and seen by the switch as self.output; the raw result is not exported."`
	Switch   SwitchMap   `json:"switch"                                description:"Required. Routing declaration: scalar shorthand (\"next\", \"end\", \"$task-id\") or an ordered list of conditional cases. The last case must be a catch-all (omit 'case')."`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Versions are assigned by the server on apply; never include a version when submitting definitions.
type ProcessDefinition struct {
	Name         string         `json:"name"         validate:"required" description:"Unique process identifier."`
	Tasks        []*Task        `json:"tasks"        validate:"required,min=1,dive" description:"Ordered list of execution tasks. Control advances linearly unless a switch case redirects."`
	InputSchema  *schema.Schema `json:"input_schema,omitempty"          description:"JSON Schema used to validate the input payload when starting a new instance."`
	ConfigSchema *schema.Schema `json:"config_schema,omitempty"         description:"JSON Schema — a flat object whose properties are primitive values (string/integer/number/boolean) — declaring configuration variables. Each is resolved at runtime from GENROC_<PROCESS>_<NAME> (falling back to GENROC_GLOBAL_<NAME>) in the server environment, coerced to its declared type, and exposed to expressions as config.<NAME>. A property may set secret:true to redact its value from logs."`
	Defs         schema.Defs    `json:"$defs,omitempty,omitzero"        description:"Shared schema definitions, referenced from input_schema and result_schemas as \"#/$defs/<name>\". Definitions may reference each other. Generated schema names (input, output, <taskID>_input, <taskID>_output) take precedence: a definition reusing one is kept but renamed with a unique suffix in the generated schemas."`
	Output       *Shape         `json:"output,omitempty"                description:"Templated value (a string expression or nested object of expressions) evaluated at completion to produce the process output."`
}

// Raises returns the set of error codes this definition can raise, sorted. It is a
// purely syntactic scan over every raise clause on every switch case and on_error rule
// — Fault.Code is a literal (R2), so there is no dataflow and no fixpoint, and a
// self-referencing (recursive) process terminates like any other.
//
// The set is statically exact, and where imprecise it errs safe: a raise on an
// unreachable task inflates it, never the reverse. Callers use it two ways — R5 checks
// a parent's on_error rules against the union over its children's raise sets, and the
// definition endpoint publishes it, since with no `errors:` declaration block it is the
// only answer to "what can this process raise?".
//
// Panic codes are deliberately excluded even though panics carry codes. This set is
// what a parent may write rules against, and no rule can ever match a panic: a
// panicking child is 'failed', so it poisons its ancestors and the parent never reaches
// resolution. Including them would let R5 bless rules that can never fire.
func (d *ProcessDefinition) Raises() []string {
	seen := map[string]struct{}{}
	for _, t := range d.Tasks {
		for _, c := range t.Switch {
			if c.Raise != nil {
				seen[c.Raise.Code] = struct{}{}
			}
		}
		for _, ec := range t.OnError {
			if ec.Raise != nil {
				seen[ec.Raise.Code] = struct{}{}
			}
		}
	}
	codes := make([]string, 0, len(seen))
	for code := range seen {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// Normalize normalizes InputSchema and all task result schemas in-place (flatten $defs,
// drop unused definitions, rewrite $refs). Process-level $defs are flattened first and
// made visible to each schema, which comes out self-contained — the shared definitions
// it uses baked into its own root $defs. A schema-local definition wins over a
// process-level one of the same name (nearest-wins).
func (d *ProcessDefinition) Normalize() error {
	if !d.Defs.IsZero() {
		flat, err := d.Defs.Flatten()
		if err != nil {
			return fmt.Errorf("$defs: %w", err)
		}
		d.Defs = flat
	}
	norm := func(s *schema.Schema) (*schema.Schema, error) {
		out, err := s.WithMergedDefs(d.Defs).Normalize()
		return &out, err
	}
	if d.InputSchema != nil {
		normalized, err := norm(d.InputSchema)
		if err != nil {
			return fmt.Errorf("input_schema: %w", err)
		}
		d.InputSchema = normalized
	}
	for _, s := range d.Tasks {
		if s.Action == nil {
			continue
		}
		if s.Action.ResultSchema != nil {
			normalized, err := norm(s.Action.ResultSchema)
			if err != nil {
				return fmt.Errorf("task %q action.result_schema: %w", s.ID, err)
			}
			s.Action.ResultSchema = normalized
		}
		// Each declared status carries its own document, so each is baked self-contained the
		// same way — a `$ref` into the process pool resolves nowhere once inference embeds it
		// in a task context otherwise. A nil entry declares no body and has nothing to bake.
		for key, sc := range s.Action.Responses {
			if sc == nil {
				continue
			}
			normalized, err := norm(sc)
			if err != nil {
				return fmt.Errorf("task %q action.responses[%q]: %w", s.ID, key, err)
			}
			s.Action.Responses[key] = normalized
		}
		if s.Action.Type == ActionTypeChildMap {
			for key, entry := range s.Action.Children {
				if entry.ResultSchema != nil {
					normalized, err := norm(entry.ResultSchema)
					if err != nil {
						return fmt.Errorf("task %q action.children[%q].result_schema: %w", s.ID, key, err)
					}
					entry.ResultSchema = normalized
					s.Action.Children[key] = entry
				}
			}
		}
	}
	return nil
}

// ValidateInput validates input against InputSchema and returns the normalized value
// (undeclared props dropped, defaults filled); passes input through when the schema is nil.
func (d *ProcessDefinition) ValidateInput(input any) (any, error) {
	if d.InputSchema == nil {
		return input, nil
	}
	return d.InputSchema.Validate(input)
}

// ValidateOutput validates output against ResultSchema and returns the normalized value
// (undeclared props dropped, defaults filled); passes output through when the schema is nil.
func (c *Action) ValidateOutput(output any) (any, error) {
	if c.ResultSchema == nil {
		return output, nil
	}
	return c.ResultSchema.Validate(output)
}

// ResultRedactionSchema is the schema governing a result for logging: the per-status one for
// a fetch, ResultSchema for everything else. status 0 means "no HTTP status involved".
// Redaction must resolve the schema the same way validation does — a secret marked on a
// status whose schema the logger cannot find is a secret printed into the audit trail.
func (c *Action) ResultRedactionSchema(status int) *schema.Schema {
	if c.Type == ActionTypeFetch {
		sc, _ := c.ResponseFor(status)
		return sc
	}
	return c.ResultSchema
}

// ValidateResponse validates a fetch response body against the schema declared for its
// status and returns the normalized value. declared=false means no key covered the status,
// which is not an error: an accepted status nobody described carries an untyped body, and an
// unaccepted one simply has no error.data. Enforcement is uniform — a declared status whose
// body does not conform is a failure on both channels, which is the caller's to raise.
func (c *Action) ValidateResponse(status int, body any) (value any, declared bool, err error) {
	sc, ok := c.ResponseFor(status)
	if !ok {
		return body, false, nil
	}
	if sc == nil {
		return nil, true, nil
	}
	v, err := sc.Validate(body)
	return v, true, err
}
