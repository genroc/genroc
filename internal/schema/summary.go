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
func (s Schema) Summary() string { return s.summary(0) }

// summaryDepth bounds the walk: a recursive type ($ref through items or a nullable arm) would
// otherwise describe itself forever. specs/recursive-type-inference.md.
const summaryDepth = 6

func (s Schema) summary(depth int) string {
	if depth > summaryDepth {
		return s.TypeName()
	}
	if resolved, err := s.Resolve(); err == nil {
		s = resolved
	}
	// A nullable value describes what it holds, then says it may be absent. Without this the
	// null arm blocks the $ref beside it from resolving and the whole thing reads `unknown` —
	// which is what `self.previous` on a looping task said.
	if s.HasNull() {
		if inner := s.StripNull(); !inner.IsZero() && !inner.IsNull() {
			return inner.summary(depth+1) + "|null"
		}
	}
	if members := s.MemberNames(); members != "" {
		return "object{" + members + "}"
	}
	if items := s.Items(); !items.IsZero() {
		return "array<" + items.summary(depth+1) + ">"
	}
	return s.TypeName()
}

// MemberNames lists an object's properties, `?` on the ones that may be absent. Empty for
// anything that is not an object with declared properties.
func (s Schema) MemberNames() string {
	if resolved, err := s.Resolve(); err == nil {
		s = resolved
	}
	props := s.Properties()
	if len(props) == 0 {
		return ""
	}
	names := make([]string, 0, len(props))
	for _, name := range slices.Sorted(maps.Keys(props)) {
		label := name
		if s.MayBeAbsent(name) {
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

// MayBeAbsent reports whether reading a property may find nothing — which is NOT the same as
// "not required": conforming fills an absent optional's default, so a defaulted property is
// always there. Navigation types it non-nullable for exactly this reason, and a summary that
// said otherwise contradicted the type beside it.
func (s Schema) MayBeAbsent(name string) bool {
	if resolved, err := s.Resolve(); err == nil {
		s = resolved
	}
	for _, r := range s.Required() {
		if r == name {
			return false
		}
	}
	prop, ok := s.Properties()[name]
	if !ok {
		return true
	}
	return prop.Default() == nil
}
