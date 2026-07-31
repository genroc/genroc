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

// ActionType identifies how the engine invokes a task's action.
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
//     AcceptedStatus (optional), Body (optional), ResultSchema (optional) — an HTTP call
//     like fetch(url, {method, headers, body}); every field is an expression/shape, so the
//     whole request can come from the context. The body is sent raw (an object as JSON).
//   - "child":      Name (required), Version (optional), Input (optional), ResultSchema (optional) —
//     runs one named child process and waits for it; the result is that child's output directly
//     (unwrapped), unlike child_map's keyed object. Use it when a task delegates to a single child.
//   - "child_map":  Children (required, keyed map) — concurrent named child processes; the result is
//     an object keyed by child name.
//   - "child_list": Name (required), Over (required), Version (optional), ResultSchema (optional) —
//     runs one child per element of the Over array; each element is that child's input, and the
//     collected result is an array of the children's outputs in the same order as Over.
//   - "delay":      Ms (required) — pauses the instance for a duration without holding a worker, then routes via switch
//   - "external":   Input (optional), ResultSchema (optional) — parks the instance until an
//     outside caller submits a result via the external-tasks API; no worker is held while waiting.
//     An optional Task.TimeoutMs (0 = wait forever) raises a catchable "external.timeout" error.
//
// Body (fetch) / Input (external): templated value evaluated against the current context —
// the raw HTTP request body (fetch), or the snapshot exposed to the resolver via the
// external-tasks queue (external).
//
// ResultSchema (fetch/external): when set, the result is validated before the instance
// resumes (the submitted result, for external). Without it the result is available only as "self" in
// this task's switch.
//
// A result_schema is also the one place an unknown is narrowed: a slot left as `{}`
// (the top type — carried, never read) becomes readable when a consumer restates its
// shape here, and the value is conformed against that shape at runtime.
// See docs/unknown-type.md.
//
// AcceptedStatus (fetch only): a shape evaluating to an array of HTTP status patterns
// treated as non-errors ("2xx".."5xx" or a 3-digit code). Defaults to any 2xx.
type Action struct {
	Type           ActionType            `json:"type"`
	URL            string                `json:"url,omitempty"`             // fetch: request URL (an expression)
	Method         string                `json:"method,omitempty"`          // fetch: HTTP method (an expression); defaults to POST
	Headers        *Shape                `json:"headers,omitempty"`         // fetch: request headers (a shape evaluating to a string map)
	AcceptedStatus *Shape                `json:"accepted_status,omitempty"` // fetch: a shape evaluating to an array of HTTP status patterns accepted as non-errors
	ResultSchema   *schema.Schema        `json:"result_schema,omitempty"`   // fetch/child/child_list: validate & persist output
	Name           string                `json:"name,omitempty"`            // child/child_list
	Version        int                   `json:"version,omitempty"`         // child/child_list
	Body           *Shape                `json:"body,omitempty"`            // fetch: templated request body
	Input          *Shape                `json:"input,omitempty"`           // child/external: templated input payload
	Children       map[string]ChildEntry `json:"children,omitempty"`        // child_map
	Over           string                `json:"over,omitempty"`            // child_list: expression evaluating to the input array (one child per element)
	Ms             string                `json:"ms,omitempty"`              // delay: milliseconds to pause, as an expression
}

// JSONSchemaBytes returns the JSON Schema for Action as a discriminated union
// so that OpenAPI reflection produces a proper oneOf instead of a flat object.
//
// The headers slot is a Shape whose value must conform to object<string> (the same target
// checkHeadersShape validates against). Rather than hand-write the "object of strings, or an
// expression" schema, it is generated from that target by shape.RelaxedSchema — the relax(S)
// transform that makes every node "the literal value or an expression". A property-level
// description is merged onto the generated node so the editor still explains the slot.
func (Action) JSONSchemaBytes() ([]byte, error) {
	headers, err := relaxedHeadersSchema()
	if err != nil {
		return nil, err
	}
	acceptedStatus, err := relaxedAcceptedStatusSchema()
	if err != nil {
		return nil, err
	}
	out := strings.Replace(actionSchemaTemplate, headersPlaceholder, string(headers), 1)
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
					"accepted_status": __ACCEPTED_STATUS_SCHEMA__,
					"body":            {"$ref": "#/$defs/ModelShape", "description": "Templated value (string expression or nested object) evaluated against the current context to build the request body. An object is sent as JSON."},
					"result_schema":   {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and persist the response body. Without it the response is available only as 'self' in this task's switch."}
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
				"description": "Delay action — pauses the instance for a duration without holding a worker, then routes via switch.",
				"properties": {
					"type": {"type": "string", "const": "delay"},
					"ms":   {"type": "string", "description": "Milliseconds to pause, as an expression: a literal such as \"30000\" or a $: expression such as \"$: outputs.x.retry_after\"."}
				},
				"required": ["type", "ms"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "External task — parks the instance until an outside caller submits a result via the external-tasks API; no worker is held while waiting. An optional task timeout_ms (0 = wait forever) raises a catchable external.timeout error.",
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
	ID        string      `json:"id"                 validate:"required" description:"Unique task identifier. 'end' and 'next' are reserved and cannot be used."`
	Action    *Action     `json:"action,omitempty"                        description:"Describes the action to perform. Omit for switch-only (routing) tasks."`
	TimeoutMs int         `json:"timeout_ms,omitempty"                  description:"Maximum execution time in milliseconds. 0 means no timeout."`
	OnlyOnce  *bool       `json:"only_once,omitempty"                   description:"When true, the engine guarantees at-most-once execution: retries are only allowed for pre.* errors (remote never reached) or on_error rules with not_reached:true. Defaults to false (retryable)."`
	OnError   []ErrorCase `json:"on_error,omitempty"                    description:"Ordered error-routing rules evaluated when the call fails. First match wins."`
	Output    *Shape      `json:"output,omitempty"                      description:"Templated value that remaps this task's output. Evaluated against the context plus self.result (the action's raw result) and self.previous (this task's prior output). When set, this value is stored as outputs.taskID and seen by the switch as self.output; the raw result is not exported."`
	Switch    SwitchMap   `json:"switch"                                description:"Required. Routing declaration: scalar shorthand (\"next\", \"end\", \"$task-id\") or an ordered list of conditional cases. The last case must be a catch-all (omit 'case')."`
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
