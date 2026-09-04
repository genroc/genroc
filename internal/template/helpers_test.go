package template

import (
	"fmt"
	"strings"
	"testing"

	"genroc/internal/schema"
)

// describe renders a parsed template's chunks so split behaviour can be asserted
// directly: LIT for literal text, EXPR for a parsed expression.
func describe(t *Template) string {
	parts := make([]string, 0, len(t.chunks))
	for _, c := range t.chunks {
		kind := "LIT"
		if c.node != nil {
			kind = "EXPR"
		}
		parts = append(parts, fmt.Sprintf("%s(%q)", kind, c.text))
	}
	return strings.Join(parts, " ")
}

func mustParse(t *testing.T, s string) *Template {
	t.Helper()
	tmpl, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return tmpl
}

// assertSplit checks how src was cut into chunks, rendered by describe.
func assertSplit(t *testing.T, src, want string) {
	t.Helper()
	if got := describe(mustParse(t, src)); got != want {
		t.Errorf("Parse(%q)\n got: %s\nwant: %s", src, got, want)
	}
}

func assertParseError(t *testing.T, src, wantContains string) {
	t.Helper()
	_, err := Parse(src)
	if err == nil {
		t.Fatalf("Parse(%q): expected error, got nil", src)
	}
	if !strings.Contains(err.Error(), wantContains) {
		t.Errorf("Parse(%q): error %q does not contain %q", src, err, wantContains)
	}
}

// evalCtx is the context every EvalAny assertion runs against.
var evalCtx = map[string]any{
	"input": map[string]any{"name": "ann", "n": 3, "ok": true},
}

func assertEvalAny(t *testing.T, src string, want any) {
	t.Helper()
	got, err := mustParse(t, src).EvalAny(evalCtx)
	if err != nil {
		t.Fatalf("EvalAny(%q): %v", src, err)
	}
	if got != want {
		t.Errorf("EvalAny(%q) = %#v, want %#v", src, got, want)
	}
}

func ctxSchema(t *testing.T) schema.Schema {
	t.Helper()
	sc, err := schema.Parse([]byte(`{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"name": {"type": "string"},
					"n": {"type": "integer"},
					"opt": {"type": ["string", "null"]},
					"token": {"type": "string", "secret": true},
					"tags": {"type": "array", "items": {"type": "string"}},
					"people": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"name": {"type": "string"},
								"secret": {"type": "string", "secret": true}
							},
							"required": ["name", "secret"]
						}
					}
				},
				"required": ["name", "n", "opt", "token", "tags", "people"]
			}
		},
		"required": ["input"]
	}`))
	if err != nil {
		t.Fatalf("context schema: %v", err)
	}
	return sc.AssumeNormalized()
}

func assertInferType(t *testing.T, src, want string) {
	t.Helper()
	got, err := mustParse(t, src).InferType(ctxSchema(t))
	if err != nil {
		t.Fatalf("InferType(%q): %v", src, err)
	}
	if !got.IsType(want) {
		t.Errorf("InferType(%q) = %q, want %q", src, got.TypeName(), want)
	}
}

func assertInferRejects(t *testing.T, src string) {
	t.Helper()
	if _, err := mustParse(t, src).InferType(ctxSchema(t)); err == nil {
		t.Errorf("InferType(%q): expected rejection, got nil", src)
	}
}

func assertInferAccepts(t *testing.T, src string) {
	t.Helper()
	if _, err := mustParse(t, src).InferType(ctxSchema(t)); err != nil {
		t.Errorf("InferType(%q): unexpected rejection: %v", src, err)
	}
}
