package api

import (
	"genroc/internal/model"
)

// ObjectEntry is one value a response could not inline. A section sits on the object that OWNS
// its values, so no path carries a list position. specs/object-store.md §The wire.
type ObjectEntry struct {
	// Path is an ARRAY, not a JSON Pointer: no RFC 6901 unescaping, and no "0" ambiguous
	// between a key and an index.
	Path []any  `json:"path"`
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// extractObjects REMOVES each value rather than leaving a marker, which user output with `ref`
// and `size` keys would mimic; a gap is the better failure than plausible data.
func extractObjects(v any, at []any, out *[]ObjectEntry) any {
	var refs []*model.ObjectRef
	res := model.Extract(v, at, &refs)
	for _, r := range refs {
		*out = append(*out, ObjectEntry{Path: r.Path, Ref: r.Ref, Size: r.Size})
	}
	return res
}
