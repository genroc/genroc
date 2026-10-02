package schema

import (
	"encoding/json"
	"errors"
	"fmt"
)

// inferBinaryOps must accept the same operator set as internal/expression's evalBinary.
var inferBinaryOps = map[string]func(left, right Schema) (Schema, error){
	"==": inferEquality,
	"!=": inferEquality,
	"<":  inferOrderingCmp,
	">":  inferOrderingCmp,
	"<=": inferOrderingCmp,
	">=": inferOrderingCmp,
	"&&": inferLogical,
	"||": inferLogical,
	"+":  inferAdd,
	"-":  inferArith,
	"*":  inferArith,
	"/":  inferDiv,
	"%":  inferMod,
	"??": inferNullCoalesce,
}

// inferUnaryOps is the unary counterpart of inferBinaryOps.
var inferUnaryOps = map[string]func(operand Schema) (Schema, error){
	"!": inferNot,
	"-": numericPassthrough,
	"+": numericPassthrough,
}

// inferEquality rejects comparing two structured values (a deep walk hidden behind an
// operator; the runtime refuses too). Fires only when BOTH sides are provably structured,
// so `someArray == null` and container-vs-scalar (merely always false) keep working.
func inferEquality(left, right Schema) (Schema, error) {
	if isStructured(left) && isStructured(right) {
		return Schema{}, fmt.Errorf("== and != are not supported between %s and %s values", left.TypeName(), right.TypeName())
	}
	return Type("boolean"), nil
}

func isStructured(s Schema) bool {
	return s.IsType("array") || s.IsType("object")
}

// binOperands rejects a nullable or ambiguous operand and returns both concrete types.
// nullErr/ambiguousErr are already-resolved messages (a literal "%", not "%%").
func binOperands(left, right Schema, nullErr, ambiguousErr string) (lt, rt string, err error) {
	if left.HasNull() || right.HasNull() {
		return "", "", errors.New(nullErr)
	}
	ltype, ltOK := concreteTypeOf(left)
	rtype, rtOK := concreteTypeOf(right)
	if !ltOK || !rtOK {
		return "", "", errors.New(ambiguousErr)
	}
	return ltype, rtype, nil
}

// unaryOperand is the single-operand counterpart of binOperands.
func unaryOperand(operand Schema, nullErr, ambiguousErr string) (string, error) {
	if operand.HasNull() {
		return "", errors.New(nullErr)
	}
	t, ok := concreteTypeOf(operand)
	if !ok {
		return "", errors.New(ambiguousErr)
	}
	return t, nil
}

func inferOrderingCmp(left, right Schema) (Schema, error) {
	lt, rt, err := binOperands(left, right, "comparison requires non-nullable operands", "comparison requires an unambiguous operand")
	if err != nil {
		return Schema{}, err
	}
	if !isNumeric(lt) || !isNumeric(rt) {
		return Schema{}, fmt.Errorf("comparison requires numeric operands, got %q and %q", lt, rt)
	}
	return Type("boolean"), nil
}

func inferLogical(left, right Schema) (Schema, error) {
	lt, rt, err := binOperands(left, right, "logical operator requires non-nullable boolean operands", "logical operator requires an unambiguous operand")
	if err != nil {
		return Schema{}, err
	}
	if lt != "boolean" || rt != "boolean" {
		return Schema{}, fmt.Errorf("logical operator requires boolean operands, got %q and %q", lt, rt)
	}
	return Type("boolean"), nil
}

func inferNot(operand Schema) (Schema, error) {
	t, err := unaryOperand(operand, "! requires a non-nullable boolean operand", "! requires an unambiguous operand")
	if err != nil {
		return Schema{}, err
	}
	if t != "boolean" {
		return Schema{}, fmt.Errorf("! requires a boolean operand, got %q", t)
	}
	return Type("boolean"), nil
}

func inferAdd(left, right Schema) (Schema, error) {
	lt, rt, err := binOperands(left, right, "operator requires non-nullable operands", "operator requires an unambiguous operand")
	if err != nil {
		return Schema{}, err
	}
	if lt == "string" && rt == "string" {
		return Type("string"), nil
	}
	return inferArith(left, right)
}

func inferArith(left, right Schema) (Schema, error) {
	lt, rt, err := binOperands(left, right, "operator requires non-nullable operands", "operator requires an unambiguous numeric operand")
	if err != nil {
		return Schema{}, err
	}
	if !isNumeric(lt) || !isNumeric(rt) {
		return Schema{}, fmt.Errorf("operator requires numeric operands, got %q and %q", lt, rt)
	}
	if lt == "integer" && rt == "integer" {
		return Type("integer"), nil
	}
	return Type("number"), nil
}

func inferMod(left, right Schema) (Schema, error) {
	lt, rt, err := binOperands(left, right, "% requires non-nullable operands", "% requires an unambiguous integer operand")
	if err != nil {
		return Schema{}, err
	}
	if lt != "integer" || rt != "integer" {
		return Schema{}, fmt.Errorf("%% requires integer operands, got %q and %q", lt, rt)
	}
	return Type("integer"), nil
}

func inferDiv(left, right Schema) (Schema, error) {
	lt, rt, err := binOperands(left, right, "/ requires non-nullable operands", "/ requires an unambiguous numeric operand")
	if err != nil {
		return Schema{}, err
	}
	if !isNumeric(lt) || !isNumeric(rt) {
		return Schema{}, fmt.Errorf("/ requires numeric operands, got %q and %q", lt, rt)
	}
	return Type("number"), nil
}

func numericPassthrough(operand Schema) (Schema, error) {
	t, err := unaryOperand(operand, "unary operator requires a non-nullable numeric operand", "unary operator requires an unambiguous numeric operand")
	if err != nil {
		return Schema{}, err
	}
	if !isNumeric(t) {
		return Schema{}, fmt.Errorf("unary operator requires a numeric operand, got %q", t)
	}
	return operand, nil
}

// inferNullCoalesce keeps a $ref operand symbolic, which keeps a recursive output type finite;
// refs resolve for analysis only (resolveTolerant).
func inferNullCoalesce(left, right Schema) (Schema, error) {
	if left.IsNull() {
		return right, nil
	}
	nonNullLeft := left.StripNull()
	leftWrapperNullable := !schemasEqual(left, nonNullLeft)

	// Mid-solve, a ref to a definition being computed lands on its estimate — the null seed
	// on the first pass, which must take the `?? default` arm like a structural null.
	analysisLeft := resolveTolerant(nonNullLeft)
	if analysisLeft.IsNull() {
		return right, nil
	}
	if !leftWrapperNullable && !analysisLeft.HasNull() {
		return left, nil // left can never be null; ?? is a no-op
	}
	if !leftWrapperNullable {
		// StripNull leaving a ref means a definition still being SOLVED, served wrapped nullable
		// (estimateNode): the estimate is the type, the wrapper the seed. Only MUTUAL recursion
		// gets here. TestGenerate_MutualOutputRecursionUnwrapsTheEstimate.
		nonNullLeft = analysisLeft.StripNull()
	}
	if schemasEqual(nonNullLeft, right) {
		return nonNullLeft, nil
	}
	if merged, ok := absorbEmptyArray(nonNullLeft, right); ok {
		return merged, nil
	}
	// A numeric accumulator materializes to its scalar so arithmetic on the result works; a
	// non-scalar left keeps the symbolic form below.
	lct, lOK := concreteTypeOf(analysisLeft.StripNull())
	rct, rOK := concreteTypeOf(right)
	if lOK && rOK && isNumeric(lct) && isNumeric(rct) {
		if lct == rct {
			return Type(lct), nil
		}
		return Type("number"), nil
	}
	// Canonicalize or the union is unsatisfiable: `boolean ?? boolean|null` builds overlapping
	// oneOf arms. A $ref arm blocks the merge (isSimpleType), keeping recursion finite.
	return OneOf(nonNullLeft, right).Canonicalize(), nil
}

// absorbEmptyArray: keeping both arms is WRONG — [] matches both and oneOf demands exactly one.
// The survivor returns untouched ($refs stay symbolic).
func absorbEmptyArray(a, b Schema) (Schema, bool) {
	if isProvablyEmpty(b) && resolveTolerant(a).IsType("array") {
		return a, true
	}
	if isProvablyEmpty(a) && resolveTolerant(b).IsType("array") {
		return b, true
	}
	return Schema{}, false
}

func isNumeric(t string) bool {
	return t == "integer" || t == "number"
}

func nullableSchema(a, b Schema) (Schema, bool) {
	if s, ok := tryNullable(a, b); ok {
		return s, true
	}
	return tryNullable(b, a)
}

// tryNullable makes self nullable in place when other is {type:"null"}. Schemas with
// properties are excluded — they need the oneOf wrapper the caller builds.
func tryNullable(self, other Schema) (Schema, bool) {
	if !other.IsNull() {
		return Schema{}, false
	}
	if self.HasNull() {
		return self, true
	}
	if self.HasProperties() {
		return Schema{}, false
	}
	if t := self.Type(); len(t) == 1 && t[0] != "null" {
		return self.WithNull(), true
	}
	return Schema{}, false
}

// resolveTolerant follows a $ref for analysis (mid-solve, to the nullable estimate). A failure
// returns s unchanged; the error surfaces via a look-inside path.
func resolveTolerant(s Schema) Schema {
	if !s.HasRef() {
		return s
	}
	r, err := s.Resolve()
	if err != nil {
		return s
	}
	return r
}

// concreteTypeOf resolves $refs (top-level and per variant) so referenced scalars take part in
// operator typing; an all-numeric union widens to "number".
func concreteTypeOf(s Schema) (string, bool) {
	s = resolveTolerant(s)
	if t := s.Type(); len(t) == 1 {
		return t[0], true
	}
	variants := s.Variants()
	if variants == nil {
		return "", false
	}
	var types []string
	for _, v := range variants {
		if v.IsZero() {
			return "", false
		}
		v = resolveTolerant(v)
		if v.IsNull() {
			return "", false
		}
		vt := v.Type()
		if len(vt) != 1 {
			return "", false
		}
		types = append(types, vt[0])
	}
	if len(types) == 0 {
		return "", false
	}
	if allEqual(types) {
		return types[0], true
	}
	if allSatisfy(types, isNumeric) {
		return "number", true
	}
	return "", false
}

func allEqual(ss []string) bool {
	for _, s := range ss[1:] {
		if s != ss[0] {
			return false
		}
	}
	return true
}

func allSatisfy(ss []string, fn func(string) bool) bool {
	for _, s := range ss {
		if !fn(s) {
			return false
		}
	}
	return true
}

// unwrapSingleVariant simplifies a oneOf/anyOf with exactly one non-null variant into
// that variant directly.
func unwrapSingleVariant(s Schema) Schema {
	variants := s.Variants()
	if variants == nil {
		return s
	}
	var nonNull []Schema
	for _, v := range variants {
		if v.IsZero() || v.IsNull() {
			return s
		}
		nonNull = append(nonNull, v)
	}
	if len(nonNull) == 1 {
		return nonNull[0]
	}
	return s
}

// schemasEqual compares two schemas structurally, ignoring the root $defs each may
// carry — navigation attaches the shared context, but two identical types must compare
// equal whether or not they were reached via navigation.
func schemasEqual(a, b Schema) bool {
	aj, err1 := json.Marshal(a.WithoutDefs())
	bj, err2 := json.Marshal(b.WithoutDefs())
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}
