package shape_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"genroc/internal/expression"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

func mustShapeVal(t *testing.T, jsonStr string) any {
	t.Helper()
	var s shape.Shape
	if err := json.Unmarshal([]byte(jsonStr), &s); err != nil {
		t.Fatalf("unmarshal shape %s: %v", jsonStr, err)
	}
	return s.Raw
}

func mustSchema(t *testing.T, jsonStr string) schema.Schema {
	t.Helper()
	raw, err := schema.Parse([]byte(jsonStr))
	if err != nil {
		t.Fatalf("parse schema %s: %v", jsonStr, err)
	}
	return raw.AssumeNormalized()
}

func schemaPtr(t *testing.T, jsonStr string) *schema.Schema {
	s := mustSchema(t, jsonStr)
	return &s
}

func assertSchemaJSON(t *testing.T, got schema.Schema, wantJSON string) {
	t.Helper()
	raw, _ := json.Marshal(got)
	var g, w any
	json.Unmarshal(raw, &g)
	if err := json.Unmarshal([]byte(wantJSON), &w); err != nil {
		t.Fatalf("bad wantJSON: %v", err)
	}
	ga, _ := json.MarshalIndent(g, "", "  ")
	wa, _ := json.MarshalIndent(w, "", "  ")
	if string(ga) != string(wa) {
		t.Errorf("schema mismatch:\n got:  %s\n want: %s", ga, wa)
	}
}

// ctxSchema declares the roots: input.n (integer), input.name (string), input.list
// (array<string>). It is a plain object schema — its properties are the roots.
func ctxSchema(t *testing.T) schema.Schema {
	return mustSchema(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"n":    {"type": "integer"},
					"name": {"type": "string"},
					"list": {"type": "array", "items": {"type": "string"}},
					"opt":  {"type": ["string", "null"]}
				},
				"required": ["n", "name", "list"]
			}
		},
		"required": ["input"]
	}`)
}

// Check (phase 1) infers the output type against the context schema; with no required
// Schema it is a free projection and the inferred type is the whole answer.
func TestShape_Check_InfersOutputType(t *testing.T) {
	sh := shape.Shape{
		Raw:  mustShapeVal(t, `{"tags": "$: input.list", "double": "$: input.n * 2", "label": "hi ${ input.name }"}`),
		Name: "out",
	}
	got, err := sh.Check(ctxSchema(t))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	assertSchemaJSON(t, got, `{
		"type": "object",
		"properties": {
			"double": {"type": "integer"},
			"label":  {"type": "string"},
			"tags":   {"type": "array", "items": {"type": "string"}}
		},
		"required": ["double", "label", "tags"]
	}`)
}

// Check enforces conformance to a declared required Schema.
func TestShape_Check_ConformanceGate(t *testing.T) {
	// Conforms: {double: integer} ⊆ {double: number}.
	ok := shape.Shape{
		Raw:    mustShapeVal(t, `{"double": "$: input.n * 2"}`),
		Schema: schemaPtr(t, `{"type":"object","properties":{"double":{"type":"number"}},"required":["double"]}`),
		Name:   "ok",
	}
	if _, err := ok.Check(ctxSchema(t)); err != nil {
		t.Errorf("conforming shape rejected: %v", err)
	}

	// Violates: a string leaf cannot satisfy an integer-typed slot.
	bad := shape.Shape{
		Raw:    mustShapeVal(t, `{"double": "hi ${ input.name }"}`),
		Schema: schemaPtr(t, `{"type":"object","properties":{"double":{"type":"integer"}},"required":["double"]}`),
		Name:   "bad",
	}
	if _, err := bad.Check(ctxSchema(t)); err == nil {
		t.Error("non-conforming shape accepted; want a conformance error")
	}
}

// Check rejects an expression that references a root the context schema does not declare.
func TestShape_Check_RejectsUndeclaredRoot(t *testing.T) {
	sh := shape.Shape{Raw: mustShapeVal(t, `"$: bogus.x"`), Name: "root"}
	if _, err := sh.Check(ctxSchema(t)); err == nil {
		t.Error("reference to undeclared root accepted; want an error")
	}
}

// Eval (phase 2) computes the concrete structure from runtime data, independent of any
// schema: a template stringifies, a $: leaf keeps its type, scalars pass through.
func TestShape_Eval_ComputesStructure(t *testing.T) {
	sh := shape.Shape{
		Raw: mustShapeVal(t, `{"n": 5, "double": "$: input.n * 2", "label": "hi ${ input.name }", "tags": ["a", "$: input.name"]}`),
	}
	ctxData := map[string]any{"input": map[string]any{"n": 3, "name": "ann"}}
	got, err := sh.Eval(ctxData)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	m := got.(map[string]any)
	// Arithmetic yields a json.Number (precision-preserving), a bare reference its raw
	// type — both are numerically 6; compare by string.
	if fmt.Sprint(m["double"]) != "6" {
		t.Errorf("double = %#v, want 6", m["double"])
	}
	if m["label"] != "hi ann" {
		t.Errorf("label = %#v, want %q", m["label"], "hi ann")
	}
	if m["n"] != float64(5) {
		t.Errorf("n = %#v, want float64 5", m["n"])
	}
	tags := m["tags"].([]any)
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "ann" {
		t.Errorf("tags = %#v, want [a ann]", tags)
	}
}

// The two phases compose: a shape that Checks clean also Evals against matching data.
func TestShape_TwoPhase(t *testing.T) {
	sh := shape.Shape{
		Raw:    mustShapeVal(t, `{"greeting": "hello ${ input.name }", "count": "$: input.n"}`),
		Schema: schemaPtr(t, `{"type":"object","properties":{"greeting":{"type":"string"},"count":{"type":"integer"}},"required":["greeting","count"]}`),
		Name:   "greet",
	}
	if _, err := sh.Check(ctxSchema(t)); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got, err := sh.Eval(map[string]any{"input": map[string]any{"name": "ann", "n": 7}})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	m := got.(map[string]any)
	if m["greeting"] != "hello ann" || m["count"] != 7 {
		t.Errorf("got %#v, want {greeting: hello ann, count: 7}", m)
	}
}

// The Result hook receives the inferred + required schemas on a conformance failure and
// inspects them (HasNull, TypeName) to craft a tailored message — the over/url case, which
// is about the shape's RESULT, not a root.
func TestShape_CheckWith_ResultHookCraftsMessage(t *testing.T) {
	overHook := shape.CheckHooks{Result: func(inferred, required schema.Schema) error {
		if inferred.HasNull() {
			return errors.New("over may be null; use ?? to provide a default array")
		}
		return fmt.Errorf("over must evaluate to an array, got %q", inferred.TypeName())
	}}

	// Wrong type: a string result against a required array → the "must be an array" branch.
	wrongType := shape.Shape{Raw: mustShapeVal(t, `"$: input.name"`), Schema: schemaPtr(t, `{"type":"array"}`), Name: "over"}
	if _, err := wrongType.CheckWith(ctxSchema(t), overHook); err == nil || !strings.Contains(err.Error(), "must evaluate to an array") {
		t.Errorf("Result hook (wrong type) = %v; want 'must evaluate to an array'", err)
	}
	// Same shape, no hook → the generic default message.
	if _, err := wrongType.Check(ctxSchema(t)); err == nil || !strings.Contains(err.Error(), "does not conform") {
		t.Errorf("default message = %v; want 'does not conform'", err)
	}

	// Nullable: a possibly-null result against a required string → the "may be null" branch.
	nullable := shape.Shape{Raw: mustShapeVal(t, `"$: input.opt"`), Schema: schemaPtr(t, `{"type":"string"}`), Name: "url"}
	urlHook := shape.CheckHooks{Result: func(inferred, required schema.Schema) error {
		if inferred.HasNull() {
			return errors.New("url may be null; use ?? to provide a default value")
		}
		return errors.New("url must be a string, number or boolean")
	}}
	if _, err := nullable.CheckWith(ctxSchema(t), urlHook); err == nil || !strings.Contains(err.Error(), "may be null") {
		t.Errorf("Result hook (nullable) = %v; want 'may be null'", err)
	}
}

// The Roots hook receives the roots the shape's expressions reference and rejects one that
// exists in general but is unavailable here — the self.result case. It fires before
// inference, so the message is about availability, not a navigation failure.
func TestShape_CheckWith_RootsHookRejectsUnavailable(t *testing.T) {
	sh := shape.Shape{Raw: mustShapeVal(t, `{"answer": "$: self.result"}`), Name: "output"}
	hook := shape.CheckHooks{Roots: func(refs expression.Roots) error {
		if refs.SelfResult {
			return errors.New("output references self.result, but the action has no result_schema — add a result_schema to type the response")
		}
		return nil
	}}
	// ctxSchema declares no `self`, so without the hook this would be an opaque navigation
	// error; the hook turns it into an actionable message.
	if _, err := sh.CheckWith(ctxSchema(t), hook); err == nil || !strings.Contains(err.Error(), "add a result_schema") {
		t.Errorf("Roots hook = %v; want the result_schema hint", err)
	}
	// A shape that does not touch self.result passes the hook cleanly.
	ok := shape.Shape{Raw: mustShapeVal(t, `{"answer": "$: input.name"}`), Name: "output"}
	if _, err := ok.CheckWith(ctxSchema(t), hook); err != nil {
		t.Errorf("shape not touching self.result should pass: %v", err)
	}
}

// An Expr shape is a bare expression (a switch case): checked and evaluated directly, with
// a required Schema of the expected type (boolean) — sharing the same hooks.
func TestShape_ExprMode(t *testing.T) {
	ctx := ctxSchema(t)
	boolReq := schemaPtr(t, `{"type":"boolean"}`)

	// A boolean expression checks clean and evaluates directly.
	ok := shape.Shape{Raw: "input.n > 2", Schema: boolReq, Name: "case", Expr: true}
	if _, err := ok.Check(ctx); err != nil {
		t.Errorf("boolean case rejected: %v", err)
	}
	v, err := ok.Eval(map[string]any{"input": map[string]any{"n": 5}})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if v != true {
		t.Errorf("Eval = %#v, want true", v)
	}

	// A non-boolean expression fails conformance; the Result hook crafts the message.
	bad := shape.Shape{Raw: "input.name", Schema: boolReq, Name: "case", Expr: true}
	_, err = bad.CheckWith(ctx, shape.CheckHooks{
		Result: func(inferred, _ schema.Schema) error {
			return fmt.Errorf("case must evaluate to boolean, got %q", inferred.TypeName())
		},
	})
	if err == nil || !strings.Contains(err.Error(), "must evaluate to boolean") {
		t.Errorf("non-boolean case = %v; want the boolean message", err)
	}

	// The Roots hook also applies to an Expr shape: an expression that touches self.result.
	touches := shape.Shape{Raw: "self.result.done", Name: "case", Expr: true}
	_, err = touches.CheckWith(ctx, shape.CheckHooks{Roots: func(r expression.Roots) error {
		if r.SelfResult {
			return errors.New("case references self.result, but the action has no result_schema")
		}
		return nil
	}})
	if err == nil || !strings.Contains(err.Error(), "no result_schema") {
		t.Errorf("Expr Roots hook = %v; want the self.result message", err)
	}
}

func TestShape_Present(t *testing.T) {
	var absent *shape.Shape
	if absent.Present() {
		t.Error("nil shape should not be Present")
	}
	var sh shape.Shape
	if err := json.Unmarshal([]byte(`{"a": "$: x", "b": ["$: y", 5, {"c": "$: z"}]}`), &sh); err != nil {
		t.Fatal(err)
	}
	if !sh.Present() {
		t.Error("shape should be Present")
	}
}
