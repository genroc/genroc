package schematest

import (
	"strings"
	"testing"

	"genroc/internal/schema"
	"genroc/internal/template"
)

// The top type is "carried, never read" (specs/unknown-type.md), and putting it in a UNION
// must not launder that away. A miss on an ordinary variant means "this one has no such
// field" — an answer, folded in as a null arm. An unknown variant means "nothing here is
// declared", which is not an answer: reading through it would be reading INTO undeclared
// data, the one thing {} exists to prevent. Reachable from an ordinary definition, since a
// fetch may declare one status `{}` and another a real shape.
func TestNavigate_UnknownVariantRefusesEveryAccess(t *testing.T) {
	parse := func(raw string) schema.Schema {
		t.Helper()
		s, err := schema.Parse([]byte(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		n, err := s.Normalize()
		if err != nil {
			t.Fatalf("normalize %s: %v", raw, err)
		}
		return n
	}
	obj := parse(`{"type":"object","properties":{"fee":{"type":"number"}},"required":["fee"]}`)
	arr := parse(`{"type":"array","items":{"type":"number"}}`)
	mapp := parse(`{"type":"object","additionalProperties":{"type":"number"}}`)
	unknown := parse(`{}`)
	null := parse(`{"type":"null"}`)

	for _, tc := range []struct {
		name, expr string
		value      schema.Schema
		wantErr    bool
		why        string
	}{
		{
			name: "property through an unknown variant", expr: "v.fee",
			value: schema.AnyOf(unknown, obj), wantErr: true,
			why: "on the unknown arm the field is undecidable, not absent",
		},
		{
			name: "index through an unknown variant", expr: "v[0]",
			value: schema.AnyOf(unknown, arr), wantErr: true,
			why: "same rule for the element type",
		},
		{
			name: "key through an unknown variant", expr: "v['k']",
			value: schema.AnyOf(unknown, mapp), wantErr: true,
			why: "same rule for a computed key",
		},
		{
			name: "the whole value stays readable", expr: "v",
			value: schema.AnyOf(unknown, obj), wantErr: false,
			why: "carried, just not read into — this is what makes {} exportable at all",
		},
		{
			name: "a null arm is an answer, not an unknown", expr: "v.fee",
			value: schema.OneOf(null, obj), wantErr: false,
			why: "nullable navigation must keep working, or every ?? idiom breaks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := schema.Object().WithProperty("v", tc.value, true)
			_, err := ctx.Infer(tc.expr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("%s = %v, wantErr %v — %s", tc.expr, err, tc.wantErr, tc.why)
			}
			if tc.wantErr && err != nil {
				// The message has to name the fix; "not found" would send the author
				// looking for a typo in a field that is not the problem.
				for _, want := range []string{"unknown", "{}"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("message %q should mention %q", err, want)
					}
				}
			}
		})
	}
}

// The two spellings must agree. `$: x` refuses an unknown because it is not a subset of the
// slot's target type; `${ x }` used to accept it, since the stringify guard fires only on a
// type that PROVABLY cannot render and the top type proves nothing. But interpolating is
// reading, which is what {} forbids — and the runtime failure is a terminal
// engine.expression, not something on_error can catch.
func TestTemplate_UnknownCannotBeInterpolated(t *testing.T) {
	raw, err := schema.Parse([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	unknown, err := raw.Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ctx := schema.Object().WithProperty("u", unknown, true)

	if _, err := ctx.Infer("u"); err != nil {
		t.Fatalf("reading the whole unknown value must stay legal: %v", err)
	}
	tpl, err := template.Parse("${ u }/x")
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}
	_, err = tpl.InferType(ctx)
	if err == nil {
		t.Fatal("interpolating an unknown into text must be refused at registration")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("message %q should say the value is unknown", err)
	}

	// A declared scalar still interpolates, or every url template breaks.
	declared := schema.Object().WithProperty("u", schema.Type("string"), true)
	if _, err := tpl.InferType(declared); err != nil {
		t.Errorf("a declared string must still interpolate: %v", err)
	}
}

// `anyOf[{}, T]` denotes the same set as `{}` — one arm already accepts everything — so the
// relation must recognise it in BOTH directions, or a change that turns nobody away is
// reported as a break. TypeScript reduces `T | unknown` to `unknown` outright; genroc keeps
// the union in the document and has to answer the question at the relation instead.
//
// oneOf is deliberately not absorbed: there a value matching two arms is REJECTED, so a
// top-type arm narrows rather than widens, and `T ⊆ oneOf[{}, T]` is genuinely false.
func TestIsSubset_TopTypeArmAbsorbs(t *testing.T) {
	parse := func(raw string) schema.Schema {
		t.Helper()
		p, err := schema.Parse([]byte(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		n, err := p.Normalize()
		if err != nil {
			t.Fatalf("normalize %s: %v", raw, err)
		}
		return n
	}
	unknown := parse(`{}`)
	obj := parse(`{"type":"object","properties":{"fee":{"type":"number"}},"required":["fee"]}`)
	str := parse(`{"type":"string"}`)

	anyUnion := schema.AnyOf(unknown, obj)
	if !anyUnion.IsSubset(unknown) {
		t.Error("anyOf[{},T] ⊆ {} must hold: {} accepts everything")
	}
	if !unknown.IsSubset(anyUnion) {
		t.Error("{} ⊆ anyOf[{},T] must hold: the top-type arm already accepts everything, " +
			"and refusing it reports a break for a change that turns nobody away")
	}
	if !str.IsSubset(anyUnion) {
		t.Error("any type ⊆ anyOf[{},T] must hold for the same reason")
	}

	// The exclusion, so absorption does not creep into oneOf where it is unsound.
	oneUnion := schema.OneOf(unknown, obj)
	if unknown.IsSubset(oneUnion) {
		t.Error("{} ⊆ oneOf[{},T] must NOT hold: a value matching both arms is rejected by oneOf")
	}
}
