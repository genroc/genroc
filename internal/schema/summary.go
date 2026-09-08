package schema

// One-line renderings of a type, for a reader rather than a validator. `TypeName` (navigate.go)
// is the smallest of these; Summary is the one an editor and `genctl schema` both print.

import (
	"maps"
	"slices"
	"strings"
)

// Summary names a type in one line — its kind, an object's members, an array's element — which
// is enough to pick the address whose document you want, or to read a hover without opening a
// second view. A `$ref` is followed: the name of a definition says less than what it holds.
func (s Schema) Summary() string {
	if resolved, err := s.Resolve(); err == nil {
		s = resolved
	}
	if members := s.MemberNames(); members != "" {
		return "object{" + members + "}"
	}
	if items := s.Items(); !items.IsZero() {
		return "array<" + items.Summary() + ">"
	}
	return s.TypeName()
}

// MemberNames lists an object's properties, `?` on the optional ones. Empty for anything that
// is not an object with declared properties.
func (s Schema) MemberNames() string {
	props := s.Properties()
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	for _, name := range s.Required() {
		required[name] = true
	}
	names := make([]string, 0, len(props))
	for _, name := range slices.Sorted(maps.Keys(props)) {
		label := name
		if !required[name] {
			label += "?"
		}
		// `=null` is what one arm says about an output another arm sets: present, and null
		// here. It is the correlation the arms exist to carry, so it has to be visible.
		if props[name].IsNull() {
			label += "=null"
		}
		names = append(names, label)
	}
	return strings.Join(names, ", ")
}
