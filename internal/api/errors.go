package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

// Code classifies every error reply and rides in its body; the HTTP status is rendered from it.
// Small on purpose: distinctions a client can act on. Engine detail is errcode's.
type Code string

const (
	// CodeInvalid — the request is malformed or unacceptable. Retrying it unchanged
	// will never succeed.
	CodeInvalid Code = "invalid"
	// CodeNotFound — the definition, instance, channel or task named does not exist.
	CodeNotFound Code = "not_found"
	// CodeConflict — the request is well-formed and the target exists, but its current
	// state forbids the operation. The identical request may succeed later.
	CodeConflict Code = "conflict"
	// CodeUnsupported — the endpoint exists but this server is not configured to serve
	// it (e.g. /tick outside manual-tick mode).
	CodeUnsupported Code = "unsupported"
	// CodeUnavailable — the database is unreachable. A statement about the whole worker,
	// so a readiness probe routes elsewhere.
	CodeUnavailable Code = "unavailable"
	// CodeUnauthenticated — no identity was established. Kept apart from CodeForbidden: the
	// two commonest auth failures have opposite fixes.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeForbidden — the caller is known and lacks the permission this action needs.
	CodeForbidden Code = "forbidden"
	// CodeInternal — anything unclassified, on purpose: a path still answering 500 is one
	// nobody has classified yet.
	CodeInternal Code = "internal"
)

// statusByCode renders a Code as an HTTP status. Every Code must appear here;
// statusOf falls back to 500, which is also what an empty Code gets.
var statusByCode = map[Code]int{
	CodeInvalid:         http.StatusBadRequest,
	CodeNotFound:        http.StatusNotFound,
	CodeConflict:        http.StatusConflict,
	CodeUnsupported:     http.StatusNotImplemented,
	CodeUnavailable:     http.StatusServiceUnavailable,
	CodeUnauthenticated: http.StatusUnauthorized,
	CodeForbidden:       http.StatusForbidden,
	CodeInternal:        http.StatusInternalServerError,
}

// statusOfOutcome is statusOf's success-side twin. Unchanged is 204, so its reply carries no
// body (CLAUDE.md). specs/id-list-commands.md.
func statusOfOutcome(o model.Outcome) int {
	switch o {
	case model.OutcomeAccepted:
		return http.StatusAccepted
	case model.OutcomeUnchanged:
		return http.StatusNoContent
	default:
		return http.StatusOK
	}
}

func statusOf(c Code) int {
	if s, ok := statusByCode[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Enum publishes the codes to the OpenAPI generator (swaggest), derived from statusByCode
// so none goes undocumented.
func (Code) Enum() []interface{} {
	out := make([]interface{}, 0, len(statusByCode))
	for _, c := range ReferenceCodes() {
		out = append(out, c.Code)
	}
	return out
}

// errorStatuses returns the HTTP statuses to document for an action, given the extra
// codes it declares. Sorted so the generated spec is stable across builds.
func errorStatuses(extra []Code) []int {
	seen := map[int]bool{http.StatusBadRequest: true, http.StatusInternalServerError: true}
	for _, c := range extra {
		seen[statusOf(c)] = true
	}
	out := make([]int, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Ints(out)
	return out
}

// Error carries an API classification. db failures classify automatically in codeOf, so a
// handler that only forwards a db error still gets the right status.
type Error struct {
	Code    Code
	Message string
	Err     error // wrapped cause, if any
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// apiErrf keeps %w walkable. The explicit Code wins in codeOf, checked before the sentinels,
// so a handler can override a db classification.
func apiErrf(code Code, format string, a ...any) *Error {
	err := fmt.Errorf(format, a...)
	return &Error{Code: code, Message: err.Error(), Err: err}
}

func invalid(format string, a ...any) *Error     { return apiErrf(CodeInvalid, format, a...) }
func notFound(format string, a ...any) *Error    { return apiErrf(CodeNotFound, format, a...) }
func conflict(format string, a ...any) *Error    { return apiErrf(CodeConflict, format, a...) }
func unsupported(format string, a ...any) *Error { return apiErrf(CodeUnsupported, format, a...) }
func forbidden(format string, a ...any) *Error   { return apiErrf(CodeForbidden, format, a...) }
func unavailable(format string, a ...any) *Error { return apiErrf(CodeUnavailable, format, a...) }

// codeOf precedence: explicit *Error, then a db sentinel, then a validation failure, else
// internal.
func codeOf(err error) Code {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	var ve *model.ValidationError
	switch {
	case errors.Is(err, db.ErrNotFound):
		return CodeNotFound
	case errors.Is(err, db.ErrConflict):
		return CodeConflict
	case errors.Is(err, db.ErrInvalid):
		return CodeInvalid
	case errors.As(err, &ve):
		return CodeInvalid
	}
	return CodeInternal
}

// fieldsOf looks through wrapping so a handler's "%s: %w" prefix does not lose the detail;
// inference reports the same way. specs/language-server.md §2.
func fieldsOf(err error) []model.FieldError {
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		return ve.Fields
	}
	var ds validation.Diagnostics
	if errors.As(err, &ds) {
		out := make([]model.FieldError, len(ds))
		for i, d := range ds {
			// Field is the LOCATION: a client uses it to point at something, and the scope it
			// was written in is the prefix, recoverable by walking up.
			out[i] = model.FieldError{Field: d.Location, Rule: string(d.Code), Message: d.Message}
		}
		return out
	}
	return nil
}
