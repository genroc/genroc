package model

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"genroc/internal/shape"
)

// Fault is a terminal error: a machine-readable code, a human-readable message, and an
// optional structured payload. Used by both `raise` and `panic` — they carry the same
// thing for the same reasons and differ only in what they do, so one type serves both and
// there is no pair of near-identical structs to drift apart. The distinction lives in
// the field name at the use site (Raise / Panic), which is where a reader looks anyway.
//
// Only the CODE is a literal: a computed one would make a definition's raise set
// uncomputable and error_code unqueryable. Message and Data are evaluated when the clause
// fires, in the scope of the clause itself. See specs/child-error-handling.md §2.1 and R2,
// and specs/error-extensions.md §X2-c for what a parent may read of Data.
type Fault struct {
	Code    string `json:"code"    validate:"required" description:"Error code, lower_snake_case, no dots (dots are reserved for engine-produced codes). A literal — never an expression."`
	Message string `json:"message" validate:"required" description:"Human-readable message explaining the condition. A template: ${ } interpolations are rendered when the clause fires, and must produce a non-null string. Unlike the code, it is not required to be a literal."`
	Data    *Shape `json:"data,omitempty" description:"Structured payload this fault carries: an expression, or an object of expressions, evaluated when the clause fires in the same scope as the message. It lands on this instance's error.data, which an operator reads on the instance detail and in logs. Omit to carry nothing — the slot is then cleared rather than left holding the error this instance caught."`
}

// SwitchCase is a single entry in a Task's switch list: a boolean expression
// evaluated against the process context (and this task's own output as "self"),
// and what to do when the expression is true.
// An empty Case means "catch-all" — it matches unconditionally and must be last.
//
// Exactly one of Goto, Raise and Panic is set (enforced at registration, not on
// decode, so the rejection message can name the task and case index):
//   - Goto routes, storing the raw wire value: "end", "next", or "$task-id".
//   - Raise concludes the process as 'raised' — an anticipated condition its parent
//     may react to by naming the code.
//   - Panic fails the process — a defect nothing may react to, ever.
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

// SwitchMap is an ordered list of SwitchCase entries. It marshals as a plain
// JSON object so the wire format is readable:
//
//	{"self.paid == true": "ship", "self.paid == false": "refund"}
//
// JSON object key order is preserved on unmarshal by reading tokens sequentially
// rather than decoding into a map.
type SwitchMap []SwitchCase

// switchWireCase is the JSON wire form of a SwitchCase, shared by SwitchMap's
// MarshalJSON and UnmarshalJSON so the tags can't drift. omitempty is ignored on
// decode, so the same type serves both directions.
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
		// Only the *shape* of a goto is checked here. Which of goto/raise/panic a case
		// must carry (exactly one — R3) is a registration rule, not a decoding one, so
		// that its rejection can name the task and the case index instead of surfacing
		// as an opaque JSON error.
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
				"description": "Shorthand for a single unconditional route. \"next\" advances to the next task (not valid on the last task), \"end\" terminates the instance, \"$task-id\" jumps to a named task."
			},
			{
				"type": "array",
				"description": "Ordered routing rules evaluated after the call. Cases are evaluated in order; first match wins. The last entry must be a catch-all (omit 'case'). Each case sets exactly one of 'goto', 'raise' or 'panic'.",
				"items": {
					"type": "object",
					"properties": {
						"case": {"type": "string", "description": "Boolean expression. Omit for a catch-all; must be last."},
						"goto": {"type": "string", "description": "\"end\" to terminate, \"next\" to advance, or \"$task-id\" to jump to a task."},
						"raise": {"$ref": "#/$defs/ModelFault", "description": "Terminate as 'raised' with this code and message — an anticipated condition a parent process may react to by naming the code in its on_error."},
						"panic": {"$ref": "#/$defs/ModelFault", "description": "Terminate as 'failed' with this code and message — a defect. Nothing can catch a panic; the code exists to classify the failure, not to branch on it."}
					},
					"additionalProperties": false
				},
				"minItems": 1
			}
		]
	}`), nil
}

// Shape is the templated value used by the data-shaping fields (action input, output,
// process output). The type and all its behaviour (grammar, Infer, Eval) live in the
// self-contained shape package; this alias keeps the model's field types spelled
// model.Shape.
type Shape = shape.Shape

// ErrorCase is a single error-routing rule evaluated when a task's call fails.
// Rules are evaluated in order; the first match applies.
// An empty Code list is a catch-all matching any error.
//
// A rule may route (Goto), conclude the process (Raise), or declare the error a defect
// (Panic) — at most one of the three; setting none fails the instance, which is the
// default when a rule exists only to document a code or to cap retries.
type ErrorCase struct {
	Code       []string `json:"code,omitempty"        description:"Patterns matched against the error code. '%' is the only wildcard (matches any run of characters); every other character, including '_' and '.', is literal — so 'order_%' matches 'order_placed' but not 'order.placed'. Empty list = catch-all. Catchable engine codes (an action task's call reports these): http.NNN (e.g. http.500), http.timeout, http.disconnected, pre.error, pre.timeout, output.parse, output.too_large, output.invalid, external.timeout, external.lost. pre.* codes mean the call never reached the remote; http.disconnected means it did go out and the connection broke before a response, so whether it took effect is unknowable. Internal engine.* failures (engine.spawn, engine.collect, engine.expression, …) are terminal and are NOT routed through on_error. On a child/child_map/child_list task the codes instead match what the child processes can raise, plus output.invalid (a child completed, but its output failed the result_schema this task narrowed it with); each pattern is checked at registration against that set. A child that failed (rather than raised) is never catchable — convert the failure into a raise inside the child."`
	Case       string   `json:"case,omitempty"        description:"Boolean expression checked in ADDITION to code, against the error this rule matched — the same predicate a switch case is, on the error channel. The rule applies only when both hold; a false case falls through to the next rule. Because code has already narrowed which error this is, error.data here is that code's declared shape rather than the union a routed task sees. Omit to match on the code alone. NOTE: with a case, naming a code no longer guarantees the error is handled — an error every rule declines is unmatched, and an unmatched raise fails the instance."`
	Retry      Retry    `json:"retry,omitempty,omitzero" description:"Retry policy applied before the rule routes: a bare attempt count, or an object naming any of attempts / delay / factor / max_delay. Omit for no retries. On only_once:true tasks only pre.* codes (or rules with not_reached:true) may have attempts > 0. On a child task a retry re-spawns the raised slot with its input rebuilt from this definition, so a fix published as a new version is what the next attempt runs; it is refused on an only_once child task, where every catchable code means the child already ran."`
	Goto       string   `json:"goto,omitempty"        description:"Task to route to when retries are exhausted. '$task-id' or 'end'. Omit to fail the instance."`
	Raise      *Fault   `json:"raise,omitempty"       description:"Terminate as 'raised' with this code and message instead of routing — an anticipated condition a parent process may react to. Mutually exclusive with goto and panic."`
	Panic      *Fault   `json:"panic,omitempty"       description:"Terminate as 'failed' with this code and message instead of routing — a defect. Nothing can catch a panic; the code exists to classify the failure, not to branch on it. Mutually exclusive with goto and raise."`
	NotReached *bool    `json:"not_reached,omitempty" description:"Assert that this error code means the remote call was never reached. When true, retries are allowed even on only_once:true tasks. On an only_once task it must name exact codes rather than a wildcard -- an assertion is about one specific error -- and it cannot be made at all about http.timeout, http.disconnected, external.timeout or only_once.interrupted, since nothing came back from those to interpret. Omit to use the engine's default classification (pre.* = not reached, everything else = potentially reached)."`
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

// switch selects with "case", on_error with "code"; a dropped selector silently becomes
// a catch-all — hence rejection plus the hints below. Strict decoding is safe over stored
// rows: SaveDefinition persists the canonical re-marshal, which carries no unknown fields.
var (
	errorCaseFields  = map[string]bool{"code": true, "case": true, "retry": true, "goto": true, "raise": true, "panic": true, "not_reached": true}
	switchCaseFields = map[string]bool{"case": true, "goto": true, "raise": true, "panic": true}

	// Advice for keys that are valid somewhere else, or used to be valid here. Only
	// reached for a key the rule itself does not accept, so a legitimate use never sees
	// it. `retries` is the pre-policy spelling: it would otherwise be dropped in silence,
	// leaving a rule that still matches and still routes but never retries.
	ruleFieldHints = map[string]string{

		"code":    `a switch case selects with "case"; "code" belongs to on_error`,
		"retries": `renamed to "retry": write "retry": 3, or "retry": {attempts: 3, delay: "30s"} to shape the backoff`,
	}
)

// rejectUnknownFields reports the first key in data that the rule does not define, in
// sorted order so the message is stable. A malformed document is left to the real decode,
// which reports it better.
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
	// `case` is legal here since M2, but a LIST under it is the old mistake wearing the new
	// key: an author reaching for `code` and typing `case`. The generic decode error names
	// only the Go type, so say what was meant instead.
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

// Terminates reports whether the rule ends the process rather than routing onward.
// A rule with none of goto/raise/panic also ends it — by failing — but that is the
// engine's generic failure, not an authored terminal clause.
func (e ErrorCase) Terminates() bool {
	return e.Goto == GotoEnd || e.Raise != nil || e.Panic != nil
}
