package schematest

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"genroc/internal/schema"

	"github.com/xeipuuv/gojsonschema"
)

// Complements the blind fuzz with a schema-DIRECTED generator: blind random documents are
// almost always invalid, so they exercise rejection but barely touch acceptance. This straddles
// each declared bound and occasionally drops a required field — where drift actually hides.
func TestValidateDecisionMatchesGojsonschemaDirected(t *testing.T) {
	const (
		seed           = 0x2545f491
		itersPerSchema = 4000
	)

	schemas := []string{
		`{"type":"object","properties":{"id":{"type":"integer","minimum":1,"maximum":10},"name":{"type":["string","null"]},"tags":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":2}},"required":["id","name"]}`,
		`{"type":"object","properties":{"user":{"type":"object","properties":{"name":{"type":"string","minLength":1,"maxLength":4}},"required":["name"]},"item":{"$ref":"#/$defs/Item"}},"required":["user"],"$defs":{"Item":{"type":"object","properties":{"v":{"type":"integer","minimum":0}},"required":["v"]}}}`,
		`{"oneOf":[{"type":"string","minLength":2},{"type":"integer","minimum":0,"maximum":100}]}`,
		`{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"null"}]}`,
		`{"type":"array","items":{"type":"object","properties":{"v":{"type":"integer"},"label":{"type":"string","minLength":2}},"required":["v"]},"minItems":1,"maxItems":3}`,
		`{"type":"string","enum":["red","green","blue"]}`,
	}

	for si, schemaJSON := range schemas {
		compiled, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(schemaJSON))
		if err != nil {
			t.Fatalf("schema[%d] gojsonschema compile: %v", si, err)
		}
		rawSc, err := schema.Parse([]byte(schemaJSON))
		if err != nil {
			t.Fatalf("schema[%d] schema.Parse: %v", si, err)
		}
		sc := rawSc.AssumeNormalized()

		r := rand.New(rand.NewSource(int64(seed) + int64(si)))
		var nValid, nInvalid int
		for i := 0; i < itersPerSchema; i++ {
			// Round-trip so both validators see identical JSON typing.
			raw, err := json.Marshal(genFromSchema(r, sc, 4))
			if err != nil {
				continue
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				continue
			}

			result, err := compiled.Validate(gojsonschema.NewGoLoader(doc))
			if err != nil {
				t.Fatalf("schema[%d] gojsonschema.Validate(%s): %v", si, raw, err)
			}
			theirsValid := result.Valid()

			_, ourErr := sc.Validate(doc)
			oursValid := ourErr == nil

			if oursValid != theirsValid {
				t.Fatalf("DISAGREEMENT\n  schema: %s\n  doc:    %s\n  ours-valid=%v (err=%v)\n  gojsonschema-valid=%v (errs=%v)",
					schemaJSON, raw, oursValid, ourErr, theirsValid, result.Errors())
			}
			if theirsValid {
				nValid++
			} else {
				nInvalid++
			}
		}
		if nValid == 0 || nInvalid == 0 {
			t.Errorf("schema[%d] degenerate directed split: valid=%d invalid=%d", si, nValid, nInvalid)
		}
		t.Logf("schema[%d]: %d accepted / %d rejected (agree)", si, nValid, nInvalid)
	}
}

// genFromSchema builds a value shaped by s. It is deliberately imperfect: it
// straddles numeric/length/item bounds and occasionally drops a required property
// or emits a wrong-typed value, so the corpus spans valid, boundary, and invalid.
func genFromSchema(r *rand.Rand, s schema.Schema, depth int) any {
	resolved, err := s.Resolve()
	if err != nil || resolved.IsZero() {
		return randScalar(r)
	}
	// ~1 in 12: emit a wrong-shaped value to force a type error at this node.
	if r.Intn(12) == 0 {
		return randScalar(r)
	}
	if variants := resolved.Variants(); len(variants) > 0 {
		return genFromSchema(r, variants[r.Intn(len(variants))], depth)
	}
	if enum := resolved.Enum(); len(enum) > 0 {
		if r.Intn(4) == 0 {
			return "definitely-not-in-enum"
		}
		return enum[r.Intn(len(enum))]
	}

	typ := "object"
	switch {
	case len(resolved.Type()) > 0:
		ts := resolved.Type()
		typ = ts[r.Intn(len(ts))]
	case resolved.HasProperties():
		typ = "object"
	case resolved.HasItems():
		typ = "array"
	default:
		return randScalar(r)
	}

	switch typ {
	case "null":
		return nil
	case "boolean":
		return r.Intn(2) == 0
	case "integer":
		return genBoundedInt(r, resolved)
	case "number":
		return float64(genBoundedInt(r, resolved)) + 0.5*float64(r.Intn(2))
	case "string":
		return genBoundedString(r, resolved)
	case "array":
		return genArray(r, resolved, depth)
	default: // object
		return genObject(r, resolved, depth)
	}
}

func genObject(r *rand.Rand, s schema.Schema, depth int) any {
	obj := map[string]any{}
	required := s.Required()
	for name, prop := range s.Properties() {
		req := contains(required, name)
		include := req || r.Intn(2) == 0
		if req && r.Intn(10) == 0 {
			include = false // occasionally drop a required field → invalid
		}
		if !include {
			continue
		}
		if depth <= 0 {
			obj[name] = randScalar(r)
		} else {
			obj[name] = genFromSchema(r, prop, depth-1)
		}
	}
	if r.Intn(3) == 0 {
		obj["undeclared"] = randScalar(r) // both validators accept extras
	}
	return obj
}

func genArray(r *rand.Rand, s schema.Schema, depth int) any {
	lo, hi := 0, 3
	if min, ok := s.MinItems(); ok {
		lo = min - 1
	}
	if max, ok := s.MaxItems(); ok {
		hi = max + 1
	}
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	n := lo + r.Intn(hi-lo+1)
	arr := make([]any, n)
	items := s.Items()
	for i := range arr {
		if depth <= 0 {
			arr[i] = randScalar(r)
		} else {
			arr[i] = genFromSchema(r, items, depth-1)
		}
	}
	return arr
}

// genBoundedInt returns an int that straddles [minimum, maximum] so off-by-one
// boundary cases (which must decide identically on both sides) show up often.
func genBoundedInt(r *rand.Rand, s schema.Schema) int {
	lo, hi := -4, 14
	if min, ok := s.Minimum(); ok {
		lo = int(min) - 2
	}
	if max, ok := s.Maximum(); ok {
		hi = int(max) + 2
	}
	if hi <= lo {
		hi = lo + 1
	}
	return lo + r.Intn(hi-lo+1)
}

// genBoundedString returns a string whose length straddles [minLength, maxLength].
func genBoundedString(r *rand.Rand, s schema.Schema) string {
	lo, hi := 0, 6
	if min, ok := s.MinLength(); ok {
		lo = min - 1
	}
	if max, ok := s.MaxLength(); ok {
		hi = max + 1
	}
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	return strings.Repeat("a", lo+r.Intn(hi-lo+1))
}

// randScalar returns an arbitrary scalar or small container, used both as the
// leaf filler and as the intentional wrong-type corruption.
func randScalar(r *rand.Rand) any {
	switch r.Intn(6) {
	case 0:
		return nil
	case 1:
		return r.Intn(2) == 0
	case 2:
		return r.Intn(20) - 5
	case 3:
		return float64(r.Intn(10)) + 0.5
	case 4:
		return stringPool[r.Intn(len(stringPool))]
	default:
		return []any{r.Intn(3)}
	}
}
