package engine

// The runtime half of a declared slot schema: the value is conformed to the declaration
// before it leaves the slot. specs/declared-slot-schemas.md §4.
//
// It is an ASSERTION, not a check. Every value it sees was computed from values already
// conformed at their own boundaries, by expressions registration type-checked against this
// very schema — so it cannot fail unless genroc's type system is wrong. Its failure is
// therefore a TERMINAL engine code and never routable: a rule that could catch it would be
// written by an author, the instance would carry on, and the bug it exists to report would
// never be seen. `engine.input` was already this assertion for a child's input.
//
// What it DOES do on the ordinary path is the repair the author should not have to write: an
// optional non-nullable property fed a null has its key removed, because absence is valid
// there and there is no filter builtin to do it by hand.

import (
	"errors"
	"fmt"

	"genroc/internal/errcode"
	"genroc/internal/schema"
)

// errDeclaredViolation marks a conform failure so a caller can fail the instance with the
// terminal code the assertion wants, rather than with the expression one. They are different
// facts: the expression evaluated fine, and what it produced contradicts the type published
// for that slot.
var errDeclaredViolation = errors.New("the value does not satisfy the schema declared for this slot")

// conformDeclared applies a declared slot schema to the value leaving that slot. A nil schema
// passes the value through, which is every slot that declares nothing.
func conformDeclared(v any, declared *schema.Schema, what string) (any, error) {
	if declared == nil {
		return v, nil
	}
	out, err := declared.Validate(v, schema.ConformToSchemaExactly)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %v — this is a defect in genroc's type checking, "+
			"which should have refused the definition at registration", what, errDeclaredViolation, err)
	}
	return out, nil
}

// declaredFailureCode picks the terminal code for a failure at a slot that may carry a
// declaration: `violation` where the conform rejected the value, `ordinary` where the
// expression itself failed. The two are different facts and an operator reads the difference.
//
// A request payload (a fetch body, a query map) folds into `engine.input` rather than earning
// a code of its own: it is what we send, the name already covers a child's input, and a third
// spelling for something that cannot happen is worse than a slightly wide one.
func declaredFailureCode(err error, violation, ordinary errcode.Code) errcode.Code {
	if errors.Is(err, errDeclaredViolation) {
		return violation
	}
	return ordinary
}
