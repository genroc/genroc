package expressiontest

import "testing"

// a["b"] exists so a key not spellable as an identifier is reachable at all; it desugars to the
// dot form's node, so what matters is not parsing but the two places a key is carried as data —
// secret tainting and narrowing. Both used to flatten to a dot-path (a["x.y"] ≡ a.x.y).

// subscriptContextJSON pairs, twice over, a flat "x.token" property with a
// nested x → token of the opposite secrecy. Each pair is a collision that a
// dot-path key resolves the wrong way, in one direction each.
const subscriptContextJSON = `{
	"type": "object",
	"properties": {
		"headers": {
			"type": "object",
			"properties": {
				"retry-after": { "type": "string" },
				"content-type": { "type": "string" }
			},
			"required": ["retry-after", "content-type"]
		},
		"leaky": {
			"type": "object",
			"properties": {
				"x.token": { "type": "string", "secret": true },
				"x": {
					"type": "object",
					"properties": { "token": { "type": "string" } },
					"required": ["token"]
				}
			},
			"required": ["x.token", "x"]
		},
		"over": {
			"type": "object",
			"properties": {
				"x.token": { "type": "string" },
				"x": {
					"type": "object",
					"properties": { "token": { "type": "string", "secret": true } },
					"required": ["token"]
				}
			},
			"required": ["x.token", "x"]
		}
	},
	"required": ["headers", "leaky", "over"]
}`

func TestStringSubscript_Infers(t *testing.T) {
	c := ctx(t, subscriptContextJSON)
	assertSchema(t, infer(t, `headers["retry-after"]`, c), `{"type": "string"}`)
	assertSchema(t, infer(t, `headers["content-type"] + "!"`, c), `{"type": "string"}`)
	// The dot form of the same key, where one exists, is the same access.
	assertSchema(t, infer(t, `leaky["x"]["token"]`, c), `{"type": "string"}`)
}

func TestStringSubscript_UnknownKeyRejected(t *testing.T) {
	c := ctx(t, subscriptContextJSON)
	inferErr(t, `headers["no-such-header"]`, c, "")
}

func TestStringSubscript_Evaluates(t *testing.T) {
	context := map[string]any{
		"headers": map[string]any{"retry-after": "30"},
		"leaky":   map[string]any{"x.token": "flat", "x": map[string]any{"token": "nested"}},
	}
	assertEq(t, evalOK(t, `headers["retry-after"]`, context), "30")
	assertEq(t, evalOK(t, `leaky["x.token"]`, context), "flat")
	assertEq(t, evalOK(t, `leaky.x.token`, context), "nested")
}

// The regression the steps refactor exists for. Under a dot-path key, reading
// leaky["x.token"] navigated leaky → x → token — a different, non-secret value —
// and the secret went out unredacted.
func TestStringSubscript_DottedKeyTaints(t *testing.T) {
	c := ctx(t, subscriptContextJSON)
	secretRefAll(t, c, true, []secretCase{
		{"flat_dotted_key_is_the_secret", `leaky["x.token"]`},
		{"nested_path_is_the_secret", `over.x.token`},
		{"nested_path_spelled_with_subscripts", `over["x"]["token"]`},
	})
	secretRefAll(t, c, false, []secretCase{
		// The non-secret half of each pair: neither may borrow the other's taint.
		{"nested_sibling_of_a_flat_secret", `leaky.x.token`},
		{"flat_sibling_of_a_nested_secret", `over["x.token"]`},
	})
}

// "" is a legal JSON key, and a["") is the only way to reach it. It is also the
// case that made a path step need an explicit index/property discriminator: an
// empty prop is indistinguishable from index 0 under the prop != "" test.
func TestStringSubscript_EmptyKey(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"m": {
				"type": "object",
				"properties": { "": { "type": "string", "secret": true } },
				"required": [""]
			},
			"arr": { "type": "array", "items": { "type": "integer" } }
		},
		"required": ["m", "arr"]
	}`)
	assertSchema(t, infer(t, `m[""]`, c), `{"type": "string", "secret": true}`)
	// Not read as index 0: m is an object, which has no element type.
	inferErr(t, `m[0]`, c, "")
	// …and the converse, so the discriminator is exercised both ways.
	inferErr(t, `arr[""]`, c, "")

	secretRefAll(t, c, true, []secretCase{{"empty_key_secret", `m[""]`}})
}

// Narrowing keys off the same access path, so the same collision applies: a
// guard on the flat key must not narrow the nested one.
const subscriptNarrowJSON = `{
	"type": "object",
	"properties": {
		"g": {
			"type": "object",
			"properties": {
				"x.v": { "anyOf": [{"type": "integer"}, {"type": "null"}] },
				"x": {
					"type": "object",
					"properties": { "v": { "anyOf": [{"type": "integer"}, {"type": "null"}] } },
					"required": ["v"]
				}
			},
			"required": ["x.v", "x"]
		}
	},
	"required": ["g"]
}`

func TestStringSubscript_NarrowingIsPerKey(t *testing.T) {
	c := ctx(t, subscriptNarrowJSON)

	// A guard narrows the path it actually names, whichever spelling.
	assertSchema(t, infer(t, `g["x.v"] == null ? 0 : g["x.v"] + 1`, c), `{"type": "integer"}`)
	assertSchema(t, infer(t, `g.x.v == null ? 0 : g.x.v + 1`, c), `{"type": "integer"}`)

	// …and only that path: g.x.v is still nullable in the else branch, so the
	// arithmetic must be rejected rather than silently type-checking.
	inferErr(t, `g["x.v"] == null ? 0 : g.x.v + 1`, c, "")
	inferErr(t, `g.x.v == null ? 0 : g["x.v"] + 1`, c, "")
}
