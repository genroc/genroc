package schema

// Where a failure is, beside what it says. A rule's prose names the slot for a reader; a path
// is what a client points at, and guessing one from the other lands on whichever quoted word
// happens to match. specs/language-server.md §2.

import "errors"

type pathError struct {
	path string
	err  error
}

func (e *pathError) Error() string { return e.err.Error() }
func (e *pathError) Unwrap() error { return e.err }

// AtPath tags err with the slot it came from, nesting outward: an inner path is joined beneath
// this one, so `input_schema` wrapping `properties.who` reads `input_schema.properties.who`.
func AtPath(path string, err error) error {
	if err == nil || path == "" {
		return err
	}
	// The wrapped error is kept WHOLE, not unwrapped to the innermost: the levels between add
	// the prose a reader follows, and only the path is recomposed.
	var inner *pathError
	if errors.As(err, &inner) {
		return &pathError{path: path + "." + inner.path, err: err}
	}
	return &pathError{path: path, err: err}
}

// PathOf returns the slot a failure came from, or "" when the rule reported none.
func PathOf(err error) string {
	var p *pathError
	if errors.As(err, &p) {
		return p.path
	}
	return ""
}
