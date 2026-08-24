package api

import (
	"genroc/internal/model"
)

// ObjectEntry is one externalized value a response could not carry inline: where it belongs, and
// the handle to fetch it with.
//
// A section sits on whatever object OWNS the values it names, and its paths are rooted there: on
// an instance detail that is the response body (["context", "outputs", "x"]), on a list it is
// each entry (["data"]). Rooted anywhere else, a path into a list would carry a position, and a
// position is valid only for one unmodified page -- a client that accumulates pages or reverses
// rows has already invalidated it. specs/object-store.md §The wire.
type ObjectEntry struct {
	// Path is an ARRAY of keys, not a JSON Pointer string: a pointer would make every recipient
	// implement RFC 6901 unescaping before it could walk anywhere, and its indices are decimal
	// strings, so "0" is ambiguous between the key "0" and element zero.
	Path []any  `json:"path"`
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// extractObjects removes every externalized marker from v and lists where each one was.
//
// The value is REMOVED, not replaced by a marker or a null. A marker would be an in-band
// sentinel indistinguishable from a process whose output legitimately has `ref` and `size` keys
// -- which is what model.Envelope refuses on disk and the API had reintroduced on the wire.
// Absence is ambiguous with "there was nothing here", and that is the better failure: a client
// ignoring the section sees a missing value rather than a plausible object it will treat as data.
func extractObjects(v any, at []any, out *[]ObjectEntry) any {
	var refs []*model.ObjectRef
	res := model.Extract(v, at, &refs)
	for _, r := range refs {
		*out = append(*out, ObjectEntry{Path: r.Path, Ref: r.Ref, Size: r.Size})
	}
	return res
}
