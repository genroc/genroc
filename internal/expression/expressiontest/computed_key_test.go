package expressiontest

import "testing"

// A computed key a[expr] is admitted exactly where the answer cannot depend on which key is
// read: an array, or a map declaring only additionalProperties. On an object with declared
// properties it stays an error — the restriction the whole feature rests on.

const computedKeyJSON = `{
	"type": "object",
	"properties": {
		"headers": {
			"type": "object",
			"additionalProperties": { "type": "string" }
		},
		"counts": {
			"type": "object",
			"additionalProperties": { "type": "integer" }
		},
		"vault": {
			"type": "object",
			"additionalProperties": { "type": "string", "secret": true }
		},
		"mixed": {
			"type": "object",
			"properties": { "known": { "type": "integer" } },
			"required": ["known"],
			"additionalProperties": { "type": "string" }
		},
		"closed": {
			"type": "object",
			"properties": { "a": { "type": "integer" } },
			"required": ["a"]
		},
		"tags": { "type": "array", "items": { "type": "string" } },
		"opaque": {},
		"k": { "type": "string" },
		"i": { "type": "integer" },
		"maybeKey": { "anyOf": [{ "type": "string" }, { "type": "null" }] }
	},
	"required": ["headers", "counts", "vault", "mixed", "closed", "tags", "opaque", "k", "i", "maybeKey"]
}`

func TestComputedKey_Infers(t *testing.T) {
	c := ctx(t, computedKeyJSON)
	// Nullable for the same reason an index is: the key may be absent.
	assertSchema(t, infer(t, `headers[k]`, c), `{"type": ["string", "null"]}`)
	assertSchema(t, infer(t, `counts[k]`, c), `{"type": ["integer", "null"]}`)
	assertSchema(t, infer(t, `tags[i]`, c), `{"type": ["string", "null"]}`)
	// The key may be any expression of the right type.
	assertSchema(t, infer(t, `headers[k + "-suffix"]`, c), `{"type": ["string", "null"]}`)
	assertSchema(t, infer(t, `tags[i + 1]`, c), `{"type": ["string", "null"]}`)
	// Chained.
	assertSchema(t, infer(t, `headers[k] ?? "none"`, c), `{"type": "string"}`)
}

func TestComputedKey_RejectedWhereTypeVariesPerKey(t *testing.T) {
	c := ctx(t, computedKeyJSON)
	// The soundness case: a computed key could land on `known` (integer) while the
	// additionalProperties answer says string.
	inferErr(t, `mixed[k]`, c, "declares named properties")
	inferErr(t, `closed[k]`, c, "declares named properties")
	inferErr(t, `opaque[k]`, c, "the value is unknown")
	inferErr(t, `k[k]`, c, "not an array or a map")
}

func TestComputedKey_KeyTypeMustMatchTheBase(t *testing.T) {
	c := ctx(t, computedKeyJSON)
	inferErr(t, `headers[i]`, c, "a computed key must be string, got integer")
	inferErr(t, `tags[k]`, c, "a computed key must be integer, got string")
	// Strict: a nullable key must be narrowed or defaulted first.
	inferErr(t, `headers[maybeKey]`, c, "a computed key must be string, got string|null")
	assertSchema(t, infer(t, `headers[maybeKey ?? ""]`, c), `{"type": ["string", "null"]}`)
}

// The hole a path-based secret walk cannot see: the taint sits on
// additionalProperties, never on the map itself, so a computed key has to read it
// off the resolved type. `vault` alone is not secret — that is what makes this a
// leak rather than a redundancy.
func TestComputedKey_SecretsOnTheValueSchema(t *testing.T) {
	c := ctx(t, computedKeyJSON)
	secretRefAll(t, c, true, []secretCase{
		{"computed_key_into_a_secret_map", `vault[k]`},
		{"computed_key_with_an_operator", `vault[k + "x"]`},
		{"tainted_result_used", `vault[k] ?? "none"`},
	})
	secretRefAll(t, c, false, []secretCase{
		{"the_map_itself_is_not_secret", `vault`},
		{"a_plain_map", `headers[k]`},
	})
}

// Building the key can read a secret in its own right.
func TestComputedKey_SecretInTheKeyExpression(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"headers": { "type": "object", "additionalProperties": { "type": "string" } },
			"which": { "type": "string", "secret": true }
		},
		"required": ["headers", "which"]
	}`)
	secretRefAll(t, c, true, []secretCase{{"secret_key", `headers[which]`}})
}

// Narrowing keys on the key expression, so two reads of the same a[k] share a
// guard. Sound because expressions are pure: the second read cannot see a
// different key than the first.
func TestComputedKey_Narrowing(t *testing.T) {
	c := ctx(t, computedKeyJSON)
	assertSchema(t, infer(t, `headers[k] != null ? headers[k] + "!" : "none"`, c), `{"type": "string"}`)
	assertSchema(t, infer(t, `headers[k] == null ? "none" : headers[k] + "!"`, c), `{"type": "string"}`)
	assertSchema(t, infer(t, `tags[i] != null ? tags[i] + "!" : "none"`, c), `{"type": "string"}`)
}

// …and only the same key. A guard on a[k] says nothing about a[j].
func TestComputedKey_NarrowingIsPerKeyExpression(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"m": { "type": "object", "additionalProperties": { "type": "string" } },
			"k": { "type": "string" },
			"j": { "type": "string" }
		},
		"required": ["m", "k", "j"]
	}`)
	assertSchema(t, infer(t, `m[k] != null ? m[k] + "!" : "none"`, c), `{"type": "string"}`)
	inferErr(t, `m[k] != null ? m[j] + "!" : "none"`, c, "")
}

// A key built by an operator is not a static path, so it gets no guard. That costs
// convenience, never soundness — and `??` still covers it.
func TestComputedKey_NoNarrowingForAComputedKeyExpression(t *testing.T) {
	c := ctx(t, computedKeyJSON)
	inferErr(t, `headers[k + "x"] != null ? headers[k + "x"] + "!" : "none"`, c, "")
	assertSchema(t, infer(t, `(headers[k + "x"] ?? "") + "!"`, c), `{"type": "string"}`)
}

// The shadowing case: a guard on m[k] must die when a lambda rebinds k, or the
// narrowing would be carried onto a different value entirely.
func TestComputedKey_NarrowingDroppedWhenTheKeyIsShadowed(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"m": { "type": "object", "additionalProperties": { "type": "string" } },
			"k": { "type": "string" },
			"xs": { "type": "array", "items": { "type": "string" } }
		},
		"required": ["m", "k", "xs"]
	}`)
	// Outside the lambda the guard holds.
	assertSchema(t, infer(t, `m[k] != null ? m[k] + "!" : "none"`, c), `{"type": "string"}`)
	// Inside, k is the lambda's parameter — a different value, so m[k] is nullable
	// again and the arithmetic must be rejected.
	inferErr(t, `m[k] != null ? map(xs, k => m[k] + "!") : []`, c, "")
	// The same lambda works once the inner read is defaulted.
	assertSchema(t, infer(t, `map(xs, k => (m[k] ?? "") + "!")`, c),
		`{"type": "array", "items": {"type": "string"}}`)
}

func TestComputedKey_Evaluates(t *testing.T) {
	context := map[string]any{
		"headers": map[string]any{"retry-after": "30", "x": "y"},
		"tags":    []any{"a", "b", "c"},
		"k":       "retry-after",
		"i":       1,
	}
	assertEq(t, evalOK(t, `headers[k]`, context), "30")
	assertEq(t, evalOK(t, `tags[i]`, context), "b")
	assertEq(t, evalOK(t, `tags[i + 1]`, context), "c")
	// An absent key and an out-of-range index are null, matching literal access.
	assertEq(t, evalOK(t, `headers["nope"]`, context), nil)
	assertEq(t, evalOK(t, `tags[99]`, context), nil)
	assertEq(t, evalOK(t, `tags[i + 99]`, context), nil)
	assertEq(t, evalOK(t, `tags[0 - 1]`, context), nil)
	// A null base short-circuits to null rather than erroring.
	assertEq(t, evalOK(t, `missing[k]`, map[string]any{"missing": nil, "k": "a"}), nil)
}

// A key whose runtime type contradicts the schema is an error, not a silent null:
// inference already established that it types, so reaching this means the data lied.
func TestComputedKey_WrongKeyTypeAtRuntimeErrors(t *testing.T) {
	context := map[string]any{
		"headers": map[string]any{"a": "1"},
		"tags":    []any{"a"},
		"num":     1,
		"str":     "x",
	}
	if err := evalErr(t, `headers[num]`, context); err == nil {
		t.Error("expected an error for a non-string key into a map")
	}
	if err := evalErr(t, `tags[str]`, context); err == nil {
		t.Error("expected an error for a non-integer index into an array")
	}
}
