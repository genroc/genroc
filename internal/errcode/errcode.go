// Package errcode is the single source of truth for engine-produced error codes, and imports
// nothing from genroc. Authored raise/panic codes are not here: they are lower_snake_case and
// never contain a dot. See specs/child-error-handling.md.
package errcode

import (
	"fmt"
	"strings"
)

// Code is an engine code or an author's raise/panic code. A defined type, so an explicit
// conversion marks exactly where a plain string becomes one.
type Code string

// Call codes, catchable by on_error. Their prose lives only in `catchable` (catchable.go).
const (
	HTTPTimeout      Code = "http.timeout"
	HTTPDisconnected Code = "http.disconnected"
	PreTimeout       Code = "pre.timeout"
	PreError         Code = "pre.error"
	ResultParse      Code = "result.parse"
	ResultTooLarge   Code = "result.too_large"
	ResultInvalid    Code = "result.invalid"
	ExternalTimeout  Code = "external.timeout"
	ExternalLost     Code = "external.lost"
)

// HTTP formats the code for a rejected HTTP status: HTTP(500) == "http.500". The status is
// unbounded, so this family is a function rather than a constant — the only dynamic code.
func HTTP(status int) Code { return Code(fmt.Sprintf("http.%d", status)) }

// The one catchable engine code, named after the only declaration that can produce it.
// specs/only-once-interrupted.md.
const (
	// OnlyOnceInterrupted: the engine will not re-run an interrupted only_once task. Catchable
	// because whether the call took effect is often knowable to the definition.
	OnlyOnceInterrupted Code = "only_once.interrupted"
)

// NotReached prefixes the codes where the request never left: retrying them is safe even
// on an only_once task.
const NotReached = "pre."

// IsNotReached reports whether c is in the pre.* "call never reached the remote" family.
func (c Code) IsNotReached() bool { return strings.HasPrefix(string(c), NotReached) }

// unknowable: the request left and nothing came back, so never retryable on only_once. A
// slice because validation iterates it and the order shows in messages.
var unknowable = []Code{OnlyOnceInterrupted, HTTPTimeout, HTTPDisconnected, ExternalTimeout, ExternalLost}

// Unknowable returns the codes whose outcome cannot be determined either way. The
// returned slice must not be modified.
func Unknowable() []Code { return unknowable }

// IsUnknowable is the mirror of IsNotReached: "cannot be known either way" versus
// "definitely did not happen".
func (c Code) IsUnknowable() bool {
	for _, u := range unknowable {
		if c == u {
			return true
		}
	}
	return false
}

// MatchCode reports whether the error code s matches the pattern p. '%' is the only wildcard;
// every other character is literal. Deliberately NOT full SQL LIKE, whose '_' wildcard is a
// footgun for codes that contain underscores.
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

// Engine-internal codes are TERMINAL: never routed through on_error. Every terminal failure
// carries one so error_code is uniformly queryable.
const (
	EngineDefinition Code = "engine.definition" // definition unusable: missing, or names a task/goto not in it
	EngineExpression Code = "engine.expression" // an expression could not be evaluated against this context
	EngineConfig     Code = "engine.config"     // config could not be resolved from the environment
	EngineInput      Code = "engine.input"      // an input did not satisfy the schema declared for it
	EngineOutput     Code = "engine.output"     // an output did not satisfy the schema declared for it
	EngineSpawn      Code = "engine.spawn"      // spawning children, or arming an external task / reading its answer, failed
	EngineCollect    Code = "engine.collect"    // collecting a settled batch's outputs failed
	EnginePanic      Code = "engine.panic"      // a Go panic escaped this instance's advance (see engine.dispatch)
)
