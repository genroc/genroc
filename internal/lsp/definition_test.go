package lsp

import "testing"

//	1 name: demo
//	2 tasks:
//	3   - id: first
//	4     switch: "$second"
//	5   - id: second
//	6     switch:
//	7       - case: "$: true"
//	8         goto: "$first"
//	9       - goto: end
//
// 10   - id: third
// 11     switch: next
const routingDoc = `name: demo
tasks:
  - id: first
    switch: "$second"
  - id: second
    switch:
      - case: "$: true"
        goto: "$first"
      - goto: end
  - id: third
    switch: next
`

func TestGotoOnAScalarSwitchJumpsToTheTask(t *testing.T) {
	r, ok := definitionAt(routingDoc, 4, 15) // inside "$second"
	if !ok {
		t.Fatal("a `$task-id` names a task and must resolve")
	}
	if r.Line != 5 {
		t.Errorf("`second` is defined on line 5, got %d", r.Line)
	}
}

func TestGotoInsideASwitchCaseJumpsToTheTask(t *testing.T) {
	r, ok := definitionAt(routingDoc, 8, 18) // inside "$first"
	if !ok {
		t.Fatal("a goto inside a case must resolve")
	}
	if r.Line != 3 {
		t.Errorf("`first` is defined on line 3, got %d", r.Line)
	}
}

// `end` terminates and `next` is positional, so neither names anything to jump to. Answering
// with a location anyway would send the reader somewhere arbitrary.
func TestEndAndNextHaveNoDefinition(t *testing.T) {
	if _, ok := definitionAt(routingDoc, 9, 17); ok {
		t.Error("`end` terminates the instance; it names no task")
	}
	if _, ok := definitionAt(routingDoc, 11, 15); ok {
		t.Error("`next` is positional; it names no task")
	}
}

func TestAGotoNamingNoTaskResolvesToNothing(t *testing.T) {
	broken := "name: demo\ntasks:\n  - id: a\n    switch: \"$nope\"\n"
	if _, ok := definitionAt(broken, 4, 15); ok {
		t.Error("a goto to a task that does not exist must not resolve somewhere else")
	}
}

// The guard that earns its place: a URL may legitimately hold a string starting with `$`, and
// only a ROUTING slot means "the task named here". Resolving on value shape alone would jump
// out of an unrelated field.
func TestOnlyARoutingSlotResolvesEvenWhenTheValueLooksLikeOne(t *testing.T) {
	//	 5   - id: second
	//	 6     action:
	//	 7       type: fetch
	//	 8       url: "$second"
	doc := "name: demo\ntasks:\n  - id: first\n    switch: end\n  - id: second\n    action:\n" +
		"      type: fetch\n      url: \"$first\"\n    switch: end\n"
	if _, ok := definitionAt(doc, 8, 15); ok {
		t.Error("a url holding `$first` is a string, not a task reference")
	}
}

// `$` is the reference sigil. A bare name in a switch is not a reference — mid-edit it is a
// value the decoder will refuse, and jumping from it would invent a meaning it does not have.
func TestABareNameInASwitchIsNotAReference(t *testing.T) {
	doc := "name: demo\ntasks:\n  - id: first\n    switch: second\n  - id: second\n    switch: end\n"
	if _, ok := definitionAt(doc, 4, 14); ok {
		t.Error("`switch: second` names no task; a reference is spelled `$second`")
	}
}

// A cursor on ordinary text is the common case, and a spurious jump is worse than none.
func TestACursorOnSomethingElseHasNoDefinition(t *testing.T) {
	if _, ok := definitionAt(routingDoc, 1, 7); ok {
		t.Error("the process name is not a reference")
	}
	if _, ok := definitionAt(routingDoc, 7, 20); ok {
		t.Error("a case expression is not a reference")
	}
}

func TestDefinitionIsAdvertisedAndAnswered(t *testing.T) {
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(uri, routingDoc),
		frame("textDocument/definition", 2, map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     map[string]any{"line": 3, "character": 14}, // inside "$second"
		}),
		frame("exit", nil, nil))

	var res initializeResult
	if err := jsonUnmarshal(msgs[0]["result"], &res); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !res.Capabilities.DefinitionProvider {
		t.Error("a server that resolves references must say so")
	}

	var loc location
	if err := jsonUnmarshal(msgs[len(msgs)-1]["result"], &loc); err != nil {
		t.Fatalf("definition result: %v", err)
	}
	if loc.URI != uri || loc.Range.Start.Line != 4 {
		t.Errorf("want the `second` task on line 5 (0-based 4), got %+v", loc)
	}
}
