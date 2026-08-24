package api

import (
	"genroc/internal/model"
)

// ObjectEntry is one externalized value a response could not carry inline: where it belongs, and
// the handle to fetch it with. specs/object-store.md §The wire.
type ObjectEntry struct {
	// Path is where the value goes, as an ARRAY of keys rooted at the response body — object
	// keys as strings, array indices as numbers. Not a JSON Pointer: that would make every
	// recipient implement RFC 6901 unescaping before it could walk anywhere, and its indices
	// are decimal strings, so "0" is ambiguous between the key "0" and element zero.
	Path []any  `json:"path"`
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// extractObjects removes every externalized marker from v and reports where each one was.
//
// The value is REMOVED, not replaced by a marker or a null. A marker would be an in-band
// sentinel indistinguishable from a process whose output legitimately has `ref` and `size`
// keys — which is what model.Envelope refuses on disk and the API had reintroduced on the wire.
// Absence is ambiguous with "there was nothing here", and that is the better failure: a client
// ignoring the section sees a missing value rather than a plausible object it will treat as data.
//
// at is the path of v itself, so a caller rooted deeper passes its own prefix.
func extractObjects(v any, at []any, out *[]ObjectEntry) any {
	switch t := v.(type) {
	case *model.ObjectRef:
		// The root itself is a marker. Callers pass a container, so this is defensive rather
		// than reached; it is here so the function is total over what it claims to walk.
		*out = append(*out, ObjectEntry{Path: append([]any{}, at...), Ref: t.Ref, Size: t.Size})
		return nil
	case map[string]any:
		for k, val := range t {
			if ref, isRef := val.(*model.ObjectRef); isRef {
				*out = append(*out, ObjectEntry{Path: childPath(at, k), Ref: ref.Ref, Size: ref.Size})
				delete(t, k)
				continue
			}
			t[k] = extractObjects(val, childPath(at, k), out)
		}
		return t
	case []any:
		for i, val := range t {
			if ref, isRef := val.(*model.ObjectRef); isRef {
				*out = append(*out, ObjectEntry{Path: childPath(at, i), Ref: ref.Ref, Size: ref.Size})
				t[i] = nil
				continue
			}
			t[i] = extractObjects(val, childPath(at, i), out)
		}
		return t
	}
	return v
}

// childPath copies rather than appending in place: append can share backing arrays between
// siblings, so two entries would end up naming the same path.
func childPath(at []any, key any) []any {
	out := make([]any, len(at)+1)
	copy(out, at)
	out[len(at)] = key
	return out
}
