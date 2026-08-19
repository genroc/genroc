package validationtest

import (
	"encoding/json"
	"testing"
)

// `{}` is genroc's `unknown`: carried, compared, exported — never read INTO. Comparison is
// allowed because it treats the value as one opaque token (and is how a definition tests an
// opaque body for presence); member access is not, because it reads a shape nobody declared.
// The same line TypeScript draws for `unknown`.
func unknownAccessDef(t *testing.T, expr string, tSchema map[string]any) string {
	t.Helper()
	if tSchema == nil {
		tSchema = map[string]any{"type": "object",
			"properties": map[string]any{"field": map[string]any{"type": "string"}}}
	}
	out, err := json.Marshal(map[string]any{
		"name": "p",
		"input_schema": map[string]any{"type": "object",
			"properties": map[string]any{"u": map[string]any{}, "t": tSchema},
			"required":   []string{"u", "t"}},
		"tasks": []any{map[string]any{"id": "c", "output": map[string]any{"v": expr}, "switch": "end"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

func TestGenerate_Unknown_ComparedNotRead(t *testing.T) {
	for _, expr := range []string{
		"$: input.u",         // exported whole — what makes {} useful at all
		"$: input.u == null", // presence test on an opaque body
		"$: input.u != null",
		"$: input.u == 'x'",
		"$: input.u == 1",
	} {
		if err := runGenerateErr(t, unknownAccessDef(t, expr, nil)); err != nil {
			t.Errorf("%s must be allowed — comparing treats the value as one token: %v", expr, err)
		}
	}
	if err := runGenerateErr(t, unknownAccessDef(t, "$: input.u.field", nil)); err == nil {
		t.Error("reading a property off an unknown must be refused")
	}
}

// Every route that could carry an unknown into a position where a property is read. Each one
// reaches the access through a different construct, so a guard added to only one of them
// leaves the others open — which is how the union case survived until it was looked for.
func TestGenerate_Unknown_CannotBeLaundered(t *testing.T) {
	for _, tc := range []struct{ name, expr string }{
		{"coalesced with a scalar", "$: (input.u ?? 'x').field"},
		{"coalesced with a typed object", "$: (input.u ?? input.t).field"},
		{"through an array literal", "$: [input.u][0].field"},
		{"through an object literal", "$: {a: input.u}.a.field"},
		{"through a lambda", "$: map([1], i => input.u)[0].field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runGenerateErr(t, unknownAccessDef(t, tc.expr, nil)); err == nil {
				t.Errorf("%s must not make an unknown readable", tc.expr)
			}
		})
	}
}

// The one shape that IS allowed, and why: `a ?? b` where `a` cannot be null never evaluates
// `b`, so the unknown arm is dead and the result is just `a`. Make the left side nullable and
// the arm becomes reachable, so the same expression is refused. Pinning both directions keeps
// a later change from "fixing" the accepted case into a false refusal.
func TestGenerate_Unknown_DeadCoalesceArmIsNotALeak(t *testing.T) {
	nullable := map[string]any{"type": []string{"object", "null"},
		"properties": map[string]any{"field": map[string]any{"type": "string"}}}

	if err := runGenerateErr(t, unknownAccessDef(t, "$: (input.t ?? input.u).field", nil)); err != nil {
		t.Errorf("a non-nullable left side makes the unknown arm unreachable, so this is sound: %v", err)
	}
	if err := runGenerateErr(t, unknownAccessDef(t, "$: (input.t ?? input.u).field", nullable)); err == nil {
		t.Error("a nullable left side makes the unknown arm reachable — the access must be refused")
	}
}

// An unknown is unreadable wherever it SITS, not just where it was declared. Each row reaches
// the value by a different route, and the routes are separate code paths — property lookup,
// index, computed key — so a guard on one says nothing about the others. The `$ref` row is the
// one that would break silently: the emptiness test has to run after the deref, or a `{}`
// behind a reference looks like an ordinary object schema and reads straight through.
func TestGenerate_Unknown_UnreadableInEveryPosition(t *testing.T) {
	unknown := map[string]any{}
	for _, tc := range []struct {
		name, expr string
		props      map[string]any
		defs       map[string]any
	}{
		{
			name: "bare, indexed", expr: "$: input.u[0]",
			props: map[string]any{"u": unknown},
		},
		{
			name: "bare, computed key", expr: "$: input.u['k']",
			props: map[string]any{"u": unknown},
		},
		{
			name: "declared as a property's schema", expr: "$: input.o.a.f",
			props: map[string]any{"o": map[string]any{"type": "object",
				"properties": map[string]any{"a": unknown}, "required": []string{"a"}}},
		},
		{
			name: "as an array's item type", expr: "$: input.arr[0].f",
			props: map[string]any{"arr": map[string]any{"type": "array", "items": unknown}},
		},
		{
			name: "as a map's value type", expr: "$: input.m['k'].f",
			props: map[string]any{"m": map[string]any{"type": "object", "additionalProperties": unknown}},
		},
		{
			name: "behind a $ref", expr: "$: input.r.f",
			props: map[string]any{"r": map[string]any{"$ref": "#/$defs/U"}},
			defs:  map[string]any{"U": unknown},
		},
		{
			name: "interpolated from a nested position", expr: "${ input.o.a }",
			props: map[string]any{"o": map[string]any{"type": "object",
				"properties": map[string]any{"a": unknown}, "required": []string{"a"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			required := make([]string, 0, len(tc.props))
			for k := range tc.props {
				required = append(required, k)
			}
			def := map[string]any{
				"name":         "p",
				"input_schema": map[string]any{"type": "object", "properties": tc.props, "required": required},
				"tasks": []any{map[string]any{"id": "c",
					"output": map[string]any{"v": tc.expr}, "switch": "end"}},
			}
			if tc.defs != nil {
				def["$defs"] = tc.defs
			}
			raw, err := json.Marshal(def)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := runGenerateErr(t, string(raw)); err == nil {
				t.Errorf("%s must not be readable: an unknown here is undeclared data wherever it sits", tc.expr)
			}
		})
	}
}

// `query` is `headers` with the differences that carry the feature: a null VALUE is legal and
// omits its parameter, so an optional parameter needs no conditional. The MAP may still not be
// null — that is a mistake, not an empty query. Scalars rather than strings-only, because the
// null-omit does not compose with `${ }` (interpolating a nullable is refused), so a
// strings-only target would make an optional NUMBER parameter unwritable. An array of scalars
// is legal too and repeats the parameter; an array of objects is not, for the same reason a
// bare object is not — it has no url encoding.
func TestGenerate_QueryShape(t *testing.T) {
	def := func(query string) string {
		return `{"name":"p","input_schema":{"type":"object","properties":{
			"s":{"type":"string"},"n":{"type":["integer","null"]},"o":{"type":"object"},
			"tags":{"type":"array","items":{"type":"string"}},
			"objs":{"type":"array","items":{"type":"object"}}},
			"required":["s","n","o","tags","objs"]},
		 "tasks":[{"id":"c","action":{"type":"fetch","url":"http://x","query":` + query + `},"switch":"end"}]}`
	}
	for _, ok := range []string{
		`{"a":"$: input.s"}`, // a string
		`{"a":"$: input.n"}`, // a nullable number — the case the slot exists for
		`{"a":"literal","b":"${ input.s }"}`,
		`{"a":"$: true"}`,       // a boolean scalar
		`{"a":"$: input.tags"}`, // an array of scalars — repeats the parameter
		`{"a":"$: ['x','y']"}`,  // an array literal
	} {
		if err := runGenerateErr(t, def(ok)); err != nil {
			t.Errorf("%s must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []struct{ query, why string }{
		{`{"a":"$: input.o"}`, "an object is not a scalar; it has no url encoding"},
		{`{"a":"$: input.objs"}`, "an array of objects has no url encoding either"},
		{`"$: input.n"`, "the map itself may not be null — that is a mistake, not an empty query"},
	} {
		if err := runGenerateErr(t, def(bad.query)); err == nil {
			t.Errorf("%s must be refused: %s", bad.query, bad.why)
		}
	}
}
