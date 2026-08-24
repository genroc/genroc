// Package errcode is the single source of truth for genroc's engine-produced error codes:
// the machine-readable discriminators stored in an instance's error_code and matched by
// on_error rules. It has no genroc dependencies, so every layer — transport, engine,
// validation — references the same constants without an import cycle.
//
// Authored codes (raise / panic) are deliberately NOT here: those are user-defined,
// lower_snake_case, and forbidden from containing a dot — which is exactly what keeps them
// distinct from the dotted engine codes below. See specs/child-error-handling.md.
package errcode

import (
	"fmt"
	"strings"
)

// Code is the value stored in an instance's error_code: an engine code from this package,
// or an author's raise/panic code. A defined type rather than a bare string so a plain
// string cannot drift into a slot expecting a code — an untyped constant still converts
// implicitly, so an explicit conversion marks exactly where a non-code becomes one.
type Code string

// Call codes — reported by an action's call, and CATCHABLE by on_error on the action task.
const (
	HTTPTimeout      Code = "http.timeout"      // connected, but no response arrived in time
	HTTPDisconnected Code = "http.disconnected" // the request went out, the connection broke before a response
	PreTimeout       Code = "pre.timeout"       // timed out before the request was written — it never left
	PreError         Code = "pre.error"         // failed before the request was written — it never left
	OutputParse      Code = "output.parse"      // the response body was not valid JSON
	OutputTooLarge   Code = "output.too_large"  // the response body exceeded the size a fetch will read
	OutputInvalid    Code = "output.invalid"    // the response did not satisfy its result_schema
	ExternalTimeout  Code = "external.timeout"  // an external task's wait deadline elapsed
	ExternalLost     Code = "external.lost"     // a worker's claim on an external task expired without an answer
)

// HTTP formats the code for a rejected HTTP status: HTTP(500) == "http.500". The status is
// unbounded, so this family is a function rather than a constant — the only dynamic code.
func HTTP(status int) Code { return Code(fmt.Sprintf("http.%d", status)) }

// Catchable engine codes — produced by the engine rather than by a call, but routed
// through on_error like a call code. There is exactly one, and the family is named after
// the declaration that produces it rather than after a subject, because that is the only
// thing that can produce it: see specs/only-once-interrupted.md.
const (
	// OnlyOnceInterrupted means an only_once task's previous attempt was interrupted, so the
	// engine will not re-run it. Catchable — unlike every other engine code — because whether
	// the call took effect is unknown here and often knowable to the definition.
	OnlyOnceInterrupted Code = "only_once.interrupted"
)

// NotReached is the prefix of the codes that mean the remote was never reached (the call
// failed before the request left). A retry of such a code is safe even for an only_once
// task, since nothing happened remotely.
const NotReached = "pre."

// IsNotReached reports whether c is in the pre.* "call never reached the remote" family.
func (c Code) IsNotReached() bool { return strings.HasPrefix(string(c), NotReached) }

// unknowable is NotReached's opposite pole: the request left, nothing came back, so the
// outcome is undeterminable — never retryable on only_once, not_reached does not override.
// Enforced at registration AND runtime (pre-rule definitions never re-validate). A slice:
// validation iterates it and order shows in messages.
var unknowable = []Code{OnlyOnceInterrupted, HTTPTimeout, HTTPDisconnected, ExternalTimeout, ExternalLost}

// Unknowable returns the codes whose outcome cannot be determined either way. The
// returned slice must not be modified.
func Unknowable() []Code { return unknowable }

// IsUnknowable reports whether c is one of the codes where the request left and no
// response came back. The mirror of IsNotReached: "definitely did not happen" versus
// "cannot be known either way".
func (c Code) IsUnknowable() bool {
	for _, u := range unknowable {
		if c == u {
			return true
		}
	}
	return false
}

// MatchCode reports whether the error code s matches the pattern p. '%' is the only
// wildcard; every other character is literal.
//
// Deliberately NOT full SQL LIKE: LIKE's '_' single-char wildcard is a footgun for codes
// that contain underscores, so here `order_%` matches `order_placed` but not
// `order.placed`.
func MatchCode(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			p = p[1:]
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if MatchCode(p, s[i:]) {
					return true
				}
			}
			return false
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

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
	EnginePanic      Code = "engine.panic"      // a Go panic escaped this instance's advance (see engine.dispatch)
)
