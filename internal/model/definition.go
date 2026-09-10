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
	Input        *Shape         `json:"input,omitempty"         description:"Templated value building the child's input payload. Of self, only self.previous is in scope — the action has not run yet."`
	ResultSchema *schema.Schema `json:"result_schema,omitempty" description:"JSON Schema validating and exposing this child's output; declaring a shape narrows what the child left untyped."`
	Raises       Raises         `json:"raises,omitempty"        description:"Shapes this child's raised faults carry, keyed by raise code. A declared code is readable as error.data in a rule that catches it; an undeclared one leaves it absent."`
}

// Raises maps a raise code to the shape that code's fault data carries, declared by the
// CALLER rather than the child — which is what keeps a generic child generic: it emits a
// payload, and each caller says what it expects of it, exactly as result_schema narrows an
// untyped output. specs/error-extensions.md §X2-c.
//
// The value is a schema, or **null** for a code whose fault carries no data. Null rather than
// a boolean because this is a schema position and genroc has no boolean schemas: `true` there
// is JSON Schema's "any value validates", the opposite of what it would mean here, and
// `internal/schema` rejects the boolean form outright so that "closed" is always spelled by
// absence. No payload is the absence of a schema.
//
// Null is DISTINCT from omitting the code, which declares nothing at all — no weight on a
// child, where the raisable set comes from the child's own definition, but load-bearing on an
// external task, whose raises IS the closed set of codes a worker may submit.
type Raises map[string]*schema.Schema

// Action describes how to invoke a task's action. It is a discriminated union on Type.
//   - "fetch":      URL (required), Method (optional, default POST), Headers (optional),
//     Query (optional), AcceptedStatus (optional), Body (optional), Responses (optional) — an HTTP call
//     like fetch(url, {method, headers, body}); every field is an expression/shape, so the
//     whole request can come from the context. The body is sent raw (an object as JSON).
//     A fetch has no ResultSchema: Responses types the body per status instead.
//   - "child":      Name (required), Version (optional), Input (optional), ResultSchema (optional),
//     Raises (optional) —
//     runs one named child process and waits for it; the result is that child's output directly
//     (unwrapped), unlike child_map's keyed object. Use it when a task delegates to a single child.
//   - "child_map":  Children (required, keyed map) — concurrent named child processes; the result is
//     an object keyed by child name.
//   - "child_list": Name (required), Over (required), Version (optional), ResultSchema (optional),
//     Raises (optional) —
//     runs one child per element of the Over array; each element is that child's input, and the
//     collected result is an array of the children's outputs in the same order as Over.
//   - "delay":      exactly one of For / Until (required), TZ (optional) — pauses the instance
//     without holding a worker, then routes via switch. For is a duration from arm time
//     ("2h30m", "1d 12h"); Until is an instant ("+2d 08:00", "*-*-01 08:00", "*:*:00" for
//     every whole minute, RFC 3339). Both
//     also accept a bare number (milliseconds for For, unix milliseconds for Until) and a
//     "$:" expression inferring to number; a "${ }" interpolation is rejected, because it
//     would produce a string at runtime. See internal/delayspec for the literal grammars.
//   - "external":   Input (optional), ResultSchema (optional), Raises (optional) — parks the
//     instance until an outside caller submits a result (or a failure) via the external-tasks
//     API; no worker is held while waiting.
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
// Raises (child/child_list/external; child_map declares it per entry): raise code -> schema
// for the payload that code's fault carries. A declared code makes it readable as error.data
// in a rule that catches the code, an undeclared one leaves the slot absent, and the payload is
// conformed when the fault resolves — a mismatch reports output.invalid in place of the code.
// On an external task the code comes from the worker's /external-tasks/fail submission rather
// than from a child, so the declared set is a contract with the worker, not a knowable set:
// an undeclared code is accepted and reads as an absent slot, exactly as a child's does.
// See specs/error-extensions.md §X2-c and specs/external-task-queue.md.
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
	Raises         Raises                    `json:"raises,omitempty"`          // child/child_list: raise code -> the shape that code's fault data carries (child_map declares per entry)
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
				"description": "HTTP call. URL, method, headers and body are all expressions, so the whole request can be driven from the context.",
				"properties": {
					"type":            {"type": "string", "const": "fetch"},
					"url":             {"type": "string", "description": "Request URL. May contain ${ } interpolations evaluated against the current context (e.g. ${ config.server_url }/path)."},
					"method":          {"type": "string", "description": "HTTP method, a template (e.g. GET, POST, ${ input.method }). Defaults to POST."},
					"headers":         __HEADERS_SCHEMA__,
					"query":           __QUERY_SCHEMA__,
					"accepted_status": __ACCEPTED_STATUS_SCHEMA__,
					"body":            {"$ref": "#/$defs/ModelShape", "description": "Templated value building the request body; an object is sent as JSON. Of self, only self.previous is in scope — the action has not run yet."},
					"responses": {
						"type": "object",
						"description": "Status pattern -> JSON Schema for that status's body. Keys are exact codes or hundred-ranges (\"200\", \"400, 401\", \"5xx\"). A 2xx key types self.result and accepts the status; a non-2xx key types error.data.",
						"propertyNames": {"pattern": "^\\s*[1-5](\\d\\d|xx)(\\s*,\\s*[1-5](\\d\\d|xx))*\\s*$"},
						"additionalProperties": {"type": ["object", "null"], "additionalProperties": true}
					}
				},
				"required": ["type", "url"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Single child-process call: runs one named process and waits. The result is the child's output directly, as outputs.taskID.",
				"properties": {
					"type":          {"type": "string", "const": "child"},
					"name":          {"type": "string", "description": "Name of the child process to invoke."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value building the child's input payload. Of self, only self.previous is in scope — the action has not run yet."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema validating and exposing the child's output. Without it the output is only self.result, in this task's switch."},
					"raises": {
						"type": "object",
						"description": "Raise code -> JSON Schema for that code's payload, readable as error.data in a rule that catches it. null declares a code carrying no data; omitting one leaves error.data absent.",
						"propertyNames": {"pattern": "^[a-z][a-z0-9_]*$"},
						"additionalProperties": {"anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}]}
					}
				},
				"required": ["type", "name"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Keyed child-process call: runs named processes concurrently and waits for all. The result is keyed by child name, as outputs.taskID.childKey.",
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
								"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value building the child's input payload. Of self, only self.previous is in scope — the action has not run yet."},
								"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema validating and exposing this child's output; declaring a shape narrows what the child left untyped."},
								"raises": {
									"type": "object",
									"description": "Raise code -> JSON Schema for that code's payload, per entry since entries can be different processes. Readable as error.data in a rule that catches it.",
									"propertyNames": {"pattern": "^[a-z][a-z0-9_]*$"},
									"additionalProperties": {"anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}]}
								}
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
				"description": "List fan-out: one child per element of 'over', run concurrently. The result is an array of their outputs in 'over' order, as outputs.taskID.",
				"properties": {
					"type":          {"type": "string", "const": "child_list"},
					"name":          {"type": "string", "description": "Name of the child process to invoke for every element."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"over":          {"type": "string", "description": "A $: expression evaluating to an array; one child is spawned per element, with that element as its input. An empty array yields an empty result."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema validating and exposing EACH child's output; the collected result is an array of values conforming to it."},
					"raises": {
						"type": "object",
						"description": "Raise code -> JSON Schema for that code's payload, readable as error.data in a rule that catches it. null declares a code carrying no data; omitting one leaves error.data absent.",
						"propertyNames": {"pattern": "^[a-z][a-z0-9_]*$"},
						"additionalProperties": {"anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}]}
					}
				},
				"required": ["type", "name", "over"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Delay action: parks the instance until a duration elapses (for) or an instant arrives (until), holding no worker. Exactly one of for / until.",
				"properties": {
					"type":  {"type": "string", "const": "delay"},
					"for":   {"type": ["string", "number"], "description": "A duration from when the task is reached: a literal such as \"2h30m\" (units ms, s, m, h, d, w, mo, y), a bare number of milliseconds, or a $: expression yielding milliseconds."},
					"until": {"type": ["string", "number"], "description": "An instant: RFC 3339, \"2026-09-01 08:00\", \"+2d 08:00\", a calendar or clock pattern (\"mon 09:00\", \"*:*:00\"), unix milliseconds, or a $: expression. A past instant resolves immediately."},
					"tz":    {"type": "string", "description": "IANA name (\"Europe/Prague\") or fixed offset (\"+02:00\") for calendar units and wall clocks; defaults to UTC. Abbreviations like \"CET\" are rejected as ambiguous across DST."}
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
				"description": "External task: parks the instance until an outside caller submits a result, holding no worker. An absent timeout waits forever; a timeout raises the catchable external.timeout.",
				"properties": {
					"type":          {"type": "string", "const": "external"},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value snapshotted for the resolver — the only context the queue exposes. Of self, only self.previous is in scope."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema the submitted result is validated against before the instance resumes. Without it any JSON result is accepted, available as self.result."},
					"raises": {
						"type": "object",
						"description": "Code -> JSON Schema for the payload a worker submits to /external-tasks/fail, readable as error.data in a rule that catches it. An undeclared code is still accepted.",
						"propertyNames": {"pattern": "^[a-z][a-z0-9_]*$"},
						"additionalProperties": {"anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}]}
					}
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
	ID       string      `json:"id"                 validate:"required" description:"Task identifier, unique within the definition."`
	Action   *Action     `json:"action,omitempty"                        description:"Describes the action to perform. Omit for switch-only (routing) tasks."`
	Timeout  Timeout     `json:"timeout,omitempty,omitzero"            description:"Maximum execution time for fetch and external tasks: a duration shorthand, or an object naming one of 'for' / 'until' plus 'tz'. 'until' is external-only. Omit for the engine default."`
	OnlyOnce *bool       `json:"only_once,omitempty"                   description:"At-most-once execution: retries are allowed only for pre.* errors or rules with not_reached:true, and never where nothing came back. A crash raises the catchable only_once.interrupted. Defaults to false."`
	OnError  []ErrorCase `json:"on_error,omitempty"                    description:"Ordered error-routing rules evaluated when the call fails. First match wins."`
	Output   *Shape      `json:"output,omitempty"                      description:"Templated value remapping this task's output, evaluated with self.result and self.previous in scope. When set it is what outputs.taskID holds and the switch sees as self.output."`
	Switch   SwitchMap   `json:"switch"                                description:"Required. Routing: a scalar shorthand (\"next\", \"end\", \"$task-id\") or an ordered list of cases whose last entry is a catch-all."`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Versions are assigned by the server on apply; never include a version when submitting definitions.
type ProcessDefinition struct {
	Name         string         `json:"name"         validate:"required" description:"Unique process identifier."`
	Tasks        []*Task        `json:"tasks"        validate:"required,min=1,dive" description:"Ordered list of execution tasks. Control advances linearly unless a switch case redirects."`
	InputSchema  *schema.Schema `json:"input_schema,omitempty"          description:"JSON Schema used to validate the input payload when starting a new instance."`
	ConfigSchema *schema.Schema `json:"config_schema,omitempty"         description:"JSON Schema — a flat object of primitive properties — declaring config variables, each resolved from GENROC_<PROCESS>_<NAME> (or GENROC_GLOBAL_<NAME>) and exposed as config.<NAME>. secret:true redacts it from logs."`
	Defs         schema.Defs    `json:"$defs,omitempty,omitzero"        description:"Shared schema definitions referenced as \"#/$defs/<name>\", which may reference each other. A name colliding with a generated one is kept but renamed with a suffix."`
	Output       *Shape         `json:"output,omitempty"                description:"Templated value (a string expression or nested object of expressions) evaluated at completion to produce the process output."`
}

// OnlyOnceAction reports whether this task is an action the engine must never run twice.
// Both halves matter: only_once on a task with no action has nothing to protect.
func (t *Task) OnlyOnceAction() bool {
	return t != nil && t.Action != nil && t.OnlyOnce != nil && *t.OnlyOnce
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
		if err := normalizeRaises(s.Action.Raises, norm, fmt.Sprintf("task %q action.raises", s.ID)); err != nil {
			return err
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
			for key, entry := range s.Action.Children {
				if err := normalizeRaises(entry.Raises, norm, fmt.Sprintf("task %q action.children[%q].raises", s.ID, key)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// normalizeRaises bakes each declared payload schema self-contained, for the reason the
// responses loop above gives: a `$ref` into the process pool resolves nowhere once inference
// embeds the document in a task context. Maps are reference types, so writing back in place
// updates the caller's map.
func normalizeRaises(r Raises, norm func(*schema.Schema) (*schema.Schema, error), where string) error {
	for code, sc := range r {
		if sc == nil {
			continue // null: the code carries no payload, so there is no document to bake
		}
		normalized, err := norm(sc)
		if err != nil {
			return fmt.Errorf("%s[%q]: %w", where, code, err)
		}
		r[code] = normalized
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
