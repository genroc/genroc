// Package errcode is the single source of truth for genroc's engine-produced error codes:
// the machine-readable discriminators stored in an instance's error_code and matched by
// on_error rules. It has no genroc dependencies, so every layer — transport, engine,
// validation — references the same constants without an import cycle.
//
// Authored codes (raise / panic) are deliberately NOT here: those are user-defined,
// lower_snake_case, and forbidden from containing a dot — which is exactly what keeps them
// distinct from the dotted engine codes below. See docs/child-error-handling.md.
package errcode

import (
	"fmt"
	"strings"
)

// Code is the value stored in an instance's error_code: an engine code from this package,
// or an author's raise/panic code. It is a defined type rather than a bare string so that
// the family test below is a method on the value, and so a plain string cannot drift into
// a slot where a code is expected — an engine failure path that took a `string` accepted
// any message by mistake.
//
// Strings stay correct at the boundaries: the value is persisted to error_code and matched
// against on_error patterns written in YAML. Every existing literal remains valid, since an
// untyped string constant converts implicitly; the conversions that had to be written out
// are exactly the places a non-code string becomes a code.
type Code string

// Call codes — reported by an action's call, and CATCHABLE by on_error on the action task.
const (
	HTTPTimeout     Code = "http.timeout"     // connected, but no response arrived in time
	PreTimeout      Code = "pre.timeout"      // timed out during dial — the request never left
	PreError        Code = "pre.error"        // dial-phase failure — the request never left
	OutputParse     Code = "output.parse"     // the response body was not valid JSON
	OutputInvalid   Code = "output.invalid"   // the response did not satisfy its result_schema
	ExternalTimeout Code = "external.timeout" // an external task's wait deadline elapsed
)

// HTTP formats the code for a rejected HTTP status: HTTP(500) == "http.500". The status is
// unbounded, so this family is a function rather than a constant — the only dynamic code.
func HTTP(status int) Code { return Code(fmt.Sprintf("http.%d", status)) }

// NotReached is the prefix of the codes that mean the remote was never reached (the call
// failed before the request left). A retry of such a code is safe even for an only_once
// task, since nothing happened remotely.
const NotReached = "pre."

// IsNotReached reports whether c is in the pre.* "call never reached the remote" family.
func (c Code) IsNotReached() bool { return strings.HasPrefix(string(c), NotReached) }

// Engine-internal codes — the engine failed the instance itself, not a call. These are
// TERMINAL: they go straight to failInstance and are never routed through on_error, so they
// cannot be caught. Every terminal failure still carries one so error_code is uniformly
// queryable.
const (
	EngineDefinition Code = "engine.definition" // definition unusable: missing, or names a task/goto not in it
	EngineExpression Code = "engine.expression" // an expression could not be evaluated against this context
	EngineConfig     Code = "engine.config"     // config could not be resolved from the environment
	EngineInput      Code = "engine.input"      // a child's input did not satisfy its input_schema
	EngineSpawn      Code = "engine.spawn"      // spawning a batch of children (or arming an external task) failed
	EngineCollect    Code = "engine.collect"    // collecting a settled batch's outputs failed
	EngineOnlyOnce   Code = "engine.only_once"  // an only_once task was interrupted and cannot be safely re-run
	EnginePanic      Code = "engine.panic"      // a Go panic escaped this instance's advance (see engine.dispatch)
)
