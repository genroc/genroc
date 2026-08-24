package model

// Extract removes every *ObjectRef from v and reports where each one was, as a path of keys
// rooted at v. Place is its inverse.
//
// The pair is here rather than in one of its callers because all three need the same answer and
// must agree exactly: the API builds a response's objects section with it, the DB stores an
// external task's refs beside its input with it, and a client (genctl, a worker) puts the values
// back with it. Three implementations of one traversal is three chances to disagree about where
// a value belongs. specs/object-store.md.
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

// Place writes value at path inside root and reports whether it landed. A step that does not
// exist is a miss rather than something to create: the path came from the same structure, so a
// gap means the data and its refs disagree, and inventing the shape would hide that.
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
