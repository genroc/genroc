package validationtest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"
	"genroc/internal/validation"
)

func mustMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// mustSchema parses a JSON schema fixture as-is, failing the test on error.
func mustSchema(t *testing.T, src string) schema.Schema {
	t.Helper()
	raw, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return raw.AssumeNormalized()
}

// recCtx builds the context as the pipeline would when inferring a self-referential task's
// output: outputs.<selfID> and self.previous are both $refs to the recursive placeholder,
// siblings always-available. Represents the process "in that state" without the pipeline.
func recCtx(t *testing.T, selfID string, siblings map[string]schema.Schema) (schema.Schema, string) {
	t.Helper()
	selfDef := selfID + "_output"
	ref := schema.Ref(selfDef)

	outputs := schema.Object().WithProperty(selfID, ref, false) // self output is NOT required (nullable previous)
	for k, v := range siblings {
		outputs = outputs.WithProperty(k, v, true)
	}

	ctx := schema.Object().
		WithProperty("outputs", outputs, true).
		WithProperty("self", schema.Object().WithProperty("previous", ref, false), true).
		WithDef(selfDef, mustSchema(t, `{"type":"null"}`)) // placeholder; the fixpoint rebinds it
	return ctx, selfDef
}

func TestInferRecursiveOutput(t *testing.T) {
	// sobj builds the expected {type:object, required, properties} JSON for a flat
	// output type whose fields are all primitives.
	sobj := func(props map[string]string, req ...string) string {
		p := make(map[string]any, len(props))
		for k, typ := range props {
			p[k] = map[string]any{"type": typ}
		}
		b, _ := json.Marshal(map[string]any{"type": "object", "properties": p, "required": req})
		return string(b)
	}

	tests := []struct {
		name     string
		exprs    map[string]string
		siblings map[string]string // sibling task id -> schema JSON
		selfID   string
		want     string // schema JSON
		wantErr  bool
	}{
		{
			name:   "counter via outputs.<self>",
			exprs:  map[string]string{"n": "$: (outputs.count.n ?? 0) + 1"},
			selfID: "count",
			want:   sobj(map[string]string{"n": "integer"}, "n"),
		},
		{
			name:   "counter via self.previous",
			exprs:  map[string]string{"n": "$: (self.previous.n ?? 0) + 1"},
			selfID: "count",
			want:   sobj(map[string]string{"n": "integer"}, "n"),
		},
		{
			name:   "string accumulator",
			exprs:  map[string]string{"s": `$: (outputs.cat.s ?? "") + "x"`},
			selfID: "cat",
			want:   sobj(map[string]string{"s": "string"}, "s"),
		},
		{
			name:   "boolean toggle via self.previous",
			exprs:  map[string]string{"f": "$: !(self.previous.f ?? false)"},
			selfID: "tog",
			want:   sobj(map[string]string{"f": "boolean"}, "f"),
		},
		{
			name:     "sum folding a sibling output",
			exprs:    map[string]string{"total": "$: (outputs.acc.total ?? 0) + outputs.item.value"},
			siblings: map[string]string{"item": sobj(map[string]string{"value": "number"}, "value")},
			selfID:   "acc",
			want:     sobj(map[string]string{"total": "number"}, "total"),
		},
		{
			name: "multiple fields mixing both self references",
			exprs: map[string]string{
				"n": "$: (outputs.s.n ?? 0) + 1",
				"f": "$: !(self.previous.f ?? false)",
			},
			selfID: "s",
			want:   sobj(map[string]string{"n": "integer", "f": "boolean"}, "f", "n"),
		},
		{
			name:    "no base case (no ?? default) is rejected",
			exprs:   map[string]string{"n": "$: outputs.c.n + 1"},
			selfID:  "c",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			siblings := make(map[string]schema.Schema, len(tt.siblings))
			for k, src := range tt.siblings {
				siblings[k] = mustSchema(t, src)
			}
			ctx, selfDef := recCtx(t, tt.selfID, siblings)
			got, err := validation.InferRecursiveOutput(tt.exprs, ctx, selfDef)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := mustSchema(t, tt.want)
			if !got.Equal(want) {
				t.Errorf("type mismatch:\n  got:  %s\n  want: %s",
					mustMarshal(got.Canonicalize()), mustMarshal(want.Canonicalize()))
			}
		})
	}
}
