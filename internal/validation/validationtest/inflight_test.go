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

func externalRaising(codes model.Raises) *model.Task {
	return &model.Task{ID: "hold", Action: &model.Action{
		Type: model.ActionTypeExternal, ResultSchema: &schema.Schema{}, Raises: codes,
	}}
}

// The error channel carries the same kind of promise as the result, and is judged the same way:
// per code, `old ⊆ new`, strictly. A worker answering a failure was handed the old declaration
// and its payload is conformed against whichever one the instance is on when it lands.
func TestInFlightResultBreaks_JudgesTheErrorChannelToo(t *testing.T) {
	optional := schemaOf(t, `{"type":"object","properties":{"why":{"type":"string"}}}`)
	required := schemaOf(t, `{"type":"object","properties":{"why":{"type":"string"}},"required":["why"]}`)

	t.Run("a narrowed payload", func(t *testing.T) {
		breaks := validation.InFlightResultBreaks(
			externalRaising(model.Raises{"boom": optional}),
			externalRaising(model.Raises{"boom": required}))
		if len(breaks) == 0 {
			t.Fatal("a worker submitting {code: boom, data: {}} would be refused after the move, " +
				"and the upgrade allowed it: the error channel is not being compared")
		}
		if got := breaks[0].Path; got != "boom.why" {
			t.Errorf("path is %q, want the code then the place inside its payload", got)
		}
	})

	// Dropping a code is not a narrowing, it is a refusal: the submission is rejected before its
	// payload is read at all.
	t.Run("a dropped code", func(t *testing.T) {
		breaks := validation.InFlightResultBreaks(
			externalRaising(model.Raises{"boom": optional}),
			externalRaising(model.Raises{"other": optional}))
		if len(breaks) == 0 {
			t.Fatal("the code a worker holds was dropped and the move was allowed")
		}
	})

	// Widening is what an upgrade is for. A code ADDED constrains nobody: no answer in flight
	// can carry it.
	t.Run("widened, and a code added", func(t *testing.T) {
		breaks := validation.InFlightResultBreaks(
			externalRaising(model.Raises{"boom": required}),
			externalRaising(model.Raises{"boom": optional, "fresh": required}))
		if len(breaks) != 0 {
			t.Errorf("an upgrade that could not break a worker was blocked: %+v", breaks)
		}
	})
}
