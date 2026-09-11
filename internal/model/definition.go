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
	Input        *Shape         `json:"input,omitempty"         description:"Templated value building the child's input payload."`
	ResultSchema *schema.Schema `json:"result_schema,omitempty" description:"JSON Schema validating and exposing this child's output."`
	Raises       Raises         `json:"raises,omitempty"        description:"Shapes this child's raised faults carry, keyed by raise code. Read as error.data."`
}

// Raises maps a raise code to the shape that code's fault data carries, declared by the
// caller rather than the child. A nil value declares a code whose fault carries no payload,
// which is distinct from omitting the code — on an external task the declared set is the
// closed set of codes a worker may submit. specs/error-extensions.md §X2-c.
type Raises map[string]*schema.Schema

// Action describes how to invoke a task's action, a discriminated union on Type. Required
// per type: fetch — URL; child — Name; child_list — Name and Over; child_map — Children;
// delay — exactly one of For/Until; external — nothing. Every other field belongs to the
// types its own comment names, and validation rejects it elsewhere.
//
// The result differs per type: child yields the child's output unwrapped, child_map an object
// keyed by child name, child_list an array in Over's order, fetch the response body typed by
// Responses (a fetch has no ResultSchema), external whatever the worker submits.
//
// See specs/fetch-http-surface.md, specs/error-extensions.md §X2-c,
// specs/external-task-queue.md, specs/unknown-type.md and internal/delayspec.
type Action struct {
	Type           ActionType                `json:"type"`
	URL            string                    `json:"url,omitempty"`             // fetch: request URL (an expression)
	Method         string                    `json:"method,omitempty"`          // fetch: HTTP method, lowercase (an expression); required
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

// DelaySpec is a target instant named by exactly one of `for` (a duration from now) or
// `until` (an instant), resolved in `tz`. Grammars: internal/delayspec.
//
// Do not give this type an UnmarshalJSON: Action embeds it, so the promoted method would be
// handed the whole action object and every other action field would decode to nothing.
type DelaySpec struct {
	For   any    `json:"for,omitempty"`   // a duration — literal ("2h30m"), bare number of milliseconds, or $: numeric expression
	Until any    `json:"until,omitempty"` // an instant — literal ("+2d 08:00"), bare number of unix milliseconds, or $: numeric expression
	TZ    string `json:"tz,omitempty"`    // IANA name or fixed offset the calendar units of `for` / wall clocks of `until` resolve in
}

// queryValueSchema is what one query parameter may evaluate to: a scalar, null to omit the
// parameter, or an array of scalars repeating it once per element (`?tag=a&tag=b`). Null is
// allowed at either level; elements may be null because there is no filter builtin.
func queryValueSchema() schema.Schema {
	scalarOrNull := schema.Type("string", "number", "boolean", "null")
	return schema.AnyOf(scalarOrNull, schema.Array(scalarOrNull))
}

// JSONSchemaBytes returns Action's schema as a discriminated union so OpenAPI reflection emits
// a oneOf. The relaxed slots are generated from their runtime targets so editor and validator
// cannot drift.
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
				"description": "HTTP call. URL, method, headers and body are all expressions.",
				"properties": {
					"type":            {"type": "string", "const": "fetch"},
					"url":             {"type": "string", "description": "Request URL. May contain ${ } interpolations, e.g. ${ config.server_url }/path."},
					"method":          {"type": "string", "description": "HTTP method, lowercase (e.g. get, post) or a template such as ${ input.method }. Required — the verb is never guessed."},
					"headers":         __HEADERS_SCHEMA__,
					"query":           __QUERY_SCHEMA__,
					"accepted_status": __ACCEPTED_STATUS_SCHEMA__,
					"body":            {"$ref": "#/$defs/ModelShape", "description": "Templated value building the request body; an object is sent as JSON."},
					"responses": {
						"type": "object",
						"description": "Status pattern -> JSON Schema for that body. A 2xx key types self.result and accepts the status.",
						"propertyNames": {"pattern": "^\\s*[1-5](\\d\\d|xx)(\\s*,\\s*[1-5](\\d\\d|xx))*\\s*$"},
						"additionalProperties": {"type": ["object", "null"], "additionalProperties": true}
					}
				},
				"required": ["type", "url", "method"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Single child call: runs one named process and waits. The result is its output, unwrapped.",
				"properties": {
					"type":          {"type": "string", "const": "child"},
					"name":          {"type": "string", "description": "Name of the child process to invoke."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value building the child's input payload."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema validating and exposing the child's output."},
					"raises": {
						"type": "object",
						"description": "Raise code -> JSON Schema for that code's payload, read as error.data by a rule that catches it.",
						"propertyNames": {"pattern": "^[a-z][a-z0-9_]*$"},
						"additionalProperties": {"anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}]}
					}
				},
				"required": ["type", "name"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Keyed child call: named processes run concurrently. The result is keyed by child name.",
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
								"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value building the child's input payload."},
								"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema validating and exposing this child's output."},
								"raises": {
									"type": "object",
									"description": "Raise code -> JSON Schema for that code's payload, declared per entry.",
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
				"description": "List fan-out: one child per element of 'over'. The result is an array in 'over' order.",
				"properties": {
					"type":          {"type": "string", "const": "child_list"},
					"name":          {"type": "string", "description": "Name of the child process to invoke for every element."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"over":          {"type": "string", "description": "A $: expression evaluating to an array; one child is spawned per element, with it as input."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema validating and exposing EACH child's output; the result is an array."},
					"raises": {
						"type": "object",
						"description": "Raise code -> JSON Schema for that code's payload, read as error.data by a rule that catches it.",
						"propertyNames": {"pattern": "^[a-z][a-z0-9_]*$"},
						"additionalProperties": {"anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}]}
					}
				},
				"required": ["type", "name", "over"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Delay action: parks the instance until a duration elapses (for) or an instant arrives (until).",
				"properties": {
					"type":  {"type": "string", "const": "delay"},
					"for":   {"type": ["string", "number"], "description": "A duration from when the task is reached: \"2h30m\", a number of milliseconds, or a $: expression."},
					"until": {"type": ["string", "number"], "description": "An instant: RFC 3339, \"+2d 08:00\", a calendar pattern, unix milliseconds, or a $: expression."},
					"tz":    {"type": "string", "description": "IANA name (\"Europe/Prague\") or fixed offset (\"+02:00\"); defaults to UTC. Abbreviations are rejected."}
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
				"description": "External task: parks the instance until an outside caller submits a result. No worker is held.",
				"properties": {
					"type":          {"type": "string", "const": "external"},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value snapshotted for the resolver — the only context the queue exposes."},
					"result_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema the submitted result is validated against. Without it any JSON is accepted."},
					"raises": {
						"type": "object",
						"description": "Code -> JSON Schema for a payload submitted to /external-tasks/fail, read as error.data.",
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

// Task is a single unit of work: an optional action, then a switch that routes on its result.
// Switch is required — a task with no action is pure routing. Its scalar shorthand is "end"
// (terminate), "next" (the following task, invalid on the last) or "$task-id" (jump); as an
// array of cases the last must be a catch-all with no "case" expression.
type Task struct {
	ID       string      `json:"id"                 validate:"required" description:"Task identifier, unique within the definition."`
	Action   *Action     `json:"action,omitempty"                        description:"Describes the action to perform. Omit for switch-only (routing) tasks."`
	Timeout  Timeout     `json:"timeout,omitempty,omitzero"            description:"Maximum execution time for fetch and external tasks. Omit for the engine default."`
	OnlyOnce *bool       `json:"only_once,omitempty"                   description:"At-most-once execution: only errors that never reached the remote may retry. Defaults to false."`
	OnError  []ErrorCase `json:"on_error,omitempty"                    description:"Ordered error-routing rules evaluated when the call fails. First match wins."`
	Output   *Shape      `json:"output,omitempty"                      description:"Templated value remapping this task's output; it becomes outputs.taskID and self.output."`
	Switch   SwitchMap   `json:"switch"                                description:"Required. Routing: a shorthand (\"next\", \"end\", \"$task-id\") or an ordered list of cases."`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Versions are assigned by the server on apply; never include a version when submitting definitions.
type ProcessDefinition struct {
	Name         string         `json:"name"         validate:"required" description:"Unique process identifier."`
	Tasks        []*Task        `json:"tasks"        validate:"required,min=1,dive" description:"Ordered list of execution tasks. Control advances linearly unless a switch case redirects."`
	InputSchema  *schema.Schema `json:"input_schema,omitempty"          description:"JSON Schema used to validate the input payload when starting a new instance."`
	ConfigSchema *schema.Schema `json:"config_schema,omitempty"         description:"Flat object of primitive config variables, resolved from the server environment and read as config.<NAME>."`
	Defs         schema.Defs    `json:"$defs,omitempty,omitzero"        description:"Shared schema definitions, referenced as \"#/$defs/<name>\"."`
	Output       *Shape         `json:"output,omitempty"                description:"Templated value evaluated at completion to produce the process output."`
}

// OnlyOnceAction reports whether this task is an action the engine must never run twice.
// Both halves matter: only_once on a task with no action has nothing to protect.
func (t *Task) OnlyOnceAction() bool {
	return t != nil && t.Action != nil && t.OnlyOnce != nil && *t.OnlyOnce
}

// Raises returns the set of error codes this definition can raise, sorted — a syntactic scan
// over every raise clause, so it errs safe (a raise on an unreachable task inflates it).
// Panic codes are excluded: a panicking child is 'failed', so no parent rule can ever match
// one and including them would let R5 bless rules that cannot fire.
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

// Normalize normalizes InputSchema and all task result schemas in-place (flatten $defs, drop
// unused definitions, rewrite $refs). Each schema comes out self-contained, with the
// process-level $defs it uses baked into its own root; a schema-local definition of the same
// name wins.
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

// ValidateResponse validates a fetch response body against the schema declared for its status
// and returns the normalized value. declared=false means no key covered the status, which is
// not an error — the body is simply untyped. A declared status whose body does not conform is
// a failure on both channels, which is the caller's to raise.
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
