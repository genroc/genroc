package schema

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// subsetMode carries the rules that differ between the three subset relations. Each is a
// deliberate relaxation with a stated reason, and each is sound only where that reason
// holds — see narrowsTo and absentAsNullSubset.
type subsetMode struct {
	// narrow admits an unknown in sub position — see narrowsTo.
	narrow bool
	// absentAsNull stops requiring the presence of a nullable property — see
	// absentAsNullSubset.
	absentAsNull bool
	// afterConform reads both schemas as descriptions of CONFORMED data rather than as
	// predicates over what may arrive: a property that is required *or* carries a default is
	// guaranteed present, because the conform filled it. See storedSubset.
	afterConform bool
}

// isSubset reports whether every value valid under sub is also valid under super.
// Both schemas must be normalized (flat $defs at root, only #/$defs/<name> refs).
func isSubset(sub, super *node) bool {
	return subsetWith(sub, super, subsetMode{})
}

// isSubset with one rule relaxed: super may require a null-admitting property sub does
// not — the relation for READERS (absence and null navigate identically). Sound ONLY
// where nothing conforms against super; its one caller is the version comparison.
func absentAsNullSubset(sub, super *node) bool {
	return subsetWith(sub, super, subsetMode{absentAsNull: true})
}

// storedSubset compares two schemas as descriptions of data ALREADY conformed. It adds one
// rule to absentAsNullSubset — a property sub declares with a default is guaranteed present —
// and that rule needs no fill behind it, which is why it does not live there. It is asked of
// the sub side only; see guaranteed. Design: specs/compat-command.md §2e.
func storedSubset(sub, super *node) bool {
	return subsetWith(sub, super, subsetMode{absentAsNull: true, afterConform: true})
}

// isSubset with one rule flipped: an unknown ({}) in sub is accepted anywhere. The static
// half of narrowing — sound only paired with a runtime conform, which exists solely at
// child-output collection. Everywhere else {} ⊄ T stands.
func narrowsTo(sub, super *node) bool {
	return subsetWith(sub, super, subsetMode{narrow: true})
}

func subsetWith(sub, super *node, mode subsetMode) bool {
	ok, _ := subsetExplain(sub, super, mode, false)
	return ok
}

// subsetBreaks runs the relation for its diagnostics: every break, in walk order, empty
// when sub fits.
func subsetBreaks(sub, super *node, mode subsetMode) []*SubsetBreak {
	_, breaks := subsetExplain(sub, super, mode, true)
	return breaks
}

// subsetExplain runs the relation, collecting breaks when asked. With explain false nothing
// is allocated for it and the walk stops at the first failure.
func subsetExplain(sub, super *node, mode subsetMode, explain bool) (bool, []*SubsetBreak) {
	var subDefs, superDefs map[string]*node
	if sub != nil {
		subDefs = sub.Defs
	}
	if super != nil {
		superDefs = super.Defs
	}
	ctx := &subsetCtx{
		subDefs:    subDefs,
		superDefs:  superDefs,
		visiting:   make(map[string]bool),
		subsetMode: mode,
	}
	if explain {
		ctx.trace = &subsetTrace{}
	}
	if ctx.check(sub, super, nil) {
		return true, nil
	}
	if ctx.trace == nil {
		return false, nil
	}
	return false, ctx.trace.breaks
}

type subsetCtx struct {
	subDefs   map[string]*node
	superDefs map[string]*node
	visiting  map[string]bool
	subsetMode
	// trace is nil on the bool path, so the relation carries no reporting cost where it
	// runs per validation; the explaining entry point sets it and re-runs.
	trace *subsetTrace
}

func (ctx *subsetCtx) check(sub, super *node, at *pathLink) bool {
	// Cycle detection before any deref.
	subRef := ""
	superRef := ""
	if sub != nil {
		subRef = sub.Ref
	}
	if super != nil {
		superRef = super.Ref
	}
	if subRef != "" || superRef != "" {
		key := subRef + "||" + superRef
		if ctx.visiting[key] {
			return true
		}
		ctx.visiting[key] = true
		defer delete(ctx.visiting, key)
	}

	// Resolve $refs.
	sub = derefSubset(sub, ctx.subDefs)
	super = derefSubset(super, ctx.superDefs)

	// {} accepts anything.
	if isEmptyNode(super) {
		return true
	}
	if isEmptyNode(sub) {
		// An unknown satisfies nothing typed — unless we are checking narrowability,
		// where a runtime conform against super stands behind the claim.
		if ctx.narrow {
			return true
		}
		return ctx.no(at, BreakUnknown, "unknown", typeNames(super))
	}

	// Composition in sub (anyOf / oneOf): every variant must be ⊆ super.
	for _, variants := range [][]*node{sub.AnyOf, sub.OneOf} {
		if variants == nil {
			continue
		}
		for _, v := range variants {
			if v == nil {
				return ctx.no(at, BreakVariants, "a null variant", typeNames(super))
			}
			if !ctx.check(v, super, at) {
				return false
			}
		}
		return true
	}

	// allOf in sub: if any single constraint is ⊆ super then the allOf is too.
	if len(sub.AllOf) > 0 {
		mark := ctx.mark()
		for _, v := range sub.AllOf {
			if v != nil && ctx.check(v, super, at) {
				ctx.rollback(mark)
				return true
			}
		}
		ctx.rollback(mark)
		return ctx.no(at, BreakVariants, "none of "+plural(len(sub.AllOf), "allOf member"), typeNames(super))
	}

	// Composition in super (anyOf / oneOf): sub must fit at least one variant.
	for _, variants := range [][]*node{super.AnyOf, super.OneOf} {
		if variants == nil {
			continue
		}
		mark := ctx.mark()
		for _, v := range variants {
			if v != nil && ctx.check(sub, v, at) {
				ctx.rollback(mark)
				return true
			}
		}
		ctx.rollback(mark)
		// Both sides described rather than named: every variant of a union of objects is
		// `object`, so the type alone says nothing about which one stopped fitting.
		return ctx.no(at, BreakVariants, describeNode(sub), variantList(variants))
	}

	// allOf in super: sub must satisfy every constraint.
	if len(super.AllOf) > 0 {
		for _, v := range super.AllOf {
			if v == nil {
				return ctx.no(at, BreakVariants, typeNames(sub), "a null allOf member")
			}
			if !ctx.check(sub, v, at) {
				return false
			}
		}
		return true
	}

	// Type compatibility.
	if len(super.Type) > 0 {
		if len(sub.Type) == 0 {
			return ctx.no(at, BreakType, "untyped", typeNames(super))
		}
		for _, st := range sub.Type {
			if !typeAllowed(st, super.Type) {
				return ctx.no(at, BreakType, typeNames(sub), typeNames(super))
			}
		}
	}

	// Structural checks.
	if super.Properties != nil || super.Required != nil || super.AdditionalProperties != nil {
		if !ctx.checkObject(sub, super, at) {
			return false
		}
	}
	if super.Items != nil {
		if !ctx.checkArray(sub, super, at) {
			return false
		}
	}
	if super.MinItems != nil || super.MaxItems != nil {
		if !ctx.checkItemCount(sub, super, at) {
			return false
		}
	}
	if super.Minimum != nil || super.Maximum != nil {
		if !ctx.checkNumericBounds(sub, super, at) {
			return false
		}
	}
	if super.MinLength != nil || super.MaxLength != nil {
		if !ctx.checkStringLength(sub, super, at) {
			return false
		}
	}
	if super.Enum != nil {
		if !ctx.checkEnum(sub, super, at) {
			return false
		}
	}

	return true
}

// typeAllowed reports whether subType is permitted by superTypes.
// integer satisfies a super that includes number (numeric widening).
func typeAllowed(subType string, superTypes SchemaType) bool {
	for _, st := range superTypes {
		if st == subType || (subType == "integer" && st == "number") {
			return true
		}
	}
	return false
}

// guaranteed is the set of properties SUB is certain to hold; under afterConform that is
// required-or-defaulted. Asked of the sub side only — what super demands of an existing row
// is its `required` set and nothing more (specs/compat-command.md §2e).
func (ctx *subsetCtx) guaranteed(n *node, defs map[string]*node) map[string]bool {
	if !ctx.afterConform {
		return stringSet(n.Required)
	}
	// Not stringSet: it returns nil for an empty list, and a defaulted property with no
	// required list at all is the whole point of this mode.
	out := make(map[string]bool, len(n.Required)+len(n.Properties))
	for _, name := range n.Required {
		out[name] = true
	}
	for name, prop := range n.Properties {
		if propDefault(prop, defs) != nil {
			out[name] = true
		}
	}
	return out
}

// An object is the one place independent differences pile up, so while TRACING the walk
// keeps going and records each one; on the bool path it stops at the first. `stop` is that
// difference, in one place, and `ok` is what the extra iterations feed.
func (ctx *subsetCtx) checkObject(sub, super *node, at *pathLink) bool {
	subReq := ctx.guaranteed(sub, ctx.subDefs)
	ok := true
	stop := func() bool { ok = false; return ctx.trace == nil }

	for _, f := range super.Required {
		if subReq[f] {
			continue
		}
		// A nullable property need not be present for a READER: absence and null navigate
		// identically. `declared` is load-bearing twice — a required name with no property has no
		// type to call nullable (the fill agrees by skipping it), and hasNullResolved would nil-deref.
		if prop, declared := super.Properties[f]; ctx.absentAsNull && declared &&
			hasNullResolved(prop, ctx.superDefs) {
			continue
		}
		ctx.no(ctx.at(at, f), BreakMissingRequired, "not guaranteed", "required")
		if stop() {
			return false
		}
	}

	if super.Properties != nil {
		var subProps map[string]*node
		if sub.Properties != nil {
			subProps = sub.Properties
		}
		superReq := stringSet(super.Required)
		// Sorted so the reported break does not depend on map order.
		for _, name := range slices.Sorted(maps.Keys(super.Properties)) {
			superProp := super.Properties[name]
			if superProp == nil {
				continue
			}
			subProp, exists := subProps[name]
			if !exists {
				continue
			}
			mark := ctx.mark()
			if ctx.check(subProp, superProp, ctx.at(at, name)) {
				continue
			}
			// The other direction of the null-versus-missing gap: where super leaves the property
			// OPTIONAL, the migration reconciles a stored null by REMOVING the key. Sound only if
			// everything but the null already fits, which is what the stripped re-check asks;
			// `required` is the case nothing can fix, since absence is not valid there either.
			if ctx.afterConform && !superReq[name] && hasNullResolved(subProp, ctx.subDefs) {
				retry := ctx.mark()
				if ctx.check(stripNull(subProp), superProp, ctx.at(at, name)) {
					ctx.rollback(mark)
					continue
				}
				// An ALTERNATIVE like any other: its break describes a schema with the null taken
				// out, which is not the one anybody wrote.
				ctx.rollback(retry)
			}
			if stop() {
				return false
			}
		}
	}

	// Open-map super: every key sub can carry that super does not declare must fit
	// super's additionalProperties — both sub's own undeclared properties and sub's
	// own open-map values. (A closed super strips extras, so it imposes nothing here.)
	if super.AdditionalProperties != nil {
		for _, name := range slices.Sorted(maps.Keys(sub.Properties)) {
			if _, declared := super.Properties[name]; declared {
				continue
			}
			subProp := sub.Properties[name]
			if subProp == nil {
				ctx.no(ctx.at(at, name), BreakUnknown, "a null property", "the additionalProperties type")
				if stop() {
					return false
				}
				continue
			}
			if !ctx.check(subProp, super.AdditionalProperties, ctx.at(at, name)) && stop() {
				return false
			}
		}
		if sub.AdditionalProperties != nil &&
			!ctx.check(sub.AdditionalProperties, super.AdditionalProperties, ctx.at(at, "*")) && stop() {
			return false
		}
	}

	return ok
}

func (ctx *subsetCtx) checkArray(sub, super *node, at *pathLink) bool {
	if super.Items == nil {
		return true
	}
	// A provably-empty array (maxItems 0, e.g. a literal `[]`) holds no element that
	// could violate super's item type, so it is a subset of any array<T>.
	if sub.MaxItems != nil && *sub.MaxItems == 0 {
		return true
	}
	if sub.Items == nil {
		return ctx.no(ctx.at(at, "[]"), BreakType, "untyped", typeNames(super.Items))
	}
	return ctx.check(sub.Items, super.Items, ctx.at(at, "[]"))
}

// checkItemCount is checkStringLength for arrays. An unset bound on sub is treated as
// absent rather than as its implicit 0/∞, so an unbounded sub never fits a bounded super —
// the same conservative floor the other two use.
func (ctx *subsetCtx) checkItemCount(sub, super *node, at *pathLink) bool {
	if super.MinItems != nil {
		if sub.MinItems == nil {
			return ctx.no(at, BreakConstraint, "no minItems", "minItems "+strconv.Itoa(*super.MinItems))
		}
		if *sub.MinItems < *super.MinItems {
			return ctx.no(at, BreakConstraint,
				"minItems "+strconv.Itoa(*sub.MinItems), "minItems "+strconv.Itoa(*super.MinItems))
		}
	}
	if super.MaxItems != nil {
		if sub.MaxItems == nil {
			return ctx.no(at, BreakConstraint, "no maxItems", "maxItems "+strconv.Itoa(*super.MaxItems))
		}
		if *sub.MaxItems > *super.MaxItems {
			return ctx.no(at, BreakConstraint,
				"maxItems "+strconv.Itoa(*sub.MaxItems), "maxItems "+strconv.Itoa(*super.MaxItems))
		}
	}
	return true
}

func (ctx *subsetCtx) checkNumericBounds(sub, super *node, at *pathLink) bool {
	if super.Minimum != nil {
		if sub.Minimum == nil {
			return ctx.no(at, BreakConstraint, "no minimum", "minimum "+num(*super.Minimum))
		}
		if *sub.Minimum < *super.Minimum {
			return ctx.no(at, BreakConstraint,
				"minimum "+num(*sub.Minimum), "minimum "+num(*super.Minimum))
		}
	}
	if super.Maximum != nil {
		if sub.Maximum == nil {
			return ctx.no(at, BreakConstraint, "no maximum", "maximum "+num(*super.Maximum))
		}
		if *sub.Maximum > *super.Maximum {
			return ctx.no(at, BreakConstraint,
				"maximum "+num(*sub.Maximum), "maximum "+num(*super.Maximum))
		}
	}
	return true
}

func (ctx *subsetCtx) checkStringLength(sub, super *node, at *pathLink) bool {
	if super.MinLength != nil {
		if sub.MinLength == nil {
			return ctx.no(at, BreakConstraint, "no minLength", "minLength "+strconv.Itoa(*super.MinLength))
		}
		if *sub.MinLength < *super.MinLength {
			return ctx.no(at, BreakConstraint,
				"minLength "+strconv.Itoa(*sub.MinLength), "minLength "+strconv.Itoa(*super.MinLength))
		}
	}
	if super.MaxLength != nil {
		if sub.MaxLength == nil {
			return ctx.no(at, BreakConstraint, "no maxLength", "maxLength "+strconv.Itoa(*super.MaxLength))
		}
		if *sub.MaxLength > *super.MaxLength {
			return ctx.no(at, BreakConstraint,
				"maxLength "+strconv.Itoa(*sub.MaxLength), "maxLength "+strconv.Itoa(*super.MaxLength))
		}
	}
	return true
}

func (ctx *subsetCtx) checkEnum(sub, super *node, at *pathLink) bool {
	if super.Enum == nil {
		return true
	}
	if sub.Enum == nil {
		return ctx.no(at, BreakConstraint, "any "+typeNames(sub), enumValues(super.Enum))
	}
	superSet := make(map[string]bool, len(super.Enum))
	for _, v := range super.Enum {
		superSet[jsonKey(v)] = true
	}
	var extra []any
	for _, v := range sub.Enum {
		if !superSet[jsonKey(v)] && !enumHasNumeric(super.Enum, v) {
			extra = append(extra, v)
		}
	}
	if len(extra) == 0 {
		return true
	}
	// Name the values that do not fit, rather than printing both pools. An absence cannot be
	// SHOWN in a capped list: two large enums differing by one value rendered identically on
	// both sides of the arrow, so the line reported a change while saying nothing changed.
	return ctx.no(at, BreakConstraint, "allows "+enumList(extra), enumValues(super.Enum))
}

func jsonKey(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func derefSubset(s *node, defs map[string]*node) *node {
	if s == nil || s.Ref == "" || defs == nil {
		return s
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(s.Ref, prefix) {
		return s
	}
	if target, ok := defs[strings.TrimPrefix(s.Ref, prefix)]; ok && target != nil {
		return target
	}
	return s
}

func stringSet(arr []string) map[string]bool {
	if len(arr) == 0 {
		return nil
	}
	out := make(map[string]bool, len(arr))
	for _, s := range arr {
		out[s] = true
	}
	return out
}
