package defdoc

import "regexp"

// directiveRe matches a whole leaf of the form `$name: path`. A leaf beginning `$$` cannot
// match -- the second character must be a letter -- which is what leaves the escape to the
// template layer instead of unescaping it twice (specs/typed-values.md).
var directiveRe = regexp.MustCompile(`^\s*\$([a-zA-Z][a-zA-Z0-9_-]*):\s*(\S.*?)\s*$`)

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
