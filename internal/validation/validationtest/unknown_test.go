package validationtest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// The unknown type across a process boundary: a child carrying a payload it never inspects, a
// parent narrowing it. The static half lives here; the runtime conform that backs the narrowing
// is exercised by the polling example's integration test.

func unknownDef(t *testing.T, raw string) *model.ProcessDefinition {
	t.Helper()
	var d model.ProcessDefinition
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return &d
}

// A child that reads `status` (typed, it drives the child's own decisions) and forwards
// `payload` (unknown, it never looks inside) — the forward-vs-act-on split.
const forwardingChildJSON = `{
  "name":"forwarder",
  "tasks":[{"id":"check",
    "action":{"type":"fetch","url":"http://x",
      "responses": { "200": {"type":"object",
        "properties":{"status":{"type":"string"},"payload":{"description":"opaque"}},
        "required":["status","payload"]} }},
    "switch":[{"goto":"end"}],
    "output":{"payload":"$: self.result.payload"}}],
  "output":{"payload":"$: outputs.check.payload"}
}`

func parentPinning(t *testing.T, resultSchema string, output string) *model.ProcessDefinition {
	t.Helper()
	return unknownDef(t, `{
      "name":"parent",
      "tasks":[{"id":"run",
        "action":{"type":"child","name":"forwarder","result_schema":`+resultSchema+`},
        "switch":[{"goto":"end"}],
        "output":`+output+`}],
      "output":{"a":"$: outputs.run.a"}
    }`)
}

// The whole point: a parent pins a concrete schema onto the child's opaque field and can
// then read through it. Statically this is childOutput.NarrowsTo(result_schema); at
// runtime collect conforms the child's output against that same schema.
func TestUnknown_ParentNarrowsChildPayload(t *testing.T) {
	child := unknownDef(t, forwardingChildJSON)
	parent := parentPinning(t,
		`{"type":"object","properties":{"payload":{"type":"object","properties":{"answer":{"type":"number"}},"required":["answer"]}},"required":["payload"]}`,
		`{"a":"$: self.result.payload.answer"}`)
	if err := validation.ValidateChildProcessRefs(parent, 1, stubGetter{"forwarder": child}); err != nil {
		t.Fatalf("narrowing a child's unknown must be accepted: %v", err)
	}
}

// Narrowing is not a rubber stamp: it relaxes the unknown case only, so a child whose
// output is genuinely the wrong shape is still caught at registration.
func TestUnknown_NarrowingStillCatchesRealMismatch(t *testing.T) {
	// The child's output object itself is typed — `payload` is a declared key — so a
	// result_schema demanding a different required key cannot be satisfied.
	child := unknownDef(t, forwardingChildJSON)
	parent := parentPinning(t,
		`{"type":"object","properties":{"missing":{"type":"string"}},"required":["missing"]}`,
		`{"a":"$: self.result.missing"}`)
	err := validation.ValidateChildProcessRefs(parent, 1, stubGetter{"forwarder": child})
	if err == nil || !strings.Contains(err.Error(), "result_schema") {
		t.Fatalf("want a result_schema incompatibility, got: %v", err)
	}
}

// A parent may also decline to narrow and forward the opacity upward: everything is a
// subset of unknown, so this needs no relaxation at all.
func TestUnknown_ParentForwardsWithoutNarrowing(t *testing.T) {
	child := unknownDef(t, forwardingChildJSON)
	parent := parentPinning(t, `{}`, `{"a":"$: self.result"}`)
	if err := validation.ValidateChildProcessRefs(parent, 1, stubGetter{"forwarder": child}); err != nil {
		t.Fatalf("forwarding a child result as unknown must be accepted: %v", err)
	}
}

// The forwarding parent gets an unknown, not an escape hatch: it still cannot read it.
func TestUnknown_ForwardingParentCannotRead(t *testing.T) {
	err := runGenerateErr(t, `{
      "name":"parent",
      "tasks":[{"id":"run",
        "action":{"type":"child","name":"forwarder","result_schema":{}},
        "switch":[{"goto":"end"}],
        "output":{"a":"$: self.result.answer"}}],
      "output":{"a":"$: outputs.run.a"}
    }`)
	if err == nil || !strings.Contains(err.Error(), "the value is unknown") {
		t.Fatalf("want the unknown-read refusal, got: %v", err)
	}
}

// The narrowing privilege belongs to result_schema alone, because that is the one slot
// with a runtime conform behind it. An unknown handed to a typed child input has no such
// check, so it is refused — the same refusal TypeScript makes for unknown → T.
func TestUnknown_TypedChildInputStillRejectsUnknown(t *testing.T) {
	child := unknownDef(t, forwardingChildJSON)
	consumer := unknownDef(t, `{
      "name":"consumer",
      "input_schema":{"type":"object","properties":{"answer":{"type":"number"}},"required":["answer"]},
      "tasks":[{"id":"t","switch":[{"goto":"end"}]}]
    }`)
	parent := unknownDef(t, `{
      "name":"parent",
      "tasks":[
        {"id":"run",
         "action":{"type":"child","name":"forwarder","result_schema":{"type":"object","properties":{"payload":{"description":"opaque"}},"required":["payload"]}},
         "switch":[{"goto":"next"}],
         "output":{"payload":"$: self.result.payload"}},
        {"id":"use",
         "action":{"type":"child","name":"consumer","input":"$: outputs.run.payload"},
         "switch":[{"goto":"end"}]}],
      "output":{"a":"$: 1"}
    }`)
	err := validation.ValidateChildProcessRefs(parent, 1, stubGetter{"forwarder": child, "consumer": consumer})
	if err == nil || !strings.Contains(err.Error(), "input") {
		t.Fatalf("want an input incompatibility for unknown → typed input, got: %v", err)
	}
}
