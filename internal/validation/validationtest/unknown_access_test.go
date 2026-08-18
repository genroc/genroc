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
