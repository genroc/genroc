package api

import (
	"genroc/internal/model"
)

// ObjectEntry is one externalized value a response could not carry inline: where it belongs, and
// the handle to fetch it with. A section sits on whatever object OWNS the values it names, with
// paths rooted there -- anywhere else a path into a list would carry a position, valid only for
// one unmodified page. specs/object-store.md §The wire.
type ObjectEntry struct {
	// Path is an ARRAY of keys, not a JSON Pointer string: a pointer would make every recipient
	// implement RFC 6901 unescaping before it could walk anywhere, and its indices are decimal
	// strings, so "0" is ambiguous between the key "0" and element zero.
	Path []any  `json:"path"`
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// extractObjects removes every externalized marker from v and lists where each one was. The
// value is REMOVED, not replaced by a marker: a marker is indistinguishable from a process whose
// output legitimately has `ref` and `size` keys. Absence is ambiguous with "nothing here", which
// is the better failure -- a client ignoring the section sees a gap, not plausible data.
func extractObjects(v any, at []any, out *[]ObjectEntry) any {
	var refs []*model.ObjectRef
	res := model.Extract(v, at, &refs)
	for _, r := range refs {
		*out = append(*out, ObjectEntry{Path: r.Path, Ref: r.Ref, Size: r.Size})
	}
	return res
}
