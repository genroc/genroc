package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"genroc/internal/shape"
)

// Fault is a terminal error, serving both `raise` and `panic`. Only Code is a literal -- a
// computed one would make a definition's raise set uncomputable; Message and Data are evaluated
// when the clause fires. specs/child-error-handling.md R2, specs/error-extensions.md.
type Fault struct {
	Code    string `json:"code"    validate:"required" description:"Error code, lower_snake_case with no dots. A literal — never an expression."`
	Message string `json:"message" validate:"required" description:"Human-readable message. A template: ${ } renders when the clause fires."`
	Data    *Shape `json:"data,omitempty" description:"Structured payload this fault carries, evaluated when the clause fires. Lands on error.data."`
}

// UnmarshalJSON rejects unknown keys, like `switch` and `on_error`: in a clause this small a
// misspelled key looks like a working one.
func (f *Fault) UnmarshalJSON(data []byte) error {
	if err := rejectUnknownFields("raise/panic", data, faultFields); err != nil {
		return err
	}
	type alias Fault // bypass this method
	return json.Unmarshal(data, (*alias)(f))
}

// SwitchCase acts when Case, evaluated with this task's output as "self", is true; an empty Case
// is the catch-all. Exactly one of Goto, Raise and Panic is set — checked at registration, not
// decode, so the message can name the task and case index.
type SwitchCase struct {
	Case  string
	Goto  string
	Raise *Fault
	Panic *Fault
}

// Terminates reports whether the case ends the process rather than routing onward.
func (c SwitchCase) Terminates() bool {
	return c.Goto == GotoEnd || c.Raise != nil || c.Panic != nil
}

// SwitchMap marshals as a JSON object ({"self.paid == true": "ship"}); key order is kept by
// reading tokens sequentially, never via a map.
type SwitchMap []SwitchCase

// switchWireCase serves both directions so the tags cannot drift.
type switchWireCase struct {
	Case  string `json:"case,omitempty"`
	Goto  string `json:"goto,omitempty"`
	Raise *Fault `json:"raise,omitempty"`
	Panic *Fault `json:"panic,omitempty"`
}

func (s SwitchMap) MarshalJSON() ([]byte, error) {
	items := make([]switchWireCase, len(s))
	for i, c := range s {
		items[i] = switchWireCase{Case: c.Case, Goto: c.Goto, Raise: c.Raise, Panic: c.Panic}
	}
	return json.Marshal(items)
}

func (s *SwitchMap) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = nil
		return nil
	}
	// Scalar shorthand: "next", "end", or "$task-id" — desugars to a single catch-all.
	if len(data) > 0 && data[0] == '"' {
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("switch: %w", err)
		}
		if v != GotoEnd && v != GotoNext && !strings.HasPrefix(v, "$") {
			return fmt.Errorf("switch: %q must be \"next\", \"end\", or a task reference like \"$task-id\"", v)
		}
		*s = SwitchMap{{Goto: v}}
		return nil
	}
	// Array form.
	var items []switchWireCase
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("switch: %w", err)
	}
	// Same strictness as on_error, for the mirror typo.
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		for _, item := range raw {
			if err := rejectUnknownFields("switch", item, switchCaseFields); err != nil {
				return err
			}
		}
	}
	*s = (*s)[:0]
	for _, item := range items {
		// Only a goto's shape: R3 is checked at registration, where it can name the task.
		if item.Goto != "" && item.Goto != GotoEnd && item.Goto != GotoNext && !strings.HasPrefix(item.Goto, "$") {
			return fmt.Errorf("switch: goto %q must be \"end\", \"next\", or a task reference like \"$task-id\"", item.Goto)
		}
		*s = append(*s, SwitchCase{Case: item.Case, Goto: item.Goto, Raise: item.Raise, Panic: item.Panic})
	}
	return nil
}

// JSONSchemaBytes returns the JSON Schema for SwitchMap so that OpenAPI
// reflection produces the correct schema for its wire format.
func (SwitchMap) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{
				"type": "string",
				"description": "A single unconditional route: \"next\", \"end\", or \"$task-id\"."
			},
			{
				"type": "array",
				"description": "Ordered routing rules; first match wins and the last must be a catch-all (omit 'case').",
				"items": {
					"type": "object",
					"properties": {
						"case": {"type": "string", "description": "Boolean expression. Omit for a catch-all; must be last."},
						"goto": {"type": "string", "description": "\"end\" to terminate, \"next\" to advance, or \"$task-id\" to jump to a task."},
						"raise": {"$ref": "#/$defs/ModelFault", "description": "Terminate as 'raised' with this code and message — a condition a parent can catch."},
						"panic": {"$ref": "#/$defs/ModelFault", "description": "Terminate as 'failed' with this code and message — a defect. Nothing can catch a panic."}
					},
					"additionalProperties": false
				},
				"minItems": 1
			}
		]
	}`), nil
}

// Shape is shape.Shape, aliased so model field types read model.Shape.
type Shape = shape.Shape

// ErrorCase is one error-routing rule; first match wins and an empty Code is a catch-all. At most
// one of Goto, Raise, Panic: none fails the instance once retries run out, which is what a rule
// existing only to cap retries wants.
type ErrorCase struct {
	Code       []string `json:"code,omitempty"        description:"Error code patterns. '%' is the only wildcard; every other character is literal. Empty = catch-all."`
	Case       string   `json:"case,omitempty"        description:"Extra condition on the matched error, checked alongside code. A false case falls to the next rule."`
	Retry      Retry    `json:"retry,omitempty,omitzero" description:"Retry policy applied before the rule routes. Omit for no retries."`
	Goto       string   `json:"goto,omitempty"        description:"Task to route to when retries are exhausted. '$task-id' or 'end'. Omit to fail the instance."`
	Raise      *Fault   `json:"raise,omitempty"       description:"Terminate as 'raised' with this code and message — a condition a parent can catch."`
	Panic      *Fault   `json:"panic,omitempty"       description:"Terminate as 'failed' with this code and message — a defect. Nothing can catch a panic."`
	NotReached *bool    `json:"not_reached,omitempty" description:"Assert this code means the remote was never reached, so an only_once task may retry it."`
}

// errorCaseWire is the JSON wire form of an ErrorCase, shared by its MarshalJSON and
// UnmarshalJSON so the tags stay in lockstep.
type errorCaseWire struct {
	Code       []string `json:"code,omitempty"`
	Case       string   `json:"case,omitempty"`
	Retry      Retry    `json:"retry,omitempty,omitzero"`
	Goto       string   `json:"goto,omitempty"`
	Raise      *Fault   `json:"raise,omitempty"`
	Panic      *Fault   `json:"panic,omitempty"`
	NotReached *bool    `json:"not_reached,omitempty"`
}

func (e ErrorCase) MarshalJSON() ([]byte, error) {
	w := errorCaseWire{Code: e.Code, Case: e.Case, Retry: e.Retry, Raise: e.Raise, Panic: e.Panic, NotReached: e.NotReached}
	if e.Goto != "" {
		if e.Goto == GotoEnd {
			w.Goto = "end"
		} else {
			w.Goto = "$" + e.Goto
		}
	}
	return json.Marshal(w)
}

// A dropped selector ("case", "code") silently becomes a catch-all, hence rejection plus
// hints. Safe over stored rows: internal/model/CLAUDE.md, on_error §3.
var (
	errorCaseFields  = map[string]bool{"code": true, "case": true, "retry": true, "goto": true, "raise": true, "panic": true, "not_reached": true}
	switchCaseFields = map[string]bool{"case": true, "goto": true, "raise": true, "panic": true}
	faultFields      = map[string]bool{"code": true, "message": true, "data": true}

	// Advice for keys valid elsewhere or formerly valid here, shown only for a key the rule
	// rejects. `retries` must stay — internal/model/CLAUDE.md, retry §1.
	ruleFieldHints = map[string]string{

		"code":    `a switch case selects with "case"; "code" belongs to on_error`,
		"retries": `renamed to "retry": write "retry": 3, or "retry": {retries: 3, delay: "30s"} to shape the backoff`,
	}
)

// rejectUnknownFields sorts so the message is stable, and leaves a malformed document to the
// real decode, which reports it better.
func rejectUnknownFields(where string, data []byte, allowed map[string]bool) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	keys := make([]string, 0, len(probe))
	for k := range probe {
		if !allowed[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	if hint, ok := ruleFieldHints[keys[0]]; ok {
		return fmt.Errorf("%s: unknown field %q - %s", where, keys[0], hint)
	}
	return fmt.Errorf("%s: unknown field %q", where, keys[0])
}

func (e *ErrorCase) UnmarshalJSON(data []byte) error {
	if err := rejectUnknownFields("on_error", data, errorCaseFields); err != nil {
		return err
	}
	// A LIST under `case` is an author reaching for `code`; the generic decode error names
	// only the Go type.
	var probe struct {
		Case json.RawMessage `json:"case"`
	}
	if json.Unmarshal(data, &probe) == nil && len(probe.Case) > 0 && probe.Case[0] == '[' {
		return fmt.Errorf(`on_error: "case" is a boolean expression, not a list - select errors by code with "code"`)
	}
	var w errorCaseWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.Code = w.Code
	e.Case = w.Case
	e.Retry = w.Retry
	e.Raise = w.Raise
	e.Panic = w.Panic
	e.NotReached = w.NotReached
	if w.Goto == "" {
		e.Goto = ""
	} else if w.Goto == "end" {
		e.Goto = GotoEnd
	} else if strings.HasPrefix(w.Goto, "$") {
		e.Goto = w.Goto[1:]
	} else {
		return fmt.Errorf("on_error: goto %q must be \"end\" or a task reference like \"$task-id\"", w.Goto)
	}
	return nil
}

// Terminates is false for a rule with none of goto/raise/panic: it ends the process by the
// engine's generic failure, not an authored clause.
func (e ErrorCase) Terminates() bool {
	return e.Goto == GotoEnd || e.Raise != nil || e.Panic != nil
}
