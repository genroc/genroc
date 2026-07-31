package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"genroc/internal/db"
	"genroc/internal/model"
)

// Code is the machine-readable classification carried by every error reply.
//
// It lives on Reply rather than being an HTTP concern because all three transports
// share Reply: a TCP or UDS client never sees a status line, so the code is the only
// classification it gets. HTTP renders the same code as a status as well (statusOf).
//
// The set is deliberately small. These are the distinctions a *client* can act on —
// fix the request, look elsewhere, wait and retry, give up — not a taxonomy of what
// went wrong internally. Engine failure detail belongs in errcode, on the instance.
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
	// CodeInternal — anything unclassified. This is the default on purpose: an error
	// nobody classified is a server fault until proven otherwise, which is what makes
	// the classification pass self-driving — any path still answering 500 is a path
	// nobody has looked at.
	CodeInternal Code = "internal"
)

// statusByCode renders a Code as an HTTP status. Every Code must appear here;
// statusOf falls back to 500, which is also what an empty Code gets.
var statusByCode = map[Code]int{
	CodeInvalid:     http.StatusBadRequest,
	CodeNotFound:    http.StatusNotFound,
	CodeConflict:    http.StatusConflict,
	CodeUnsupported: http.StatusNotImplemented,
	CodeInternal:    http.StatusInternalServerError,
}

func statusOf(c Code) int {
	if s, ok := statusByCode[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Enum publishes the code set to the OpenAPI generator (swaggest picks up this
// interface), which is what makes the code a documented part of the contract rather
// than an undocumented debugging aid clients key on anyway.
func (Code) Enum() []interface{} {
	return []interface{}{CodeInvalid, CodeNotFound, CodeConflict, CodeUnsupported, CodeInternal}
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

// Error is an error carrying an API classification. Handlers construct one through
// invalid/notFound/conflict/unsupported when they reject a request themselves;
// failures that come back out of the db package classify automatically in codeOf, so
// a handler that only forwards a db error still gets the right status.
type Error struct {
	Code    Code
	Message string
	Err     error // wrapped cause, if any
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

// apiErrf builds the message with fmt.Errorf and keeps the result as the cause, so a
// %w in format stays walkable: errors.Is/As on the *Error reach straight through to
// whatever was wrapped. The explicit Code still wins in codeOf, which checks *Error
// before the sentinels — that is how a handler overrides a db classification when it
// knows better.
func apiErrf(code Code, format string, a ...any) *Error {
	err := fmt.Errorf(format, a...)
	return &Error{Code: code, Message: err.Error(), Err: err}
}

func invalid(format string, a ...any) *Error     { return apiErrf(CodeInvalid, format, a...) }
func notFound(format string, a ...any) *Error    { return apiErrf(CodeNotFound, format, a...) }
func conflict(format string, a ...any) *Error    { return apiErrf(CodeConflict, format, a...) }
func unsupported(format string, a ...any) *Error { return apiErrf(CodeUnsupported, format, a...) }

// codeOf classifies any error into a Code, in precedence order:
//
//  1. an explicit *Error from a handler,
//  2. a db-layer sentinel, so forwarding a db error needs no per-call-site decision,
//  3. a definition-validation failure, which is always the submitter's fault,
//  4. otherwise internal.
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

// fieldsOf returns the per-field detail of a definition-validation failure, or nil.
// It looks through wrapping, so a handler's "%s: %w" context prefix does not lose it.
func fieldsOf(err error) []model.FieldError {
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		return ve.Fields
	}
	return nil
}
