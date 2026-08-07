package schema

import (
	"encoding/json"
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

// storedSubset compares two schemas as descriptions of data that has ALREADY been conformed,
// which is what an instance's stored state is. It relaxes two rules, and they are relaxed for
// different reasons that must not be conflated:
//
//   - absentAsNull, as above: a gap the migration closes by writing the null in.
//   - afterConform: a property sub declares with a default is guaranteed present, because
//     creation filled it. **This one needs no fill** — the value is in the row already — which
//     is why it is not part of absentAsNullSubset, whose whole contract is that it tolerates
//     exactly what Validate(v, ConformToSchemaExactly) closes (schematest/absent_test.go).
//
// The default rule reads the SUB side as a guarantee and the SUPER side as a requirement:
// a default on super means only that super-conformed data would have had it, so sub must
// still guarantee it. Sound only where nothing conforms the value against super afterwards.
// Design: specs/compat-command.md §2e.
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
	return ctx.check(sub, super)
}

type subsetCtx struct {
	subDefs   map[string]*node
	superDefs map[string]*node
	visiting  map[string]bool
	subsetMode
}

func (ctx *subsetCtx) check(sub, super *node) bool {
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
		return ctx.narrow
	}

	// Composition in sub (anyOf / oneOf): every variant must be ⊆ super.
	for _, variants := range [][]*node{sub.AnyOf, sub.OneOf} {
		if variants == nil {
			continue
		}
		for _, v := range variants {
			if v == nil || !ctx.check(v, super) {
				return false
			}
		}
		return true
	}

	// allOf in sub: if any single constraint is ⊆ super then the allOf is too.
	if len(sub.AllOf) > 0 {
		for _, v := range sub.AllOf {
			if v != nil && ctx.check(v, super) {
				return true
			}
		}
		return false
	}

	// Composition in super (anyOf / oneOf): sub must fit at least one variant.
	for _, variants := range [][]*node{super.AnyOf, super.OneOf} {
		if variants == nil {
			continue
		}
		for _, v := range variants {
			if v != nil && ctx.check(sub, v) {
				return true
			}
		}
		return false
	}

	// allOf in super: sub must satisfy every constraint.
	if len(super.AllOf) > 0 {
		for _, v := range super.AllOf {
			if v == nil || !ctx.check(sub, v) {
				return false
			}
		}
		return true
	}

	// Type compatibility.
	if len(super.Type) > 0 {
		if len(sub.Type) == 0 {
			return false
		}
		for _, st := range sub.Type {
			if !typeAllowed(st, super.Type) {
				return false
			}
		}
	}

	// Structural checks.
	if super.Properties != nil || super.Required != nil || super.AdditionalProperties != nil {
		if !ctx.checkObject(sub, super) {
			return false
		}
	}
	if super.Items != nil {
		if !ctx.checkArray(sub, super) {
			return false
		}
	}
	if super.Minimum != nil || super.Maximum != nil {
		if !checkNumericBounds(sub, super) {
			return false
		}
	}
	if super.MinLength != nil || super.MaxLength != nil {
		if !checkStringLength(sub, super) {
			return false
		}
	}
	if super.Enum != nil {
		if !checkEnum(sub, super) {
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

// guaranteed is the set of properties SUB is certain to hold. Under afterConform that is
// required-or-defaulted, because the conform that produced this data filled the default.
//
// It is asked of the sub side only. Super's defaults say what data conformed under SUPER
// would hold — and the row in hand was not: it was conformed under sub and is being carried
// over, with a fill that writes no defaults. What super demands of an existing row is its
// `required` set and nothing more (specs/compat-command.md §2e).
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

func (ctx *subsetCtx) checkObject(sub, super *node) bool {
	subReq := ctx.guaranteed(sub, ctx.subDefs)

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
		return false
	}

	if super.Properties != nil {
		var subProps map[string]*node
		if sub.Properties != nil {
			subProps = sub.Properties
		}
		superReq := stringSet(super.Required)
		for name, superProp := range super.Properties {
			if superProp == nil {
				continue
			}
			subProp, exists := subProps[name]
			if !exists {
				continue
			}
			if ctx.check(subProp, superProp) {
				continue
			}
			// The other direction of the null-versus-missing gap, and the mirror of the rule
			// above. Sub may hold a null here that super will not take — but where super
			// leaves the property OPTIONAL, the migration reconciles it by REMOVING the key,
			// so the pair still fits. Sound only if everything but the null already fits,
			// which is what the stripped re-check asks; required is the case nothing can fix,
			// since absence is not valid there either.
			if ctx.afterConform && !superReq[name] &&
				hasNullResolved(subProp, ctx.subDefs) &&
				ctx.check(stripNull(subProp), superProp) {
				continue
			}
			return false
		}
	}

	// Open-map super: every key sub can carry that super does not declare must fit
	// super's additionalProperties — both sub's own undeclared properties and sub's
	// own open-map values. (A closed super strips extras, so it imposes nothing here.)
	if super.AdditionalProperties != nil {
		for name, subProp := range sub.Properties {
			if _, declared := super.Properties[name]; declared {
				continue
			}
			if subProp == nil || !ctx.check(subProp, super.AdditionalProperties) {
				return false
			}
		}
		if sub.AdditionalProperties != nil && !ctx.check(sub.AdditionalProperties, super.AdditionalProperties) {
			return false
		}
	}

	return true
}

func (ctx *subsetCtx) checkArray(sub, super *node) bool {
	if super.Items == nil {
		return true
	}
	// A provably-empty array (maxItems 0, e.g. a literal `[]`) holds no element that
	// could violate super's item type, so it is a subset of any array<T>.
	if sub.MaxItems != nil && *sub.MaxItems == 0 {
		return true
	}
	if sub.Items == nil {
		return false
	}
	return ctx.check(sub.Items, super.Items)
}

func checkNumericBounds(sub, super *node) bool {
	if super.Minimum != nil {
		if sub.Minimum == nil || *sub.Minimum < *super.Minimum {
			return false
		}
	}
	if super.Maximum != nil {
		if sub.Maximum == nil || *sub.Maximum > *super.Maximum {
			return false
		}
	}
	return true
}

func checkStringLength(sub, super *node) bool {
	if super.MinLength != nil {
		if sub.MinLength == nil || *sub.MinLength < *super.MinLength {
			return false
		}
	}
	if super.MaxLength != nil {
		if sub.MaxLength == nil || *sub.MaxLength > *super.MaxLength {
			return false
		}
	}
	return true
}

func checkEnum(sub, super *node) bool {
	if super.Enum == nil {
		return true
	}
	if sub.Enum == nil {
		return false
	}
	superSet := make(map[string]bool, len(super.Enum))
	for _, v := range super.Enum {
		superSet[jsonKey(v)] = true
	}
	for _, v := range sub.Enum {
		if !superSet[jsonKey(v)] {
			return false
		}
	}
	return true
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
