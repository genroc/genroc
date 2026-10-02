package lsp

import (
	"strings"
	"testing"

	"genroc/internal/validation"
)

// tests/lsp/agreement_test.ts pins the two binaries at the SLOT; this pins a KEY inside one,
// in Go because the CLI's lookup is a function call here.
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
	// shapeKeyHover's two steps: the slot as `genctl schema type` prints it, then the member off
	// it. Not the key's own address — `SlotAt` reads an optional member as nullable.
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

	// The premise, so both sides cannot be wrong together: the conform leaves `number`, may be
	// absent, not the expression's `number|null`.
	if s := prop.Summary(); s != "number" {
		t.Fatalf("premise gone: the slot's `v` reads %q; the repair this pins is the null removal", s)
	}
	if !parent.MayBeAbsent("v") {
		t.Fatal("premise gone: a removed null must leave the key may-be-absent")
	}
}
