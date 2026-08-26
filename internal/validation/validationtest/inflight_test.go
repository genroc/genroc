package validationtest

import (
	"encoding/json"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

func schemaOf(t *testing.T, raw string) *schema.Schema {
	t.Helper()
	var s schema.Schema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("schema %s: %v", raw, err)
	}
	return &s
}

func externalTask(rs *schema.Schema) *model.Task {
	return &model.Task{ID: "hold", Action: &model.Action{Type: model.ActionTypeExternal, ResultSchema: rs}}
}

// A result in flight is judged by CONTRACT optics, not storage optics, and the difference is
// the whole point of this test.
//
// Stored state gets the tolerant relation because a migration can repair it: a required
// nullable that the old version left absent is a gap MigrateState closes by writing the null
// in. Nobody does that to a worker's submission — it arrives from outside, is conformed once
// at the boundary, and an absent key is simply refused. So the relation for an in-flight
// result must be the strict one, and this is the gap where the two disagree.
func TestInFlightResultBreaks_JudgesByContractNotByStorage(t *testing.T) {
	// `a` optional becomes required-and-nullable.
	from := schemaOf(t, `{"type":"object","properties":{"a":{"type":"string"}}}`)
	to := schemaOf(t, `{"type":"object","properties":{"a":{"type":["string","null"]}},"required":["a"]}`)

	// Guards the fixture, not the code: a change that made these agree would leave the
	// assertion below passing while testing nothing.
	if !from.IsSubsetAsStored(*to) {
		t.Fatal("fixture no longer discriminates: the STORAGE relation must accept this gap")
	}
	if from.IsSubset(*to) {
		t.Fatal("fixture no longer discriminates: the STRICT relation must refuse this gap")
	}

	breaks := validation.InFlightResultBreaks(externalTask(from), externalTask(to))
	if len(breaks) == 0 {
		t.Fatal("a worker submitting {} would be refused after the move, and the upgrade allowed it: " +
			"the in-flight check is reading the result through storage optics")
	}
	for _, b := range breaks {
		if b.Member != validation.MemberUpgrade {
			t.Errorf("member is %q; an in-flight result is an UPGRADE concern -- the contract half "+
				"gates registration and answers a different question", b.Member)
		}
	}
}

// The counterpart: widening is what an upgrade is allowed to do. A worker's result satisfies
// the old schema, and the new one accepts everything the old did.
func TestInFlightResultBreaks_AcceptsAWidenedResult(t *testing.T) {
	from := schemaOf(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)
	to := schemaOf(t, `{"type":"object","properties":{"a":{"type":["string","null"]},"b":{"type":"number"}},"required":["a"]}`)

	if breaks := validation.InFlightResultBreaks(externalTask(from), externalTask(to)); len(breaks) != 0 {
		t.Errorf("a widened result was refused, so an upgrade that could not break a worker is blocked: %+v", breaks)
	}
}
