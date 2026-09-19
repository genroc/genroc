package defdoc

import (
	"regexp"
	"strings"
)

// directiveRe matches a whole leaf of the form `$name: path`. A leaf beginning `$$` cannot
// match -- the second character must be a letter -- which is what leaves the escape to the
// template layer instead of unescaping it twice (specs/typed-values.md).
//
// The SPACE after the colon is required, and it is the only thing separating a directive from a
// routing target: `$` is the sigil for both, so `goto: $a:b` naming a task called `a:b` read as
// a directive named `a` -- and ran, where a resolver happened to carry that name. YAML draws the
// same line, since a mapping needs the space and a plain scalar like `a:b` does not, so this
// follows the host grammar rather than inventing a looser rule. It also stops `$scheme://host`.
var directiveRe = regexp.MustCompile(`^\s*\$([a-zA-Z][a-zA-Z0-9_-]*):[ \t]+(\S.*?)\s*$`)

// Directive splits a source-resolution directive into its resolver name and verbatim argument.
//
// It lives here rather than in genctl, which owns resolution, because defdoc parses a document
// BEFORE genctl sees it and has to recognise a `<<` directive to leave it alone. One shape, one
// definition: two would drift, and the day they disagreed a `<<` would be merged by one and
// resolved by the other. Whether a name is REGISTERED is still genctl's question alone.
func Directive(s string) (name, argument string, ok bool) {
	m := directiveRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// UnescapeDirective drops the doubling from a leaf written `$$name: argument`, which is how an
// author writes a literal `$name: argument` where Directive would otherwise claim the leaf.
//
// It is the exact inverse of Directive BY CONSTRUCTION rather than by a second pattern: drop one
// `$`, and the leaf was escaped iff what remains is a directive. A second regexp here is the
// drift this file exists to prevent.
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
