package lsp

import (
	"strings"
	"testing"

	"genroc/internal/validation"
)

// The one claim the type unification makes, at the level where it was false: a KEY inside a
// shape hovers as the type validation computed for its slot, so the editor's answer is the
// CLI's own. For a day it was not — `genctl schema type` published the inferred side while
// hover applied a rule of its own, and one key read `number|null` in the CLI and `number` in
// the editor. tests/lsp/agreement_test.ts pins the two binaries at the SLOT; this is the key,
// in Go because the CLI's lookup is a function call here and a printed document out there.
//
//	 1 name: agree
//	 2 input_schema:
//	 3   type: object
//	 4   properties: { n: { type: [number, "null"] } }
//	 5 tasks:
//	 6   - id: t
//	 7     action:
//	 8       type: child
//	 9       name: nope
//	10       input:
//	11         v: '$: input.n'
//	12       input_schema: { type: object, properties: { v: { type: number, description: prose } } }
//	13     switch: [{ goto: end }]
//	14 output: { ok: true }
const agreeDoc = `name: agree
input_schema:
  type: object
  properties: { n: { type: [number, "null"] } }
tasks:
  - id: t
    action:
      type: child
      name: nope
      input:
        v: '$: input.n'
      input_schema: { type: object, properties: { v: { type: number, description: prose } } }
    switch: [{ goto: end }]
output: { ok: true }
`

func TestKeyHoverIsTheCLIsOwnAnswer(t *testing.T) {
	docs, err := parseDocuments(agreeDoc, "")
	if err != nil || len(docs) != 1 {
		t.Fatalf("parse: %v", err)
	}
	def, ok := docs[0].definition()
	if !ok {
		t.Fatal("the document did not decode")
	}
	slots, err := validation.TypeSlots(def)
	if err != nil {
		t.Fatalf("TypeSlots: %v", err)
	}
	// What `genctl schema type agree tasks.t.action.input` prints, and then the member read off
	// it — the two steps shapeKeyHover takes, so a rule added on EITHER side breaks this. Not the
	// key's own address: `SlotAt` walks it the way an expression would, so an optional member
	// comes back nullable there, which is the read view and not a description of the key.
	parent, found, err := validation.SlotAt(slots, "tasks.t.action.input")
	if err != nil || !found {
		t.Fatalf("the slot is not in the type view: %v", err)
	}
	parent = unwrap(parent)
	prop, declared := parent.Properties()["v"]
	if !declared {
		t.Fatal("the slot's type carries no `v`")
	}

	// Line 11 is `        v: '$: input.n'`; column 9 is inside `v`.
	md := hoverOf(t, agreeDoc, 11, 9)
	if want := "`" + prop.Summary() + "`"; !strings.Contains(md, want) {
		t.Errorf("hover %q does not carry the CLI's type %s", md, want)
	}
	if absent := parent.MayBeAbsent("v"); absent != strings.Contains(md, "**v?**") {
		t.Errorf("hover %q and the type view disagree about whether `v` may be absent (%v)", md, absent)
	}
	if !strings.Contains(md, "prose") {
		t.Errorf("hover %q lost the declaration's prose, which the conformed type carries", md)
	}

	// The premise, so this cannot pass by both sides being wrong together. The conform removes
	// the null and the key with it: `number`, may be absent — not the `number|null` the
	// expression types, which is what the CLI answered before the type was computed once.
	if s := prop.Summary(); s != "number" {
		t.Fatalf("premise gone: the slot's `v` reads %q; the repair this pins is the null removal", s)
	}
	if !parent.MayBeAbsent("v") {
		t.Fatal("premise gone: a removed null must leave the key may-be-absent")
	}
}
