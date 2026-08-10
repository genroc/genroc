package schema

// Reporting is a MODE on the subset relation, never a second walk beside it — the same rule
// as ConformMode in validate.go, learned the same way. CLAUDE.md has the argument.

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"genroc/internal/numeric"
)

// SubsetBreakKind names why the relation failed, so a caller can word it. A kind rather than
// a finished sentence: a check running new ⊆ old must read from the reader's point of view.
type SubsetBreakKind string

const (
	BreakMissingRequired SubsetBreakKind = "missing_required"
	BreakType            SubsetBreakKind = "type"
	BreakConstraint      SubsetBreakKind = "constraint"
	BreakVariants        SubsetBreakKind = "variants"
	BreakUnknown         SubsetBreakKind = "unknown"
)

// SubsetBreak is the first place one schema fails to fit another. Sub and Super each say
// what their side holds, direction-neutrally, so a caller may print them in either order.
type SubsetBreak struct {
	Path  string
	Kind  SubsetBreakKind
	Sub   string
	Super string
}

// pathLink locates a node inside the compared schemas as a parent chain, so descending
// costs a pointer rather than a string join. It is only ever built while tracing.
type pathLink struct {
	parent *pathLink
	step   string // a property name, or "[]" for array elements
}

func (p *pathLink) String() string {
	if p == nil {
		return ""
	}
	prefix := p.parent.String()
	switch {
	case p.step == "[]":
		return prefix + "[]"
	case prefix == "":
		return p.step
	default:
		return prefix + "." + p.step
	}
}

// subsetTrace collects EVERY break, not the first: two properties of one object fail
// independently. Order is the walk's, so it stays the shape of the schema.
type subsetTrace struct{ breaks []*SubsetBreak }

// at extends the location, but only while tracing — on the bool path nothing is built, so
// the relation stays allocation-free where it runs per validation.
func (ctx *subsetCtx) at(p *pathLink, step string) *pathLink {
	if ctx.trace == nil {
		return nil
	}
	return &pathLink{parent: p, step: step}
}

// no records a LEAF failure and returns false. Sites that merely propagate a nested false
// must not call it — the break belongs where it was found, at its own path.
func (ctx *subsetCtx) no(at *pathLink, kind SubsetBreakKind, sub, super string) bool {
	if ctx.trace != nil {
		ctx.trace.breaks = append(ctx.trace.breaks,
			&SubsetBreak{Path: at.String(), Kind: kind, Sub: sub, Super: super})
	}
	return false
}

// mark/rollback bracket an ALTERNATIVE — a union arm or an allOf member that may fail
// while the whole still holds. Without the rollback a losing arm's break would be reported
// as the reason for a relation that actually succeeded, or for a later, unrelated failure.
func (ctx *subsetCtx) mark() int {
	if ctx.trace == nil {
		return 0
	}
	return len(ctx.trace.breaks)
}

func (ctx *subsetCtx) rollback(mark int) {
	if ctx.trace != nil {
		ctx.trace.breaks = ctx.trace.breaks[:mark]
	}
}

// enumHasNumeric is the fallback behind the literal-bytes lookup: a number's enum membership
// is by VALUE, the way validate's enumContains decides it. A relation that disagreed would
// call a reformatted schema a breaking change.
func enumHasNumeric(pool []any, v any) bool {
	if _, isNum := numeric.ToDecimal(v); !isNum {
		return false
	}
	for _, p := range pool {
		if numeric.Equal(p, v) {
			return true
		}
	}
	return false
}

func typeNames(n *node) string {
	if len(n.Type) == 0 {
		return "untyped"
	}
	return strings.Join([]string(n.Type), "|")
}

// maxPropsRendered caps describeNode for the same reason maxEnumRendered caps an enum: the
// break becomes one line.
const maxPropsRendered = 3

// describeNode names a node well enough to tell two variants of a union apart: `typeNames`
// renders every object as `object`, a word every alternative shares.
func describeNode(n *node) string {
	name := typeNames(n)
	if len(n.Properties) == 0 {
		return name
	}
	shown := slices.Sorted(maps.Keys(n.Properties))
	rest := 0
	if len(shown) > maxPropsRendered {
		rest, shown = len(shown)-maxPropsRendered, shown[:maxPropsRendered]
	}
	out := name + " {" + strings.Join(shown, ", ")
	if rest > 0 {
		out += ", +" + strconv.Itoa(rest) + " more"
	}
	return out + "}"
}

// variantList describes the pool sub failed to fit. Naming what IS accepted is the whole
// difference between a break an operator can act on and a count of alternatives.
func variantList(variants []*node) string {
	shown := variants
	rest := 0
	if len(shown) > maxPropsRendered {
		rest, shown = len(shown)-maxPropsRendered, shown[:maxPropsRendered]
	}
	parts := make([]string, 0, len(shown))
	for _, v := range shown {
		if v == nil {
			parts = append(parts, "a null variant")
			continue
		}
		parts = append(parts, describeNode(v))
	}
	out := "any of [" + strings.Join(parts, ", ")
	if rest > 0 {
		out += ", +" + strconv.Itoa(rest) + " more"
	}
	return out + "]"
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// maxEnumRendered caps enumValues. An enum has no size limit and a break becomes ONE line
// of a report, so a large pool rendered whole buries the constraint it is there to name.
const maxEnumRendered = 5

func enumValues(vs []any) string {
	if len(vs) == 0 {
		return "no enum"
	}
	return "enum " + enumList(vs)
}

func enumList(vs []any) string {
	shown := vs
	if len(shown) > maxEnumRendered {
		shown = shown[:maxEnumRendered]
	}
	parts := make([]string, 0, len(shown))
	for _, v := range shown {
		parts = append(parts, jsonKey(v))
	}
	out := "[" + strings.Join(parts, ", ")
	if rest := len(vs) - len(shown); rest > 0 {
		out += ", +" + strconv.Itoa(rest) + " more"
	}
	return out + "]"
}
