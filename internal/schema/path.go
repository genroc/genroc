package schema

import "strconv"

// One spelling of an access path, shared by everything that names a position in a
// value: validation error messages, shape inference labels, and the narrowing-guard
// keys in inference. It is the expression language's own accessor syntax, so a path
// in an error message is something the author can paste straight back into a
// definition.
//
// The reason it cannot just be dots: a property key is an arbitrary JSON string,
// while dot access reaches only identifiers. `headers.retry-after` is a subtraction,
// and `headers.x.y` is indistinguishable from the nested x → y — so a dot-joined
// path is not merely ugly for such keys, it names the wrong thing. Keys that need
// it take the bracket-quoted form: headers["retry-after"], headers["x.y"].
//
// That also makes the rendering injective, which is what lets the guard map key off
// it: an identifier can contain none of . [ " so the two forms never collide.

// identifierKey reports whether name can be spelled with dot access. Deliberately
// narrower than the lexer's identifier rule — bracket-quoting a key that did not
// strictly need it is still correct, whereas dotting one that needed brackets is not.
func identifierKey(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// JoinPath extends a rendered path with a property key.
func JoinPath(path, name string) string {
	if identifierKey(name) {
		if path == "" {
			return name
		}
		return path + "." + name
	}
	return path + "[" + strconv.Quote(name) + "]"
}

// JoinIndex extends a rendered path with an array index.
func JoinIndex(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}

// renderPath renders navigation steps as an access path.
func renderPath(steps []pathStep) string {
	path := ""
	for _, st := range steps {
		if st.isIndex {
			path = JoinIndex(path, st.index)
			continue
		}
		path = JoinPath(path, st.prop)
	}
	return path
}
