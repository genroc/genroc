package engine

// The runtime half of a declared slot schema, conformed as an ASSERTION: a failure is a defect
// in genroc's type checking, so its code is terminal. CLAUDE.md; specs/declared-slot-schemas.md §4.

import (
	"errors"
	"fmt"

	"genroc/internal/errcode"
	"genroc/internal/schema"
)

// errDeclaredViolation is not an expression failure: the expression evaluated fine, and its
// value contradicts the type published for the slot.
var errDeclaredViolation = errors.New("the value does not satisfy the schema declared for this slot")

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

// declaredFailureCode keeps two facts an operator reads apart. A request payload folds into
// engine.input rather than a code of its own (specs/declared-slot-schemas.md §4).
func declaredFailureCode(err error, violation, ordinary errcode.Code) errcode.Code {
	if errors.Is(err, errDeclaredViolation) {
		return violation
	}
	return ordinary
}
