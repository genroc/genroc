package schematest

import (
	"encoding/json"
	"math/rand"
	"testing"

	"genroc/internal/schema"

	"github.com/xeipuuv/gojsonschema"
)

// The real guard that "we fail in the same cases as gojsonschema": random documents at both
// validators, asserting the same decision, exploring the space between the hand-written cases.
// The seed is fixed; a failure prints the document so it can be pinned as a case.
func TestValidateDecisionMatchesGojsonschemaFuzz(t *testing.T) {
	const (
		seed           = 0x9e3779b9
		itersPerSchema = 3000
	)

	schemas := []string{
		`{"type":"object","properties":{"id":{"type":"integer","minimum":1,"maximum":10},"name":{"type":["string","null"]},"tags":{"type":"array","items":{"type":"string"},"maxItems":2}},"required":["id"]}`,
		`{"type":"object","properties":{"user":{"type":"object","properties":{"name":{"type":"string","minLength":1},"age":{"type":"integer"}},"required":["name"]},"item":{"$ref":"#/$defs/Item"}},"required":["user"],"$defs":{"Item":{"type":"object","properties":{"v":{"type":"integer"}},"required":["v"]}}}`,
		`{"oneOf":[{"type":"string","minLength":2},{"type":"integer","minimum":0}]}`,
		`{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"null"}]}`,
		`{"type":"array","items":{"type":"object","properties":{"v":{"type":"integer"}},"required":["v"]},"minItems":1,"maxItems":3}`,
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
			data := randValue(r, 3)

			// Round-trip through JSON so both validators see identical typing
			// (numbers as float64, etc.) — the same shape the runtime produces.
			raw, err := json.Marshal(data)
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
		// Guard against a degenerate corpus: parity is only meaningful if the
		// generator produces both accepted and rejected documents for each schema.
		if nValid == 0 || nInvalid == 0 {
			t.Errorf("schema[%d] degenerate fuzz split: valid=%d invalid=%d (agreement is not exercised)", si, nValid, nInvalid)
		}
		t.Logf("schema[%d]: %d accepted / %d rejected (agree)", si, nValid, nInvalid)
	}
}

// keyPool overlaps the property names used by the fuzz schemas (so required/type
// checks fire) plus a couple of undeclared keys (which both validators accept).
var keyPool = []string{"id", "name", "tags", "user", "age", "item", "v", "a", "extra", "x"}

// stringPool straddles the declared minLength/maxLength/enum bounds.
var stringPool = []string{"", "a", "ab", "red", "green", "héllo", "abcdef"}

// randValue builds a random JSON-compatible value up to the given depth.
func randValue(r *rand.Rand, depth int) any {
	kind := r.Intn(7)
	if depth <= 0 && (kind == 5 || kind == 6) {
		kind = r.Intn(5) // no more nesting at the bottom
	}
	switch kind {
	case 0:
		return nil
	case 1:
		return r.Intn(2) == 0
	case 2:
		return r.Intn(15) - 3 // -3..11, straddles minimum/maximum 0..10
	case 3:
		return float64(r.Intn(24)-3) / 2 // -1.5..10, mixes integral and fractional
	case 4:
		return stringPool[r.Intn(len(stringPool))]
	case 5:
		n := r.Intn(4)
		arr := make([]any, n)
		for i := range arr {
			arr[i] = randValue(r, depth-1)
		}
		return arr
	default:
		n := r.Intn(4)
		obj := make(map[string]any, n)
		for i := 0; i < n; i++ {
			obj[keyPool[r.Intn(len(keyPool))]] = randValue(r, depth-1)
		}
		return obj
	}
}
