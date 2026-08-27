package validationtest

import (
	"fmt"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// The error-channel analogue of child_output_type_test.go: the payload a code carries must
// narrow to the shape the CALLER declared for it under `raises`. Same relation, same
// soundness condition — Engine.raisedData conforms the payload against that very schema.
// specs/error-extensions.md §X2-c.

// raiser builds a child that raises `code` carrying `data` (nil = attaches nothing),
// normalised as the stored definition would be.
func raiser(t *testing.T, name, code string, data any) *model.ProcessDefinition {
	t.Helper()
	fault := &model.Fault{Code: code, Message: "m"}
	if data != nil {
		fault.Data = &model.Shape{Raw: data}
	}
	d := &model.ProcessDefinition{
		Name: name,
		Tasks: []*model.Task{{
			ID:     "go",
			Switch: model.SwitchMap{{Case: "true", Raise: fault}, {Goto: model.GotoEnd}},
		}},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize child %q: %v", name, err)
	}
	return d
}

// childActionParentRaises builds a parent whose standalone `child` action declares `raises`.
// The optional input is for a child whose input_schema requires one.
func childActionParentRaises(t *testing.T, childName string, raises model.Raises, input ...*model.Shape) *model.ProcessDefinition {
	t.Helper()
	action := &model.Action{Type: model.ActionTypeChild, Name: childName, Raises: raises}
	if len(input) == 1 {
		action.Input = input[0]
	}
	d := &model.ProcessDefinition{
		Name:  "parent",
		Tasks: []*model.Task{{ID: "spawn", Action: action, Switch: model.SwitchMap{{Goto: model.GotoEnd}}}},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize parent: %v", err)
	}
	return d
}

const declineShape = `{"type":"object","properties":{"why":{"type":"string"}},"required":["why"]}`

func raisesOf(t *testing.T, code, raw string) model.Raises {
	t.Helper()
	if raw == "" {
		return model.Raises{code: nil}
	}
	return model.Raises{code: normalizedSchema(t, raw)}
}

// ── the shape relation ───────────────────────────────────────────────────────────────────

func TestChildRaises_MatchingObjectAccepted(t *testing.T) {
	child := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`})
	assertValidateOK(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", declineShape)),
		stubGetter{"kid": child})
}

func TestChildRaises_StringVsObjectRejected(t *testing.T) {
	child := raiser(t, "kid", "declined", `$: "just a string"`)
	assertValidateErr(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", declineShape)),
		stubGetter{"kid": child}, "string is not accepted where object is expected")
}

func TestChildRaises_NoDataIsNullNotAbsent(t *testing.T) {
	// setErrorData CLEARS the slot, so the caller conforms null — which an object shape does
	// not admit. Typing this as "nothing to check" is the bug the check exists for.
	child := raiser(t, "kid", "declined", nil)
	assertValidateErr(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", declineShape)),
		stubGetter{"kid": child}, "null is not accepted where object is expected")
}

func TestChildRaises_MissingRequiredFieldRejected(t *testing.T) {
	child := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`})
	rs := `{"type":"object","properties":{"why":{"type":"string"},"code":{"type":"string"}},"required":["why","code"]}`
	assertValidateErr(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", rs)),
		stubGetter{"kid": child}, "code: declared required, never set")
}

// Subset, not exact match — the same two relaxations the success channel has.
func TestChildRaises_ChildAttachesMoreAccepted(t *testing.T) {
	child := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`, "extra": `$: "dropped"`})
	assertValidateOK(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", declineShape)),
		stubGetter{"kid": child})
}

func TestChildRaises_OptionalNotYetAttachedAccepted(t *testing.T) {
	child := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`})
	rs := `{"type":"object","properties":{"why":{"type":"string"},"code":{"type":"string"}},"required":["why"]}`
	assertValidateOK(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", rs)),
		stubGetter{"kid": child})
}

// ── the three declaration states ─────────────────────────────────────────────────────────

func TestChildRaises_TopTypeDeclarationAlwaysFits(t *testing.T) {
	// `{}` narrows nothing: the caller is carrying the payload whole, not reading it.
	child := raiser(t, "kid", "declined", `$: "just a string"`)
	assertValidateOK(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", `{}`)),
		stubGetter{"kid": child})
}

func TestChildRaises_NullDeclarationSkipsTheShapeCheck(t *testing.T) {
	// `raises: {code: null}` says the code carries nothing for a rule to read, so collect
	// conforms nothing — there is no shape here to disagree with, whatever the child attaches.
	child := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`})
	assertValidateOK(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", "")),
		stubGetter{"kid": child})
}

func TestChildRaises_UndeclarableCodeStillRefused(t *testing.T) {
	child := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`})
	assertValidateErr(t, childActionParentRaises(t, "kid", raisesOf(t, "expired", declineShape)),
		stubGetter{"kid": child}, "never raises")
}

// ── a payload built from another definition's declaration ────────────────────────────────

// forwarder raises `outer` carrying, verbatim, the payload it declared for its own child's
// `inner` — a wrapper passing a refusal up. The type has to survive that hop, or a caller two
// levels up could declare anything.
func forwarder(t *testing.T, name, childName string) *model.ProcessDefinition {
	t.Helper()
	d := &model.ProcessDefinition{
		Name: name,
		Tasks: []*model.Task{{
			ID: "call",
			Action: &model.Action{Type: model.ActionTypeChild, Name: childName,
				Raises: model.Raises{"inner": normalizedSchema(t, declineShape)}},
			OnError: []model.ErrorCase{{
				Code:  []string{"inner"},
				Raise: &model.Fault{Code: "outer", Message: "m", Data: &model.Shape{Raw: "$: error.data"}},
			}},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		}},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize forwarder: %v", err)
	}
	return d
}

func TestChildRaises_ForwardedPayloadKeepsItsTypeAcrossTheHop(t *testing.T) {
	kid := raiser(t, "kid", "inner", map[string]any{"why": `$: "card"`})
	mid := forwarder(t, "mid", "kid")
	reg := stubGetter{"kid": kid, "mid": mid}

	sf, err := validation.Generate(mid)
	if err != nil {
		t.Fatalf("generate forwarder: %v", err)
	}
	if got := jsonString(t, sf.Raises["outer"]); got == "" || got == "{}" {
		t.Fatalf("the forwarded payload must keep its declared type, got %s", got)
	}

	assertValidateOK(t, childActionParentRaises(t, "mid", raisesOf(t, "outer", declineShape)), reg)

	tighter := `{"type":"object","properties":{"why":{"type":"string"},"code":{"type":"string"}},"required":["why","code"]}`
	assertValidateErr(t, childActionParentRaises(t, "mid", raisesOf(t, "outer", tighter)), reg,
		"code: declared required, never set")
}

// A recursive payload is the one that arrives as a $ref into the definition's own $defs, so it
// is only comparable if checkDeclaredRaises re-attaches the pool — and the relation's cycle
// guard is what stops the walk. Both are silent failures: a lost pool reads as "unknown", which
// NARROWS to anything, so the check would pass everything.
// Self-contained: normalizedSchema resolves each document on its own, the way a stored
// definition carries the definitions it uses baked into its own root $defs.
const recursiveDefs = `"$defs":{"node":{"type":"object","properties":{"v":{"type":"string"},` +
	`"kid":{"$ref":"#/$defs/node"}},"required":["v"]}}`
const recursiveNode = `{"$ref":"#/$defs/node",` + recursiveDefs + `}`

func recursiveRaiser(t *testing.T, name string) *model.ProcessDefinition {
	t.Helper()
	d := &model.ProcessDefinition{
		Name:        name,
		InputSchema: normalizedSchema(t, `{"type":"object","properties":{"tree":{"$ref":"#/$defs/node"}},"required":["tree"],`+recursiveDefs+`}`),
		Tasks: []*model.Task{{
			ID: "go",
			Switch: model.SwitchMap{
				{Case: "true", Raise: &model.Fault{Code: "declined", Message: "m", Data: &model.Shape{Raw: "$: input.tree"}}},
				{Goto: model.GotoEnd},
			},
		}},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return d
}

func TestChildRaises_RecursivePayloadComparesThroughItsPool(t *testing.T) {
	child := recursiveRaiser(t, "kid")
	sf, err := validation.Generate(child)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !sf.Raises["declined"].HasRef() {
		t.Fatalf("a recursive payload must stay a $ref, got %s — the pool test below is then vacuous",
			jsonString(t, sf.Raises["declined"]))
	}
	reg := stubGetter{"kid": child}
	tree := &model.Shape{Raw: map[string]any{"tree": map[string]any{"v": `$: "x"`}}}
	assertValidateOK(t, childActionParentRaises(t, "kid", raisesOf(t, "declined", recursiveNode), tree), reg)
	assertValidateErr(t, childActionParentRaises(t, "kid", raisesOf(t, "declined",
		`{"type":"object","properties":{"v":{"type":"integer"}},"required":["v"]}`), tree), reg, "v: string → integer")
}

// ── a known imprecision, inherited ───────────────────────────────────────────────────────

// The context at a task COLLAPSES the paths into it, so an output set on every branch of a
// join is still merely optional there and `a ?? b` types nullable. A payload built that way
// gets a null arm it can never carry at runtime, and a caller declaring the non-null shape is
// refused. specs/path-sensitive-output.md — the process output slot recovers this with a
// per-terminal walk, and a raise clause has no equivalent.
//
// Pinned rather than fixed: the remedy is the one every other read in the collapsed context
// takes (`?? ""`, or declaring the slot nullable), and the break names the slot and both types.
func TestChildRaises_JoinedBranchPayloadTypesNullable(t *testing.T) {
	child := &model.ProcessDefinition{
		Name:        "kid",
		InputSchema: normalizedSchema(t, `{"type":"object","properties":{"go":{"type":"boolean"}}}`),
		Tasks: []*model.Task{
			{ID: "pick", Switch: model.SwitchMap{{Case: "input.go ?? false", Goto: "$a"}, {Goto: "$b"}}},
			{ID: "a", Output: &model.Shape{Raw: map[string]any{"v": `$: "x"`}}, Switch: model.SwitchMap{{Goto: "$join"}}},
			{ID: "b", Output: &model.Shape{Raw: map[string]any{"v": `$: "y"`}}, Switch: model.SwitchMap{{Goto: "$join"}}},
			{ID: "join", Switch: model.SwitchMap{
				{Case: "true", Raise: &model.Fault{Code: "declined", Message: "m",
					Data: &model.Shape{Raw: map[string]any{"v": "$: outputs.a.v ?? outputs.b.v"}}}},
				{Goto: model.GotoEnd},
			}},
		},
	}
	if err := child.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	reg := stubGetter{"kid": child}
	assertValidateErr(t, childActionParentRaises(t, "kid",
		raisesOf(t, "declined", `{"type":"object","properties":{"v":{"type":"string"}},"required":["v"]}`)), reg,
		"v: null|string → string")
	// Declaring what the collapse actually says is what registers.
	assertValidateOK(t, childActionParentRaises(t, "kid",
		raisesOf(t, "declined", `{"type":"object","properties":{"v":{"type":["string","null"]}},"required":["v"]}`)), reg)
}

// ── the other two child action types ─────────────────────────────────────────────────────

func TestChildRaises_ChildMapChecksEachEntryAgainstItsOwnChild(t *testing.T) {
	// Entries can be different processes, so a declaration is judged against the entry's own
	// child — never the union across the map.
	good := raiser(t, "good", "declined", map[string]any{"why": `$: "card"`})
	bad := raiser(t, "bad", "declined", `$: "just a string"`)
	shape := normalizedSchema(t, declineShape)

	parent := func(t *testing.T, entries map[string]model.ChildEntry) *model.ProcessDefinition {
		d := &model.ProcessDefinition{
			Name: "parent",
			Tasks: []*model.Task{{
				ID:     "spawn",
				Action: &model.Action{Type: model.ActionTypeChildMap, Children: entries},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			}},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("normalize parent: %v", err)
		}
		return d
	}

	reg := stubGetter{"good": good, "bad": bad}
	assertValidateOK(t, parent(t, map[string]model.ChildEntry{
		"a": {Name: "good", Raises: model.Raises{"declined": shape}},
		"b": {Name: "bad"},
	}), reg)
	assertValidateErr(t, parent(t, map[string]model.ChildEntry{
		"a": {Name: "good", Raises: model.Raises{"declined": shape}},
		"b": {Name: "bad", Raises: model.Raises{"declined": shape}},
	}), reg, `children["b"]`)
}

func TestChildRaises_ChildListDeclaresOnTheAction(t *testing.T) {
	bad := raiser(t, "bad", "declined", `$: "just a string"`)
	d := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{{
			ID: "fan",
			Action: &model.Action{
				Type: model.ActionTypeChildList, Name: "bad", Over: `$: [1, 2]`,
				Raises: model.Raises{"declined": normalizedSchema(t, declineShape)},
			},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		}},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize parent: %v", err)
	}
	assertValidateErr(t, d, stubGetter{"bad": bad}, "string is not accepted where object is expected")
}

// ── self-reference ───────────────────────────────────────────────────────────────────────

// A recursive process resolves its own call without a lookup, so the check infers the very
// definition it is validating. Nothing may be left mutated behind it.
func TestChildRaises_SelfReferenceIsCheckedAgainstItself(t *testing.T) {
	def := func(t *testing.T, declared string) *model.ProcessDefinition {
		d := &model.ProcessDefinition{
			Name: "loop",
			Tasks: []*model.Task{
				{
					ID:     "recurse",
					Action: &model.Action{Type: model.ActionTypeChild, Name: "loop", Raises: raisesOf(t, "declined", declared)},
					Switch: model.SwitchMap{{Goto: "$stop"}},
				},
				{
					ID: "stop",
					Switch: model.SwitchMap{{Case: "true",
						Raise: &model.Fault{Code: "declined", Message: "m", Data: &model.Shape{Raw: map[string]any{"why": `$: "card"`}}}},
						{Goto: model.GotoEnd}},
				},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		return d
	}
	assertValidateOK(t, def(t, declineShape), stubGetter{})

	tighter := `{"type":"object","properties":{"why":{"type":"string"},"code":{"type":"string"}},"required":["why","code"]}`
	assertValidateErr(t, def(t, tighter), stubGetter{}, "code: declared required, never set")
}

// ── version pinning ──────────────────────────────────────────────────────────────────────

// A pinned version is the one checked: a stored v1 whose payload the caller fits stays valid
// however v2 was retyped.
func TestChildRaises_PinnedVersionIsTheOneChecked(t *testing.T) {
	v1 := raiser(t, "kid", "declined", map[string]any{"why": `$: "card"`})
	v2 := raiser(t, "kid", "declined", map[string]any{"why": `$: 51`})
	byVersion := versionGetter{1: v1, 2: v2}

	pinned := func(t *testing.T, v int) *model.ProcessDefinition {
		d := &model.ProcessDefinition{
			Name: "parent",
			Tasks: []*model.Task{{
				ID: "spawn",
				Action: &model.Action{Type: model.ActionTypeChild, Name: "kid", Version: v,
					Raises: raisesOf(t, "declined", declineShape)},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			}},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("normalize: %v", err)
		}
		return d
	}
	assertValidateOK(t, pinned(t, 1), byVersion)
	assertValidateErr(t, pinned(t, 2), byVersion, "integer")
	// Version 0 is "latest", which this registry answers with v2.
	assertValidateErr(t, pinned(t, 0), byVersion, "integer")
}

type versionGetter map[int]*model.ProcessDefinition

func (g versionGetter) GetDefinition(_ string, version int) (*model.ProcessDefinition, error) {
	d, ok := g[version]
	if !ok {
		return nil, fmt.Errorf("kid v%d not found", version)
	}
	return d, nil
}
func (g versionGetter) LatestVersion(string) (int, error) { return 2, nil }
