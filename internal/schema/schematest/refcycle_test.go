// Tests for recursive-$defs handling: CheckDoc productivity, the conform cycle
// guard, ref-aware joins, and secret taint carried on $ref nodes.
// See specs/recursive-type-inference.md.
package schematest

import (
	"strings"
	"testing"

	"genroc/internal/schema"
)

// linkedListJSON is a productive recursive schema: every cycle passes through
// properties, so each unrolling consumes one level of the value.
const linkedListJSON = `{
	"$ref": "#/$defs/list",
	"$defs": {
		"list": {
			"type": "object",
			"properties": {
				"value": {"type": "integer"},
				"next":  {"oneOf": [{"$ref": "#/$defs/list"}, {"type": "null"}]}
			},
			"required": ["value", "next"]
		}
	}
}`

func TestCheckDocAcceptsProductiveRecursion(t *testing.T) {
	sc := mustParse(t, linkedListJSON)
	if err := sc.CheckDoc(); err != nil {
		t.Fatalf("CheckDoc rejected a productive recursive schema: %v", err)
	}
}

func TestCheckDocRejectsBareSelfCycle(t *testing.T) {
	sc := mustParse(t, `{
		"$ref": "#/$defs/x",
		"$defs": {
			"x": {"oneOf": [{"$ref": "#/$defs/x"}, {"type": "string"}]}
		}
	}`)
	err := sc.CheckDoc()
	if err == nil {
		t.Fatal("CheckDoc accepted a bare self-cycle (x = oneOf[$ref x, string])")
	}
	if !strings.Contains(err.Error(), "without structural progress") {
		t.Errorf("error %q does not mention structural progress", err)
	}
}

func TestCheckDocRejectsMutualBareCycle(t *testing.T) {
	sc := mustParse(t, `{
		"$ref": "#/$defs/a",
		"$defs": {
			"a": {"anyOf": [{"$ref": "#/$defs/b"}, {"type": "string"}]},
			"b": {"anyOf": [{"$ref": "#/$defs/a"}, {"type": "integer"}]}
		}
	}`)
	err := sc.CheckDoc()
	if err == nil {
		t.Fatal("CheckDoc accepted a mutual bare cycle a -> b -> a")
	}
	if !strings.Contains(err.Error(), "a -> b -> a") && !strings.Contains(err.Error(), "b -> a -> b") {
		t.Errorf("error %q does not spell out the cycle", err)
	}
}

func TestCheckDocAcceptsMutualProductiveRecursion(t *testing.T) {
	// a and b reference each other, but only under properties — a tree of
	// alternating node kinds. Legal.
	sc := mustParse(t, `{
		"$ref": "#/$defs/a",
		"$defs": {
			"a": {"type": "object", "properties": {"b": {"oneOf": [{"$ref": "#/$defs/b"}, {"type": "null"}]}}, "required": ["b"]},
			"b": {"type": "object", "properties": {"a": {"oneOf": [{"$ref": "#/$defs/a"}, {"type": "null"}]}}, "required": ["a"]}
		}
	}`)
	if err := sc.CheckDoc(); err != nil {
		t.Fatalf("CheckDoc rejected mutual productive recursion: %v", err)
	}
}

func TestCheckDocAcceptsSharedAcyclicRefs(t *testing.T) {
	// Diamond-shaped reuse without any cycle stays legal, including bare
	// union-position refs.
	sc := mustParse(t, `{
		"$ref": "#/$defs/top",
		"$defs": {
			"top":  {"oneOf": [{"$ref": "#/$defs/leaf"}, {"type": "null"}]},
			"leaf": {"type": "string"}
		}
	}`)
	if err := sc.CheckDoc(); err != nil {
		t.Fatalf("CheckDoc rejected acyclic shared refs: %v", err)
	}
}

// ---- conform (Validate) on recursive schemas ----

func TestValidateProductiveRecursiveValue(t *testing.T) {
	sc := mustParse(t, linkedListJSON)

	got, err := sc.Validate(map[string]any{
		"value": 1,
		"next": map[string]any{
			"value": 2,
			"next":  nil,
		},
	})
	if err != nil {
		t.Fatalf("Validate rejected a valid linked list: %v", err)
	}
	if got == nil {
		t.Fatal("Validate returned nil for a valid list")
	}

	if _, err := sc.Validate(map[string]any{"value": 1, "next": 5}); err == nil {
		t.Error("Validate accepted next=5 (neither list nor null)")
	}
	if _, err := sc.Validate(map[string]any{"value": 1, "next": map[string]any{"value": "x", "next": nil}}); err == nil {
		t.Error("Validate accepted a nested node with a string value")
	}
}

func TestValidateDegenerateCycleErrorsInsteadOfHanging(t *testing.T) {
	// Load bypasses CheckDoc, mimicking a degenerate schema decoded from
	// storage. The validator must fail the cyclic branch, not spin.
	sc := schema.Load(map[string]any{
		"$ref": "#/$defs/x",
		"$defs": map[string]any{
			"x": map[string]any{
				"oneOf": []any{map[string]any{"$ref": "#/$defs/x"}},
			},
		},
	})
	if _, err := sc.Validate("anything"); err == nil {
		t.Fatal("Validate accepted a value against x = oneOf[$ref x]")
	}
}

func TestValidateDegenerateCycleStillMatchesConcreteBranch(t *testing.T) {
	// The cyclic branch fails, but a concrete sibling branch can still match:
	// x = oneOf[$ref x, string] accepts "abc" via the string arm.
	sc := schema.Load(map[string]any{
		"$ref": "#/$defs/x",
		"$defs": map[string]any{
			"x": map[string]any{
				"oneOf": []any{
					map[string]any{"$ref": "#/$defs/x"},
					map[string]any{"type": "string"},
				},
			},
		},
	})
	got, err := sc.Validate("abc")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != "abc" {
		t.Errorf("got %v, want %q", got, "abc")
	}
}

// ---- IsSubset on recursive schemas (coinductive) ----

func TestIsSubsetRecursiveSelf(t *testing.T) {
	a := mustParse(t, linkedListJSON)
	b := mustParse(t, linkedListJSON)
	if !a.IsSubset(b) {
		t.Error("a recursive schema is not a subset of itself")
	}
}

// ---- Join with $refs ----

func TestJoinIdenticalRefs(t *testing.T) {
	a := schema.Ref("x")
	if got := a.Join(schema.Ref("x")); !got.Equal(a.Canonicalize()) {
		t.Errorf("join($ref x, $ref x) = %s, want $ref x", mustJSON(t, got))
	}
}

func TestJoinRefWithItsNullableForm(t *testing.T) {
	ref := schema.Ref("x")
	nullable := ref.WithNull()
	got := nullable.Join(ref)
	if !got.Equal(nullable.Canonicalize()) {
		t.Errorf("join(nullable $ref x, $ref x) = %s, want the nullable form", mustJSON(t, got))
	}
	// And joining twice is stable (no flapping between forms).
	again := got.Join(ref)
	if !again.Equal(got) {
		t.Errorf("join is not stable: %s then %s", mustJSON(t, got), mustJSON(t, again))
	}
}

func TestJoinDistinctRefsUnions(t *testing.T) {
	got := schema.Ref("x").Join(schema.Ref("y"))
	variants := got.Variants()
	if len(variants) != 2 {
		t.Fatalf("join($ref x, $ref y) has %d variants, want 2: %s", len(variants), mustJSON(t, got))
	}
	if !variants[0].HasRef() || !variants[1].HasRef() {
		t.Errorf("join($ref x, $ref y) lost a ref: %s", mustJSON(t, got))
	}
}

// ---- secret taint on $ref nodes ----

// taintedRefJSON marks the *reference* to t as secret; the shared definition
// itself stays clean (tainting it would over-taint other users).
const taintedRefJSON = `{
	"type": "object",
	"properties": {
		"token": {"$ref": "#/$defs/t", "secret": true},
		"plain": {"$ref": "#/$defs/t"}
	},
	"required": ["token", "plain"],
	"$defs": {
		"t": {"type": "string"}
	}
}`

func TestSecretOnRefNode(t *testing.T) {
	sc := mustParse(t, taintedRefJSON)

	if !sc.SecretAt("token") {
		t.Error("SecretAt(token) = false, want true (taint on the $ref node)")
	}
	if sc.SecretAt("plain") {
		t.Error("SecretAt(plain) = true; the shared definition must stay clean")
	}

	data := map[string]any{"token": "s3cret", "plain": "visible"}
	redacted, ok := sc.Redact(data).(map[string]any)
	if !ok {
		t.Fatalf("Redact returned %T", sc.Redact(data))
	}
	if redacted["token"] != "***" {
		t.Errorf("token = %v, want ***", redacted["token"])
	}
	if redacted["plain"] != "visible" {
		t.Errorf("plain = %v, want untouched", redacted["plain"])
	}

	secrets := sc.CollectSecrets(data)
	if len(secrets) != 1 || secrets[0] != "s3cret" {
		t.Errorf("CollectSecrets = %v, want [s3cret]", secrets)
	}
}

func TestTaintOnRefSchema(t *testing.T) {
	tainted := schema.Ref("t").Taint()
	if !tainted.IsSecret() {
		t.Error("Taint on a $ref schema is not reported secret")
	}
	if !tainted.HasRef() {
		t.Error("Taint dropped the $ref")
	}
}

func mustJSON(t *testing.T, s schema.Schema) string {
	t.Helper()
	b, err := s.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
