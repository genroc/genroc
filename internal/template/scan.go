package template

import (
	"strings"

	"genroc/internal/expression/syntax"
)

// Parse's scanner with the positions kept. Parse throws offsets away — it returns values — but
// anything that has to point AT a marker needs them, and re-deriving where a `$:` starts is a
// second copy of the rule. TestScanAgreesWithParse holds the two together.

// Span is a byte range in the scanned source, half-open.
type Span struct {
	From int
	To   int
}

func (s Span) Empty() bool { return s.From >= s.To }

// Region is one expression written inside a leaf: the marker that introduced it, the body, and
// the `}` that closed it. Close is empty for a `$:` leaf, which runs to the end.
type Region struct {
	Marker Span
	Body   Span
	Close  Span
}

// Scan returns the expression regions in s by byte offset, under Parse's rules: a leading `$:`
// (first non-whitespace content) makes the whole leaf one expression, `$$` escapes, and `${ }`
// interpolates. Unlike Parse it never fails — a buffer being typed in is the normal input —
// so an unterminated `${` runs to the end of s and a body that does not parse is still a body.
func Scan(s string) []Region {
	ws := leadingWS(s)
	if strings.HasPrefix(s[ws:], exprMarker) {
		body := ws + len(exprMarker)
		return []Region{{
			Marker: Span{ws, body},
			Body:   trimSpan(s, Span{body, len(s)}),
		}}
	}

	var out []Region
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 >= len(s) {
			i++
			continue
		}
		switch s[i+1] {
		case '$':
			i += 2 // an escape, not a marker
		case '{':
			body := i + 2
			end := blockEnd(s[body:])
			r := Region{Marker: Span{i, body}, Body: trimSpan(s, Span{body, body + end})}
			if body+end < len(s) {
				r.Close = Span{body + end, body + end + 1}
			}
			out = append(out, r)
			i = body + end + 1
		default:
			i++
		}
	}
	return out
}

// blockEnd is parseBlock's terminator search over an unparsed source: the first `}` whose body
// PARSES, so a `}` inside a nested object or string cannot end the block. Where none parses it
// falls back to the first `}` — parseBlock reports an error there, but a scanner that gave up
// would leave the rest of the line unmarked while it is still being typed.
func blockEnd(s string) int {
	first := -1
	for at := 0; ; {
		end := strings.Index(s[at:], "}")
		if end == -1 {
			if first >= 0 {
				return first
			}
			return len(s)
		}
		end += at
		if first < 0 {
			first = end
		}
		if _, err := syntax.Parse(s[:end]); err == nil {
			return end
		}
		at = end + 1
	}
}

func trimSpan(s string, sp Span) Span {
	for sp.From < sp.To && isSpace(s[sp.From]) {
		sp.From++
	}
	for sp.To > sp.From && isSpace(s[sp.To-1]) {
		sp.To--
	}
	return sp
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
