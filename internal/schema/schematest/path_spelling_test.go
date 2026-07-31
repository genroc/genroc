package schematest

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"genroc/internal/expression/syntax"
	"genroc/internal/schema"
)

// An access path is one spelling everywhere — validation error messages, shape
// labels, guard keys — and that spelling is the expression language's own accessor
// syntax. The point is that a path a user is shown can be pasted back into a
// definition, which for a key that is not an identifier means the bracket form:
// dotting it produces either a parse error (`headers.retry-after` is a
// subtraction) or, worse, a path that silently names something else (`h.x.y` for
// the single key "x.y" reads as the nested x → y).

const pathSpellingSchema = `{
	"type": "object",
	"properties": {
		"headers": {
			"type": "object",
			"properties": {
				"retry-after": {"type": "integer"},
				"x.y":         {"type": "integer"},
				"plain":       {"type": "integer"}
			},
			"required": ["retry-after", "x.y", "plain"]
		},
		"list": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {"a-b": {"type": "integer"}},
				"required": ["a-b"]
			}
		}
	},
	"required": ["headers", "list"]
}`

func TestValidateErrorPathIsAnAccessor(t *testing.T) {
	sc := mustParse(t, pathSpellingSchema)
	cases := []struct{ name, data, wantPrefix string }{
		{"hyphenated_key", `{"headers":{"retry-after":"x","x.y":1,"plain":1},"list":[]}`, `headers["retry-after"]: `},
		{"dotted_key", `{"headers":{"retry-after":1,"x.y":"x","plain":1},"list":[]}`, `headers["x.y"]: `},
		{"identifier_key_stays_dotted", `{"headers":{"retry-after":1,"x.y":1,"plain":"x"},"list":[]}`, `headers.plain: `},
		{"under_an_index", `{"headers":{"retry-after":1,"x.y":1,"plain":1},"list":[{"a-b":"x"}]}`, `list[0]["a-b"]: `},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := sc.Validate(mustData(t, c.data))
			if err == nil {
				t.Fatalf("Validate(%s): expected an error", c.data)
			}
			if !strings.HasPrefix(err.Error(), c.wantPrefix) {
				t.Errorf("error %q does not start with %q", err.Error(), c.wantPrefix)
			}
		})
	}
}

// The path in a message is only useful if it resolves back to the value it named,
// so the rendering and At/SecretAt must agree on the syntax.
func TestErrorPathResolvesBack(t *testing.T) {
	sc := mustParse(t, pathSpellingSchema)
	cases := []struct{ path, want string }{
		{`headers["retry-after"]`, "integer"},
		{`headers["x.y"]`, "integer"},
		{`headers.plain`, "integer"},
		// Nullable because an index may be out of bounds — Index()'s standing
		// behaviour, unrelated to how the key is spelled.
		{`list[0]["a-b"]`, "integer|null"},
	}
	for _, c := range cases {
		at, err := sc.At(c.path)
		if err != nil {
			t.Errorf("At(%q): %v", c.path, err)
			continue
		}
		if got := at.TypeName(); got != c.want {
			t.Errorf("At(%q) type = %q, want %q", c.path, got, c.want)
		}
	}
}

// A dotted path keeps meaning nesting, so every path authored before the bracket
// form existed still resolves the same way.
func TestDottedPathStillNests(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"a": {
				"type": "object",
				"properties": {"b": {"type": "string"}},
				"required": ["b"]
			},
			"a.b": {"type": "integer"}
		},
		"required": ["a", "a.b"]
	}`)
	nested, err := sc.At("a.b")
	if err != nil {
		t.Fatalf(`At("a.b"): %v`, err)
	}
	if got := nested.TypeName(); got != "string" {
		t.Errorf(`At("a.b") type = %q, want string (the nested a → b)`, got)
	}
	flat, err := sc.At(`["a.b"]`)
	if err != nil {
		t.Fatalf(`At(["a.b"]): %v`, err)
	}
	if got := flat.TypeName(); got != "integer" {
		t.Errorf(`At(["a.b"]) type = %q, want integer (the flat key)`, got)
	}
}

func TestSecretAtDistinguishesQuotedKey(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"a": {
				"type": "object",
				"properties": {"b": {"type": "string"}},
				"required": ["b"]
			},
			"a.b": {"type": "string", "secret": true}
		},
		"required": ["a", "a.b"]
	}`)
	if !sc.SecretAt(`["a.b"]`) {
		t.Error(`SecretAt(["a.b"]) = false, want true (the flat key is the secret)`)
	}
	if sc.SecretAt("a.b") {
		t.Error(`SecretAt("a.b") = true, want false (the nested a → b is not secret)`)
	}
}

// The whole promise in one test, over keys chosen to break it: the path a
// validation error reports must (a) resolve back to the node it named, and (b) be
// a valid expression yielding that same key. Escaping is what makes this hold —
// the renderer quotes with strconv.Quote, and expr-lang's lexer reads that dialect
// back, including \" \\ \t \n \xNN and \uNNNN.
func TestErrorPathSurvivesAdversarialKeys(t *testing.T) {
	keys := []string{
		`with space`,
		`say "hi"`,
		`it's`,
		`back\slash`,
		"tab\there",
		"nl\nhere",
		"\x01ctl",    // renders as \x01
		"\u0085nel",  // NEL - non-printable, so strconv.Quote escapes it
		"\u200bzwsp", // zero-width space: invisible in source, must survive anyway
		`café`,       // printable non-ASCII: kept literal
		`emoji 🎉`,
		`a.b`,    // would read as nesting if dotted
		`a["b"]`, // the accessor syntax itself, as a key
		`has]bracket`,
		`0start`, // not an identifier: leading digit
		``,       // the empty key
		`plain`,  // the control: an identifier, stays dotted
	}
	for _, key := range keys {
		t.Run(strconv.Quote(key), func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"type":       "object",
				"properties": map[string]any{key: map[string]any{"type": "integer"}},
				"required":   []string{key},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			sc, err := schema.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			nsc := sc.AssumeNormalized()

			_, verr := nsc.Validate(map[string]any{key: "not an integer"})
			if verr == nil {
				t.Fatal("expected a validation error")
			}
			path := strings.TrimSuffix(verr.Error(), ": expected type integer, got string")
			if path == verr.Error() {
				t.Fatalf("unexpected error shape: %v", verr)
			}

			// (a) the path names the node the error was about.
			back, err := nsc.At(path)
			if err != nil {
				t.Fatalf("At(%s): %v", path, err)
			}
			if got := back.TypeName(); got != "integer" {
				t.Errorf("At(%s) type = %q, want integer", path, got)
			}

			// (b) the path is an accessor an author can write.
			expr := "input." + path
			if strings.HasPrefix(path, "[") {
				expr = "input" + path
			}
			n, err := syntax.Parse(expr)
			if err != nil {
				t.Fatalf("Parse(%s): %v", expr, err)
			}
			m, ok := n.(*syntax.MemberNode)
			if !ok {
				t.Fatalf("Parse(%s) = %T, want *MemberNode", expr, n)
			}
			if m.Name != key {
				t.Errorf("Parse(%s) key = %q, want %q", expr, m.Name, key)
			}
		})
	}
}

func TestPathParseErrors(t *testing.T) {
	sc := mustParse(t, pathSpellingSchema)
	for _, path := range []string{
		``,
		`.headers`,
		`headers..plain`,
		`headers.`,
		`headers["x.y"`,      // unterminated quote
		`headers["x.y"plain`, // no ']' after the key
		`list[x]`,            // non-integer, unquoted
	} {
		if _, err := sc.At(path); err == nil {
			t.Errorf("At(%q): expected an error, got none", path)
		}
	}
}
