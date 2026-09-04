package schema

import "strconv"

// One spelling of an access path shared by error messages, shape labels and guard keys —
// the expression language's own accessor syntax, so authors can paste it back. Dots alone
// name the wrong thing (headers.retry-after is a subtraction; a.a.b ambiguates a["a.b"]);
// bracket-quoting keeps the rendering injective, which is what lets guards key off it.

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

// JoinPath renders a non-identifier key in bracket form, so an error names the accessor
// the author would write — headers["retry-after"], not the unparseable headers.retry-after.
func JoinPath(path, name string) string {
	if identifierKey(name) {
		if path == "" {
			return name
		}
		return path + "." + name
	}
	return path + "[" + strconv.Quote(name) + "]"
}

func JoinIndex(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}

// renderPath renders navigation steps as an access path. A computed key renders
// as the accessor it came from, `a[k]` — still valid expression syntax, though not
// something parsePath reads back: parsePath handles static paths only, and a
// computed step never arises from one.
func renderPath(steps []pathStep) string {
	path := ""
	for _, st := range steps {
		switch st.kind {
		case stepIndex:
			path = JoinIndex(path, st.index)
		case stepKey:
			path += "[" + renderPath(st.key) + "]"
		default:
			path = JoinPath(path, st.prop)
		}
	}
	return path
}

// Segment is one step of a static path: a property name, or an array index when IsIndex.
type Segment struct {
	Name    string
	Index   int
	IsIndex bool
}

// ParsePath reads back the syntax JoinPath and JoinIndex emit, for a caller addressing
// something that is NOT a schema — a slot address is this same path grammar over a definition
// (specs/schema-command.md). One parser for one syntax: a second reader of it is a drift that
// shows up the day the two disagree.
func ParsePath(path string) ([]Segment, error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	out := make([]Segment, len(steps))
	for i, st := range steps {
		// A bracket holds a quoted key or an integer, so no computed step can arrive here.
		if st.kind == stepIndex {
			out[i] = Segment{Index: st.index, IsIndex: true}
			continue
		}
		out[i] = Segment{Name: st.prop}
	}
	return out, nil
}
