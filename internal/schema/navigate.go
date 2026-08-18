package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// pathStep is one segment of a navigation path. The kind is an explicit tag
// rather than something inferred from the fields: a property key is an arbitrary
// JSON string, and once expressions can spell one (a["…"]) the empty key is
// reachable and would otherwise be indistinguishable from index 0.
type stepKind uint8

const (
	stepProp  stepKind = iota // a named property: .name or ["name"]
	stepIndex                 // a literal array index: [0]
	// stepKey is a computed key, a[expr]. It names no single position, so it
	// navigates to the type every key shares — the array element or the map value.
	// key holds the key expression when that is itself a static path, which is what
	// lets two reads of a[k] share a narrowing guard.
	stepKey
)

type pathStep struct {
	kind  stepKind
	prop  string
	index int
	key   []pathStep
}

func propStep(name string) pathStep { return pathStep{kind: stepProp, prop: name} }
func indexStep(i int) pathStep      { return pathStep{kind: stepIndex, index: i} }
func keyStep(key []pathStep) pathStep {
	return pathStep{kind: stepKey, key: key}
}

// parsePath reads the syntax renderPath emits, so an error-message path can be handed
// straight back to At/SecretAt. Bare segments run to '.'/'['; the quoted form is the only
// way to name a key containing them.
func parsePath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, fmt.Errorf("path must not be empty")
	}
	var steps []pathStep
	i := 0
	for i < len(path) {
		switch {
		case path[i] == '[':
			step, next, err := parseBracket(path, i)
			if err != nil {
				return nil, err
			}
			steps = append(steps, step)
			i = next
		case path[i] == '.':
			// A separator is only meaningful between steps; the segment that follows
			// carries the name.
			if len(steps) == 0 || i+1 == len(path) || path[i+1] == '.' {
				return nil, fmt.Errorf("invalid path %q: empty segment", path)
			}
			i++
		default:
			end := i
			for end < len(path) && path[end] != '.' && path[end] != '[' {
				end++
			}
			if end == i {
				return nil, fmt.Errorf("invalid path %q: empty segment", path)
			}
			// Two adjacent bare segments cannot occur — the loop above stops only at a
			// separator — so a name here always follows '.' or starts the path.
			steps = append(steps, propStep(path[i:end]))
			i = end
		}
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("invalid path %q: no steps", path)
	}
	return steps, nil
}

// parseBracket reads one [...] step starting at open, returning the step and the
// offset just past the closing bracket.
func parseBracket(path string, open int) (pathStep, int, error) {
	if open+1 < len(path) && path[open+1] == '"' {
		// Scan for the closing quote, respecting backslash escapes, then let
		// strconv.Unquote apply the same escape rules that quoted it.
		for j := open + 2; j < len(path); j++ {
			switch path[j] {
			case '\\':
				j++
			case '"':
				if j+1 >= len(path) || path[j+1] != ']' {
					return pathStep{}, 0, fmt.Errorf("invalid path %q: expected ']' after quoted key", path)
				}
				name, err := strconv.Unquote(path[open+1 : j+1])
				if err != nil {
					return pathStep{}, 0, fmt.Errorf("invalid path %q: bad quoted key: %w", path, err)
				}
				return propStep(name), j + 2, nil
			}
		}
		return pathStep{}, 0, fmt.Errorf("invalid path %q: unterminated quoted key", path)
	}
	close := strings.IndexByte(path[open:], ']')
	if close == -1 {
		return pathStep{}, 0, fmt.Errorf("invalid path %q: unmatched '['", path)
	}
	close += open
	idx, err := strconv.Atoi(path[open+1 : close])
	if err != nil {
		return pathStep{}, 0, fmt.Errorf("invalid path %q: non-integer index %q", path, path[open+1:close])
	}
	return indexStep(idx), close + 1, nil
}

func navigate(s *node, defs map[string]*node, path string) (*node, error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	return navigateSchema(s, defs, steps)
}

// lookupProperty: optional-without-default comes back nullable. A $ref-valued property
// returns as the ref, not its expansion — keeping recursive output types finite;
// descent resolves on demand.
func lookupProperty(s *node, name string, defs map[string]*node) (*node, error) {
	return lookupPropertyGuard(s, name, defs, nil)
}

// lookupPropertyGuard is lookupProperty with a union-walk cycle guard: visiting holds
// union nodes already being walked at this value position, so a reference cycle through
// union variants fails that variant (a miss) instead of recursing forever. Recursion
// through properties/items starts fresh — that is productive structure, not a cycle.
func lookupPropertyGuard(s *node, name string, defs map[string]*node, visiting map[*node]bool) (*node, error) {
	resolved, err := deref(s, defs)
	if err != nil {
		return nil, err
	}

	// A property of null is null, not an access error — matching the evaluator (member access
	// on missing yields nil, letting `a.x ?? b.x` fall through). Path-partitioned output
	// inference relies on it: definitely-absent outputs type null and reads must yield null.
	if isNullType(resolved) {
		return &node{Type: SchemaType{"null"}}, nil
	}

	for _, kw := range []struct {
		name     string
		variants []*node
	}{
		{"anyOf", resolved.AnyOf},
		{"oneOf", resolved.OneOf},
	} {
		if kw.variants == nil {
			continue
		}
		if visiting[resolved] {
			return nil, fmt.Errorf("cannot access .%s: schema reference cycle", name)
		}
		next := make(map[*node]bool, len(visiting)+1)
		for k := range visiting {
			next[k] = true
		}
		next[resolved] = true
		results := make([]*node, 0, len(kw.variants))
		hadNull := false
		hadMiss := false
		for i, v := range kw.variants {
			if v == nil {
				return nil, fmt.Errorf("cannot access .%s: %s[%d] is nil", name, kw.name, i)
			}
			// Accessing a property *inside* a union variant is a look-inside
			// operation: resolve a $ref variant first, so a reference to a
			// null seed (mid-solve estimate) counts as the null arm rather
			// than a missing field.
			rv, verr := deref(v, defs)
			if verr != nil {
				return nil, verr
			}
			if isNullType(rv) {
				hadNull = true
				continue
			}
			// An unknown variant makes the whole access undecidable, so it is refused rather
			// than folded in as another null arm. A miss elsewhere means "this variant does
			// not have that field" — an answer; the top type means "nothing here is
			// declared", which is not one. Reading through it would be reading INTO unknown
			// data, which is the one thing {} exists to prevent.
			if isEmptyNode(rv) {
				return nil, fmt.Errorf("cannot access .%s: one variant of the value is unknown (its schema is {}), "+
					"so nothing can be read through it — declare that variant's shape, or read the whole value", name)
			}
			r, err := lookupPropertyGuard(rv, name, defs, next)
			if err != nil {
				hadMiss = true
				hadNull = true
				continue
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			if hadMiss {
				return nil, fmt.Errorf("field %q not found in any %s variant", name, kw.name)
			}
			return &node{Type: SchemaType{"null"}}, nil
		}
		var result *node
		if allSame(results) {
			result = results[0]
		} else {
			cp := make([]*node, len(results))
			copy(cp, results)
			if kw.name == "oneOf" {
				result = &node{OneOf: cp}
			} else {
				result = &node{AnyOf: cp}
			}
		}
		if hadNull {
			return withNull(result), nil
		}
		return result, nil
	}

	prop, ok := resolved.Properties[name]
	if !ok {
		// Undeclared key. On an open map (additionalProperties set) it takes the
		// additionalProperties type, wrapped nullable since the key may be absent;
		// on a closed object it's an access error.
		if resolved.AdditionalProperties != nil {
			return withNull(resolved.AdditionalProperties), nil
		}
		if resolved.Properties == nil {
			// The top type ({}) is the one "no properties" case with an actionable fix,
			// and the one an author reaches deliberately, so it gets its own message —
			// the more so because the empty schema does not announce its own intent.
			if isEmptyNode(resolved) {
				return nil, fmt.Errorf("cannot access .%s: the value is unknown (its schema is {})", name)
			}
			return nil, fmt.Errorf("cannot access .%s: schema has no properties", name)
		}
		return nil, fmt.Errorf("field %q not found in schema", name)
	}
	// Returned as declared — a $ref stays a $ref, its taint riding the ref node. Non-nullable
	// iff guaranteed present after validation: required, or defaulted (conformObject fills an
	// absent optional's default). Only optional-without-default comes back nullable.
	if !isRequired(resolved, name) && propDefault(prop, defs) == nil {
		return withNull(prop), nil
	}
	return prop, nil
}

// inferIndex returns the element type for array index access on s, always nullable
// because the index may be out of bounds at runtime.
func inferIndex(s *node, defs map[string]*node) (*node, error) {
	return inferIndexGuard(s, defs, nil)
}

// inferIndexGuard carries the same union-walk cycle guard as
// lookupPropertyGuard.
func inferIndexGuard(s *node, defs map[string]*node, visiting map[*node]bool) (*node, error) {
	resolved, err := deref(s, defs)
	if err != nil {
		return nil, err
	}
	resolved = stripNull(resolved)

	for _, variants := range [][]*node{resolved.AnyOf, resolved.OneOf} {
		if variants == nil {
			continue
		}
		if visiting[resolved] {
			return nil, fmt.Errorf("index access [n]: schema reference cycle")
		}
		next := make(map[*node]bool, len(visiting)+1)
		for k := range visiting {
			next[k] = true
		}
		next[resolved] = true
		results := make([]*node, 0, len(variants))
		hadNull := false
		for _, v := range variants {
			if v == nil {
				continue
			}
			// Indexing into a union variant is a look-inside operation:
			// resolve a $ref variant first (a mid-solve null seed counts as
			// the null arm).
			rv, verr := deref(v, defs)
			if verr != nil {
				return nil, verr
			}
			if isNullType(rv) {
				hadNull = true
				continue
			}
			// Same rule as a property access through a union: an unknown variant makes the
			// element type undecidable rather than absent, so indexing through it is refused.
			if isEmptyNode(rv) {
				return nil, fmt.Errorf("cannot index: one variant of the value is unknown (its schema is {}), " +
					"so nothing can be read through it — declare that variant's shape, or read the whole value")
			}
			r, err := inferIndexGuard(rv, defs, next)
			if err != nil {
				hadNull = true
				continue
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			return &node{Type: SchemaType{"null"}}, nil
		}
		var result *node
		if allSame(results) {
			result = results[0]
		} else {
			result = &node{AnyOf: results}
		}
		if hadNull && !hasNullType(result) {
			return withNull(result), nil
		}
		return result, nil
	}

	if !resolved.Type.Contains("array") {
		t := ""
		if len(resolved.Type) > 0 {
			t = resolved.Type[0]
		}
		return nil, fmt.Errorf("index access [n] requires an array schema, got type %q", t)
	}
	if resolved.Items == nil {
		return &node{}, nil
	}
	return withNull(resolved.Items), nil
}

// deref follows $ref chains (a definition may alias another after a collision rename);
// a repeated node is a pure ref cycle and errors. A solver-sentinel target routes back to
// its Solver — the hook that makes resolution demand-driven during generation.
func deref(s *node, defs map[string]*node) (*node, error) {
	var seen map[*node]bool
	for s != nil && s.Ref != "" {
		if seen[s] {
			return nil, fmt.Errorf("circular $ref %q never resolves to a schema", s.Ref)
		}
		if defs == nil {
			return nil, fmt.Errorf("cannot resolve $ref %q: no defs available", s.Ref)
		}
		const prefix = "#/$defs/"
		if !strings.HasPrefix(s.Ref, prefix) {
			return nil, fmt.Errorf("unsupported $ref %q: only #/$defs/<name> is supported", s.Ref)
		}
		target, ok := defs[strings.TrimPrefix(s.Ref, prefix)]
		if !ok || target == nil {
			return nil, fmt.Errorf("$ref %q not found in defs", s.Ref)
		}
		if seen == nil {
			seen = map[*node]bool{}
		}
		seen[s] = true
		if p := target.pending; p != nil {
			resolved, err := p.solver.resolvePending(p.name)
			if err != nil {
				return nil, err
			}
			s = resolved
			continue
		}
		if target.Anchor == pendingAnchor {
			return nil, fmt.Errorf("internal: $ref %q points at an unresolved pending definition", s.Ref)
		}
		s = target
	}
	return s, nil
}

// isSecret reports whether s is marked secret:true, looking through nullable /
// single-variant union wrappers so an optional or wrapped secret is still recognised.
func isSecret(s *node) bool {
	if s == nil {
		return false
	}
	if s.Secret {
		return true
	}
	for _, v := range s.OneOf {
		if isSecret(v) {
			return true
		}
	}
	for _, v := range s.AnyOf {
		if isSecret(v) {
			return true
		}
	}
	return false
}

// taintNode returns a copy of s marked secret:true — used to taint (conservatively, as
// a whole) the result of an expression that reads a secret value.
func taintNode(s *node) *node {
	if s == nil {
		return &node{Secret: true}
	}
	if s.Secret {
		return s
	}
	n := *s
	n.Secret = true
	return &n
}

// A taint on a $ref marks the pointer, not the shared target (that would over-taint its
// other users), so BOTH sides are consulted — and at every union position: items and
// additionalProperties values arrive as oneOf[{$ref}, null], where a missed arm's taint
// sends the value out unredacted.
func nodeOrTargetSecret(n *node, defs map[string]*node) bool {
	return secretThroughRefs(n, defs, nil)
}

// secretThroughRefs walks a node's union variants and $ref targets. visiting guards
// the union walk against a reference cycle; deref carries its own guard for chains.
func secretThroughRefs(n *node, defs map[string]*node, visiting map[*node]bool) bool {
	if n == nil || visiting[n] {
		return false
	}
	if n.Secret {
		return true
	}
	next := make(map[*node]bool, len(visiting)+1)
	for k := range visiting {
		next[k] = true
	}
	next[n] = true
	if n.Ref != "" {
		if resolved, err := deref(n, defs); err == nil && secretThroughRefs(resolved, defs, next) {
			return true
		}
	}
	for _, variants := range [][]*node{n.OneOf, n.AnyOf} {
		for _, v := range variants {
			if secretThroughRefs(v, defs, next) {
				return true
			}
		}
	}
	return false
}

// pathHitsSecret reports whether navigating path from s passes through (or ends at) a
// secret node — reading from inside a secret object is itself secret. False if the path
// cannot be resolved.
func pathHitsSecret(s *node, defs map[string]*node, path string) bool {
	steps, err := parsePath(path)
	if err != nil {
		return false
	}
	return stepsHitSecret(s, defs, steps)
}

// stepsHitSecret is pathHitsSecret over already-parsed steps. Callers that derive a
// path from an expression tree use this: a property key there is an arbitrary string
// which the dot-path syntax cannot round-trip (a key containing "." or "[" would
// re-split into the wrong steps, and silently miss the secret).
func stepsHitSecret(s *node, defs map[string]*node, steps []pathStep) bool {
	if nodeOrTargetSecret(s, defs) {
		return true
	}
	cur, err := deref(s, defs)
	if err != nil {
		return false
	}
	for _, step := range steps {
		cur, err = navigateStep(cur, defs, step)
		if err != nil {
			return false
		}
		if nodeOrTargetSecret(cur, defs) {
			return true
		}
	}
	return false
}

// collectSecrets appends to *out the string form of every secret-marked value in value.
// It descends via lookupProperty / inferIndex — the same primitives type inference uses
// — so the walk cannot drift from schema navigation. Gather half of log redaction: the
// collected values are then scrubbed from free-form log text.
func collectSecrets(value any, node *node, defs map[string]*node, out *[]string) {
	if node == nil || value == nil {
		return
	}
	if nodeOrTargetSecret(node, defs) {
		if s := SecretString(value); s != "" {
			*out = append(*out, s)
		}
		return
	}
	resolved, err := deref(node, defs)
	if err != nil {
		return
	}
	switch v := value.(type) {
	case map[string]any:
		for k, val := range v {
			if child, err := lookupProperty(resolved, k, defs); err == nil {
				collectSecrets(val, child, defs, out)
			}
		}
	case []any:
		child, err := inferIndex(resolved, defs)
		if err != nil {
			return
		}
		for _, el := range v {
			collectSecrets(el, child, defs, out)
		}
	}
}

// redact returns value with every secret-marked field replaced by "***", descending via
// lookupProperty / inferIndex like collectSecrets. Non-secret values pass through
// unchanged. Used to scrub secret-derived values before they cross a public boundary.
func redact(value any, node *node, defs map[string]*node) any {
	if node == nil || value == nil {
		return value
	}
	if nodeOrTargetSecret(node, defs) {
		return "***"
	}
	resolved, err := deref(node, defs)
	if err != nil {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if child, err := lookupProperty(resolved, k, defs); err == nil {
				out[k] = redact(val, child, defs)
			} else {
				out[k] = val // key not in schema — leave untouched
			}
		}
		return out
	case []any:
		child, err := inferIndex(resolved, defs)
		if err != nil {
			return value
		}
		out := make([]any, len(v))
		for i, el := range v {
			out[i] = redact(el, child, defs)
		}
		return out
	default:
		return value
	}
}

// SecretString renders a secret value as it appears in logs so the substring scrub
// matches it. Strings pass through raw (they appear unquoted, and as a substring of
// their quoted JSON form). Everything else uses its JSON encoding — notably a number is
// "1000000" as json.Marshal writes it, not fmt's "1e+06", which would never match.
func SecretString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// isNullType reports whether s is exactly {type:"null"}.
func isNullType(s *node) bool {
	return s != nil && len(s.Type) == 1 && s.Type[0] == "null"
}

// hasNullType reports whether null is a possible type for s.
func hasNullType(s *node) bool {
	if s == nil {
		return false
	}
	if s.Type.Contains("null") {
		return true
	}
	for _, v := range s.OneOf {
		if isNullType(v) {
			return true
		}
	}
	for _, v := range s.AnyOf {
		if isNullType(v) {
			return true
		}
	}
	return false
}

// IsType reports whether s resolves to uniformly type typ: a non-empty type list all
// equal to typ, or a oneOf/anyOf whose variants all resolve to typ. $refs are followed,
// so a reference to a boolean definition is a boolean. Used, e.g., to require a switch
// expression to be boolean.
func (s Schema) IsType(typ string) bool { return nodeIsType(s.n, s.rootDefs(), typ) }

func nodeIsType(s *node, defs map[string]*node, typ string) bool {
	s, err := deref(s, defs)
	if err != nil || s == nil {
		return false
	}
	if len(s.Type) > 0 {
		for _, t := range s.Type {
			if t != typ {
				return false
			}
		}
		return true
	}
	for _, variants := range [][]*node{s.OneOf, s.AnyOf} {
		if variants == nil {
			continue
		}
		for _, v := range variants {
			if !nodeIsType(v, defs, typ) {
				return false
			}
		}
		return len(variants) > 0
	}
	return false
}

// hasNullResolved reports whether null is a possible runtime value for s, following
// $refs (top-level and one union level, matching hasNullType's depth) so nullability
// declared inside a referenced definition is seen. Resolution failures degrade to the
// structural answer.
func hasNullResolved(s *node, defs map[string]*node) bool {
	return hasNullGuard(s, defs, nil)
}

// hasNullGuard recurses through nested unions: a null inside oneOf[oneOf[…, null], …]
// is still a null the value may take, and a one-level scan under-reports it — which
// would let a caller treat a nullable type as non-null. visiting holds union nodes
// already on the walk so a reference cycle terminates instead of recursing forever.
func hasNullGuard(s *node, defs map[string]*node, visiting map[*node]bool) bool {
	// A nil node is the zero Schema — no declared type, so nothing that admits null. It
	// reaches here from any map index that missed, which is an ordinary way to ask "is the
	// property at this name nullable"; panicking on it would make every such caller guard.
	if s == nil {
		return false
	}
	r, err := deref(s, defs)
	if err != nil || r == nil {
		return hasNullType(s)
	}
	if hasNullType(r) {
		return true
	}
	if visiting[r] {
		return false
	}
	if len(r.OneOf) == 0 && len(r.AnyOf) == 0 {
		return false
	}
	next := make(map[*node]bool, len(visiting)+1)
	for k := range visiting {
		next[k] = true
	}
	next[r] = true
	for _, variants := range [][]*node{r.OneOf, r.AnyOf} {
		for _, v := range variants {
			if v != nil && hasNullGuard(v, defs, next) {
				return true
			}
		}
	}
	return false
}

// TypeName returns a readable name for s's type ("string", "string|null", or "unknown"),
// for error messages.
func (s Schema) TypeName() string { return nodeTypeName(s.n) }

func nodeTypeName(s *node) string {
	if s == nil {
		return "unknown"
	}
	if len(s.Type) > 0 {
		return strings.Join([]string(s.Type), "|")
	}
	for _, variants := range [][]*node{s.OneOf, s.AnyOf} {
		if variants == nil {
			continue
		}
		seen := make(map[string]bool, len(variants))
		var parts []string
		for _, v := range variants {
			if v == nil {
				continue
			}
			name := nodeTypeName(v)
			if !seen[name] {
				seen[name] = true
				parts = append(parts, name)
			}
		}
		return strings.Join(parts, "|")
	}
	return "unknown"
}

// withNull makes s nullable: a simple type widens to {type:[T,"null"]}; a complex schema
// is wrapped in {oneOf:[s,{type:"null"}]}.
func withNull(s *node) *node {
	if s == nil || isEmptyNode(s) {
		return s
	}
	if s.Type.Contains("null") {
		return s
	}
	for _, v := range s.OneOf {
		if isNullType(v) {
			return s
		}
	}
	for _, v := range s.AnyOf {
		if isNullType(v) {
			return s
		}
	}
	// Simple type without properties — widen type array to include null.
	if len(s.Type) >= 1 && s.Properties == nil {
		n := *s
		n.Type = make(SchemaType, len(s.Type)+1)
		copy(n.Type, s.Type)
		n.Type[len(s.Type)] = "null"
		return &n
	}
	return &node{OneOf: []*node{s, {Type: SchemaType{"null"}}}}
}

func stripNull(s *node) *node {
	if s == nil {
		return s
	}
	if len(s.Type) > 0 {
		var nonNull SchemaType
		for _, t := range s.Type {
			if t != "null" {
				nonNull = append(nonNull, t)
			}
		}
		if len(nonNull) == len(s.Type) {
			return s
		}
		n := *s
		n.Type = nonNull
		return &n
	}
	if len(s.OneOf) > 0 {
		nonNull, changed := stripNullVariants(s.OneOf)
		if !changed || len(nonNull) == 0 {
			return s
		}
		if len(nonNull) == 1 {
			return nonNull[0]
		}
		n := *s
		n.OneOf = nonNull
		return &n
	}
	if len(s.AnyOf) > 0 {
		nonNull, changed := stripNullVariants(s.AnyOf)
		if !changed || len(nonNull) == 0 {
			return s
		}
		if len(nonNull) == 1 {
			return nonNull[0]
		}
		n := *s
		n.AnyOf = nonNull
		return &n
	}
	return s
}

// Drops exactly-null variants AND strips null inside survivors, so StripNull's contract
// (HasNull false after) holds when nullability hides in an arm's type list. $refs ride
// through underefed — that keeps recursion finite; inferNullCoalesce materializes for those.
func stripNullVariants(vs []*node) ([]*node, bool) {
	out := make([]*node, 0, len(vs))
	changed := false
	for _, v := range vs {
		if isNullType(v) {
			changed = true
			continue
		}
		sv := stripNull(v)
		if sv != v {
			changed = true
		}
		out = append(out, sv)
	}
	return out, changed
}

// IsUnknown reports whether s is the top type ({}) — undeclared data, which is carried but
// never read. Distinct from "a type we could not pin down": a union of scalars is unpinned
// but declared, while this constrains nothing at all.
func (s Schema) IsUnknown() bool { return isEmptyNode(s.n) }

// isEmptyNode: does s constrain nothing — the top type {}, i.e. unknown? Root $defs are
// ignored (navigation sub-schemas carry them as context only), and so is Description:
// {"description": …} is still top, the recommended spelling for deliberate opacity.
// Secret/Default being treated as top predates the unknown work; left as-is.
func isEmptyNode(s *node) bool {
	return s == nil || (len(s.Type) == 0 && s.Properties == nil && s.Required == nil &&
		s.AdditionalProperties == nil &&
		s.Items == nil && s.OneOf == nil && s.AnyOf == nil && s.AllOf == nil &&
		s.Enum == nil && s.Ref == "" && s.Minimum == nil &&
		s.Maximum == nil && s.MinLength == nil && s.MaxLength == nil &&
		s.MinItems == nil && s.MaxItems == nil)
}

func navigateSchema(s *node, defs map[string]*node, steps []pathStep) (*node, error) {
	current := s
	for _, step := range steps {
		var err error
		current, err = navigateStep(current, defs, step)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

// navigateStep applies one step. The three kinds share this so navigation,
// secret-walking and inference cannot drift in what a step means.
func navigateStep(cur *node, defs map[string]*node, step pathStep) (*node, error) {
	switch step.kind {
	case stepIndex:
		return inferIndex(cur, defs)
	case stepKey:
		value, _, err := anyKey(cur, defs)
		return value, err
	default:
		return lookupProperty(cur, step.prop, defs)
	}
}

// anyKey: the type every key of s shares, and the key type required — array elements
// (integer key) or additionalProperties-only maps (string key). Exists only for those two
// shapes; with declared properties the type varies per key, so a computed key errors.
func anyKey(s *node, defs map[string]*node) (*node, string, error) {
	resolved, err := deref(s, defs)
	if err != nil {
		return nil, "", err
	}
	stripped := stripNull(resolved)
	switch {
	case stripped.Type.Contains("array") || stripped.Items != nil:
		el, err := inferIndex(stripped, defs)
		return el, "integer", err
	case stripped.AdditionalProperties != nil && len(stripped.Properties) == 0:
		// Nullable for the same reason an index is: the key may be absent.
		return withNull(stripped.AdditionalProperties), "string", nil
	case len(stripped.Properties) > 0:
		return nil, "", fmt.Errorf("cannot read a computed key: the value declares named properties, so its type depends on which key is read; use a literal key")
	case isEmptyNode(stripped):
		return nil, "", fmt.Errorf("cannot read a computed key: the value is unknown (its schema is {})")
	}
	return nil, "", fmt.Errorf("cannot read a computed key: the value is not an array or a map")
}

func isRequired(s *node, name string) bool {
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}
	return false
}

func allSame(schemas []*node) bool {
	if len(schemas) == 0 {
		return true
	}
	first, _ := json.Marshal(schemas[0])
	for _, s := range schemas[1:] {
		other, _ := json.Marshal(s)
		if string(first) != string(other) {
			return false
		}
	}
	return true
}
