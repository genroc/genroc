package defdoc

import (
	"regexp"
	"strings"
)

// directiveRe cannot match `$$` (the second character must be a letter), leaving the escape to the
// template layer. The SPACE after the colon is required: it alone separates a directive from a
// `goto: $a:b` routing target. specs/source-resolution.md §Directive syntax.
var directiveRe = regexp.MustCompile(`^\s*\$([a-zA-Z][a-zA-Z0-9_-]*):[ \t]+(\S.*?)\s*$`)

// Directive splits a source-resolution directive into its resolver name and verbatim argument.
// It lives here because defdoc must recognise a `<<` directive before genctl sees it; whether a
// name is REGISTERED is genctl's question alone.
func Directive(s string) (name, argument string, ok bool) {
	m := directiveRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// UnescapeDirective drops the doubling from a leaf written `$$name: argument`, an escaped literal
// `$name: argument`. It inverts Directive by construction; a second regexp here would drift.
func UnescapeDirective(s string) (string, bool) {
	i := strings.IndexByte(s, '$')
	if i < 0 || i+1 >= len(s) || s[i+1] != '$' || strings.TrimSpace(s[:i]) != "" {
		return s, false
	}
	un := s[:i] + s[i+1:]
	if _, _, ok := Directive(un); !ok {
		return s, false
	}
	return un, true
}

// EscapeDirective is UnescapeDirective's inverse: a leaf that reads as a directive gets its `$`
// doubled, so text copied out of an unescaped document stays text.
func EscapeDirective(s string) (string, bool) {
	if _, _, ok := Directive(s); !ok {
		return s, false
	}
	i := strings.IndexByte(s, '$')
	return s[:i] + "$" + s[i:], true
}
