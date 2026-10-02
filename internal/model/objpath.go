package model

// Extract removes every *ObjectRef from v and reports each one's path from v; Place inverts it.
// Three callers (the API's objects section, the DB's external-task refs, a client putting values
// back) must agree exactly on paths. specs/object-store.md.
func Extract(v any, at []any, out *[]*ObjectRef) any {
	switch t := v.(type) {
	case *ObjectRef:
		*out = append(*out, &ObjectRef{Ref: t.Ref, Size: t.Size, Path: append([]any{}, at...)})
		return nil
	case map[string]any:
		for k, val := range t {
			if ref, isRef := val.(*ObjectRef); isRef {
				*out = append(*out, &ObjectRef{Ref: ref.Ref, Size: ref.Size, Path: childPath(at, k)})
				delete(t, k)
				continue
			}
			t[k] = Extract(val, childPath(at, k), out)
		}
		return t
	case []any:
		for i, val := range t {
			if ref, isRef := val.(*ObjectRef); isRef {
				*out = append(*out, &ObjectRef{Ref: ref.Ref, Size: ref.Size, Path: childPath(at, i)})
				t[i] = nil
				continue
			}
			t[i] = Extract(val, childPath(at, i), out)
		}
		return t
	}
	return v
}

// Place reports whether value landed at path. A missing step is a miss, never created: the path
// came from this structure, so a gap means data and refs disagree.
func Place(root any, path []any, value any) bool {
	if len(path) == 0 {
		return false
	}
	cur := root
	for i, seg := range path {
		last := i == len(path)-1
		switch node := cur.(type) {
		case map[string]any:
			key, ok := seg.(string)
			if !ok {
				return false
			}
			if last {
				node[key] = value
				return true
			}
			cur = node[key]
		case []any:
			idx, ok := index(seg)
			if !ok || idx < 0 || idx >= len(node) {
				return false
			}
			if last {
				node[idx] = value
				return true
			}
			cur = node[idx]
		default:
			return false
		}
	}
	return false
}

// index accepts every numeric shape a decoded path can arrive in: an int from Go, a float64 from
// encoding/json, a json.Number from the numeric decoder.
func index(seg any) (int, bool) {
	switch n := seg.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	if s, ok := seg.(interface{ Int64() (int64, error) }); ok {
		if v, err := s.Int64(); err == nil {
			return int(v), true
		}
	}
	return 0, false
}

// childPath copies rather than appending in place: append can share a backing array between
// siblings, so two refs would name the same location and one would overwrite the other.
func childPath(at []any, key any) []any {
	out := make([]any, len(at)+1)
	copy(out, at)
	out[len(at)] = key
	return out
}
