package engine

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/shape"
)

// The dereferencing matrix: one context carrying references at known places, and a table of
// expressions over it asserting BOTH axes -- the value the expression produces, and exactly
// which objects had to be loaded to produce it.
//
// Both axes or neither. The value alone passes whether or not the reference was needlessly
// loaded (content addressing makes a copied reference and a re-loaded one indistinguishable
// downstream), and the load set alone passes if the expression quietly returns nil.
// specs/lazy-context.md.

// The fixture. Each big value is distinct, so its object has its own hash and the load set
// names exactly which one was fetched.
const (
	bigBlob   = "BLOB" // input.blob
	bigCode   = "CODE" // outputs.a.code
	bigWhole  = "WHOLE"
	bigDetail = "DETAIL" // last_error.data.detail
	bigItem   = "ITEM"   // outputs.list[0]
)

func padded(s string) string { return s + strings.Repeat("=", 4096) }

// refs are the markers as the engine finds them after a read: one per externalized leaf, keyed
// by the name the table uses for it.
type fixture struct {
	engine *Engine
	refs   map[string]*model.ObjectRef
}

// newFixture stores one instance whose oversized leaves externalize, reads it back, and harvests
// the markers the decode placed. Real objects and the real loader: the only thing the test
// supplies is the shape of the context.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	database, e := lazyEngine(t)

	seed := storedContext(t, database, map[string]any{
		"input": map[string]any{"blob": padded(bigBlob), "name": "sam"},
		"outputs": map[string]any{
			"a":     map[string]any{"code": padded(bigCode), "n": float64(1)},
			"whole": padded(bigWhole),
			"list":  []any{padded(bigItem), "small"},
		},
		"last_error": map[string]any{
			"code": "http.500", "message": "boom",
			"data": map[string]any{"detail": padded(bigDetail)},
		},
	})

	f := &fixture{engine: e, refs: map[string]*model.ObjectRef{}}
	for name, path := range map[string][]any{
		"blob":   {"input", "blob"},
		"code":   {"outputs", "a", "code"},
		"whole":  {"outputs", "whole"},
		"item":   {"outputs", "list", 0},
		"detail": {"last_error", "data", "detail"},
	} {
		ref, ok := valueAt(seed.State, path).(*model.ObjectRef)
		if !ok {
			t.Fatalf("setup: %v is %T, want an externalized marker -- the cut chose differently, so the table's load names no longer mean what they say",
				path, valueAt(seed.State, path))
		}
		f.refs[name] = ref
	}
	// Distinct content must give distinct objects, or a load set cannot tell them apart.
	seen := map[string]string{}
	for name, ref := range f.refs {
		if other, dup := seen[ref.Ref]; dup {
			t.Fatalf("setup: %s and %s share object %s", name, other, ref.Ref)
		}
		seen[ref.Ref] = name
	}
	return f
}

// instance is a fresh instance over the fixture's context. Fresh per case: ResolvedObjects is
// the load record, and it accumulates.
func (f *fixture) instance() *model.ProcessInstance {
	r := f.refs
	return &model.ProcessInstance{
		ID: "matrix", ProcessName: "lazy", Status: model.StatusRunning,
		State: map[string]any{
			"input": map[string]any{"blob": r["blob"], "name": "sam"},
			"outputs": map[string]any{
				"a":     map[string]any{"code": r["code"], "n": float64(1)},
				"whole": r["whole"],
				"list":  []any{r["item"], "small"},
			},
			"last_error": map[string]any{
				"code": "http.500", "message": "boom",
				"data": map[string]any{"detail": r["detail"]},
			},
		},
	}
}

// loadedNames is which objects the advance actually fetched, by the table's names for them.
func (f *fixture) loadedNames(inst *model.ProcessInstance) []string {
	byHash := map[string]string{}
	for name, ref := range f.refs {
		byHash[ref.Ref] = name
	}
	var out []string
	for hash := range inst.ResolvedObjects {
		name, known := byHash[hash]
		if !known {
			name = "unknown:" + hash
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestLazyMatrix(t *testing.T) {
	f := newFixture(t)
	r := f.refs

	obj := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	cases := []struct {
		name  string
		expr  any      // the shape's raw value: a `$:` leaf, a template, or a structure
		want  any      // the evaluated value; a *model.ObjectRef here means "still a reference"
		loads []string // exactly the objects that had to be fetched
	}{
		// ---- copy positions: the reference travels through, nothing is fetched ----
		{
			name: "a slot copied whole keeps its leaf reference",
			expr: "$: outputs.a",
			want: obj("code", r["code"], "n", float64(1)),
		},
		{
			name: "a slot that IS a reference is copied as one",
			expr: "$: outputs.whole",
			want: r["whole"],
		},
		{
			name: "into a SHAPE's object value (aggregated by shape.Roots)",
			expr: obj("x", "$: outputs.a"),
			want: obj("x", obj("code", r["code"], "n", float64(1))),
		},
		{
			name: "into a SHAPE's array item",
			expr: []any{"$: outputs.whole"},
			want: []any{r["whole"]},
		},
		{
			// The expression-level object literal, which is a different branch of collectRoots
			// from the shape map above -- a shape leaf is its own template, so the two rows
			// that look alike exercise different code.
			name: "into an EXPRESSION's object literal",
			expr: "$: {x: outputs.a}",
			want: obj("x", obj("code", r["code"], "n", float64(1))),
		},
		{
			name: "into an EXPRESSION's array literal",
			expr: "$: [outputs.whole]",
			want: []any{r["whole"]},
		},
		{
			name: "two slots copied at once still load nothing",
			expr: obj("x", "$: outputs.a", "y", "$: outputs.whole"),
			want: obj("x", obj("code", r["code"], "n", float64(1)), "y", r["whole"]),
		},
		{
			name: "an array slot copied whole keeps its element reference",
			expr: "$: outputs.list",
			want: []any{r["item"], "small"},
		},
		{
			name: "a conditional branch is a copy position",
			expr: "$: 1 == 1 ? outputs.whole : outputs.a",
			want: r["whole"],
		},
		{
			name: "reading last_error.code does not pull the body",
			expr: "$: last_error.code",
			want: "http.500",
		},

		// ---- read-through positions: exactly what the read needs, and no more ----
		{
			name:  "reading a leaf loads its object",
			expr:  "$: outputs.a.code",
			want:  padded(bigCode),
			loads: []string{"code"},
		},
		{
			name:  "indexing an array loads the element's object",
			expr:  "$: outputs.list[0]",
			want:  padded(bigItem),
			loads: []string{"item"},
		},
		{
			name:  "reading into the error body loads it",
			expr:  "$: last_error.data.detail",
			want:  padded(bigDetail),
			loads: []string{"detail"},
		},
		{
			// A BARE reference in an interpolation is the case that pins the ${ } rule: with a
			// member chain the navigation marks it read-through anyway, so such a row would
			// pass even if interpolation were miscounted as a copy.
			name:  "an interpolation of a bare reference stringifies, so it loads",
			expr:  "${outputs.whole}",
			want:  padded(bigWhole),
			loads: []string{"whole"},
		},
		{
			name:  "an interpolation of a leaf loads it",
			expr:  "${outputs.a.code}",
			want:  padded(bigCode),
			loads: []string{"code"},
		},
		{
			name:  "a comparison operates on it, so it loads",
			expr:  "$: outputs.whole == null",
			want:  false,
			loads: []string{"whole"},
		},
		{
			name:  "a function argument loads",
			expr:  "$: map(outputs.list, e => e)",
			want:  []any{padded(bigItem), "small"},
			loads: []string{"item"},
		},
		{
			name:  "one slot read, another copied: only the read one loads",
			expr:  obj("read", "$: outputs.a.code", "copied", "$: outputs.whole"),
			want:  obj("read", padded(bigCode), "copied", r["whole"]),
			loads: []string{"code"},
		},

		// ---- the deferred half: laziness is per slot, so a sibling read still pays ----
		{
			name:  "reading a small sibling still materializes its slot (path-level laziness deferred)",
			expr:  "$: outputs.a.n",
			want:  float64(1),
			loads: []string{"code"},
		},
		{
			name:  "reading input.name still materializes input (path-level laziness deferred)",
			expr:  "$: input.name",
			want:  "sam",
			loads: []string{"blob"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := f.instance()
			sh := shape.Shape{Raw: tc.expr}
			got, err := f.engine.evalShape(inst, sh, nil)
			if err != nil {
				t.Fatalf("evalShape(%v): %v", tc.expr, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("value:\n got %s\nwant %s", brief(got), brief(tc.want))
			}
			loaded := f.loadedNames(inst)
			want := tc.loads
			if want == nil {
				want = []string{}
			}
			if loaded == nil {
				loaded = []string{}
			}
			sort.Strings(want)
			if !reflect.DeepEqual(loaded, want) {
				t.Errorf("loaded %v, want %v -- an extra load is a value copied for nothing; a missing one is a marker handed to whatever reads the result", loaded, want)
			}
		})
	}
}

// brief renders a value with big strings and markers shortened, so a failure is readable.
func brief(v any) string {
	switch t := v.(type) {
	case *model.ObjectRef:
		return "<ref " + t.Ref[:8] + ">"
	case string:
		if len(t) > 24 {
			return `"` + t[:8] + `..."`
		}
		return `"` + t + `"`
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+brief(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, brief(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return strings.TrimSpace(strings.Join(strings.Fields(reflect.ValueOf(&v).Elem().String()), " "))
}

func valueAt(root any, path []any) any {
	cur := root
	for _, seg := range path {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg.(string)]
		case []any:
			i, ok := seg.(int)
			if !ok || i >= len(node) {
				return nil
			}
			cur = node[i]
		default:
			return nil
		}
	}
	return cur
}
