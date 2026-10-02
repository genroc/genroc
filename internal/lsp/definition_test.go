package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Line numbers are asserted against this, so a change here moves the cases below.
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
	r, ok := definitionAt(routingDoc, "", 4, 15) // inside "$second"
	if !ok {
		t.Fatal("a `$task-id` names a task and must resolve")
	}
	if r.Line != 5 {
		t.Errorf("`second` is defined on line 5, got %d", r.Line)
	}
}

func TestGotoInsideASwitchCaseJumpsToTheTask(t *testing.T) {
	r, ok := definitionAt(routingDoc, "", 8, 18) // inside "$first"
	if !ok {
		t.Fatal("a goto inside a case must resolve")
	}
	if r.Line != 3 {
		t.Errorf("`first` is defined on line 3, got %d", r.Line)
	}
}

func TestEndAndNextHaveNoDefinition(t *testing.T) {
	if _, ok := definitionAt(routingDoc, "", 9, 17); ok {
		t.Error("`end` terminates the instance; it names no task")
	}
	if _, ok := definitionAt(routingDoc, "", 11, 15); ok {
		t.Error("`next` is positional; it names no task")
	}
}

func TestAGotoNamingNoTaskResolvesToNothing(t *testing.T) {
	broken := "name: demo\ntasks:\n  - id: a\n    switch: \"$nope\"\n"
	if _, ok := definitionAt(broken, "", 4, 15); ok {
		t.Error("a goto to a task that does not exist must not resolve somewhere else")
	}
}

func TestOnlyARoutingSlotResolvesEvenWhenTheValueLooksLikeOne(t *testing.T) {
	//	 8       method: post
	//	 9       url: "$first"
	doc := "name: demo\ntasks:\n  - id: first\n    switch: end\n  - id: second\n    action:\n" +
		"      type: fetch\n      method: post\n      url: \"$first\"\n    switch: end\n"
	if _, ok := definitionAt(doc, "", 8, 15); ok {
		t.Error("a url holding `$first` is a string, not a task reference")
	}
}

func TestABareNameInASwitchIsNotAReference(t *testing.T) {
	doc := "name: demo\ntasks:\n  - id: first\n    switch: second\n  - id: second\n    switch: end\n"
	if _, ok := definitionAt(doc, "", 4, 14); ok {
		t.Error("`switch: second` names no task; a reference is spelled `$second`")
	}
}

func TestACursorOnSomethingElseHasNoDefinition(t *testing.T) {
	if _, ok := definitionAt(routingDoc, "", 1, 7); ok {
		t.Error("the process name is not a reference")
	}
	if _, ok := definitionAt(routingDoc, "", 7, 20); ok {
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

const parentDoc = `name: parent
tasks:
  - id: spawn
    action:
      type: child
      name: worker
    switch: end
`

const workerDoc = `name: worker
tasks:
  - id: work
    switch: end
`

func TestAChildActionsProcessResolvesToTheFileThatDefinesIt(t *testing.T) {
	const parentURI = "file:///w/parent.genroc.yaml"
	const workerURI = "file:///w/worker.genroc.yaml"
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(workerURI, workerDoc),
		openDoc(parentURI, parentDoc),
		frame("textDocument/definition", 2, map[string]any{
			"textDocument": map[string]any{"uri": parentURI},
			"position":     map[string]any{"line": 5, "character": 12}, // on `worker`
		}),
		frame("exit", nil, nil))

	var loc location
	if err := jsonUnmarshal(msgs[len(msgs)-1]["result"], &loc); err != nil {
		t.Fatalf("definition result: %v", err)
	}
	if loc.URI != workerURI {
		t.Fatalf("want the file defining `worker`, got %q", loc.URI)
	}
	if loc.Range.Start.Line != 0 {
		t.Errorf("`name: worker` is on line 1 (0-based 0), got %d", loc.Range.Start.Line)
	}
}

func TestAProcessIsFoundOnDiskWhenNoBufferHasIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker.genroc.yaml"), []byte(workerDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	const parentURI = "file:///w/parent.genroc.yaml"
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{"rootUri": "file://" + dir}),
		openDoc(parentURI, parentDoc),
		frame("textDocument/definition", 2, map[string]any{
			"textDocument": map[string]any{"uri": parentURI},
			"position":     map[string]any{"line": 5, "character": 12},
		}),
		frame("exit", nil, nil))

	var loc location
	if err := jsonUnmarshal(msgs[len(msgs)-1]["result"], &loc); err != nil {
		t.Fatalf("definition result: %v", err)
	}
	if !strings.HasSuffix(loc.URI, "/worker.genroc.yaml") {
		t.Fatalf("want the file on disk, got %q", loc.URI)
	}
}

func TestAChildNamingAProcessThatDoesNotExistResolvesToNothing(t *testing.T) {
	const parentURI = "file:///w/parent.genroc.yaml"
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(parentURI, parentDoc),
		frame("textDocument/definition", 2, map[string]any{
			"textDocument": map[string]any{"uri": parentURI},
			"position":     map[string]any{"line": 5, "character": 12},
		}),
		frame("exit", nil, nil))
	if raw := string(msgs[len(msgs)-1]["result"]); raw != "null" {
		t.Errorf("nothing defines `worker`, so there is nowhere to go; got %s", raw)
	}
}

func TestTheDefinitionsOwnNameIsNotAReference(t *testing.T) {
	if _, ok := definitionAt(parentDoc, "", 1, 8); ok {
		t.Error("the process's own name names itself")
	}
}

func TestANameThatIsNotAChildActionsIsNotAReference(t *testing.T) {
	//	 5     output:
	//	 6       name: worker
	doc := "name: parent\ntasks:\n  - id: a\n    switch: end\n    output:\n      name: worker\n"
	const parentURI = "file:///w/parent.genroc.yaml"
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc("file:///w/worker.genroc.yaml", workerDoc),
		openDoc(parentURI, doc),
		frame("textDocument/definition", 2, map[string]any{
			"textDocument": map[string]any{"uri": parentURI},
			"position":     map[string]any{"line": 5, "character": 12},
		}),
		frame("exit", nil, nil))
	if raw := string(msgs[len(msgs)-1]["result"]); raw != "null" {
		t.Errorf("an output field named `name` is data, not a reference; got %s", raw)
	}
}

func TestAChildMapEntrysProcessResolves(t *testing.T) {
	//	 4     action:
	//	 5       type: child_map
	//	 6       children:
	//	 7         left:
	//	 8           name: worker
	doc := "name: parent\ntasks:\n  - id: fan\n    action:\n      type: child_map\n" +
		"      children:\n        left:\n          name: worker\n    switch: end\n"
	const parentURI = "file:///w/parent.genroc.yaml"
	const workerURI = "file:///w/worker.genroc.yaml"
	msgs, _ := session(t,
		frame("initialize", 1, map[string]any{}),
		openDoc(workerURI, workerDoc),
		openDoc(parentURI, doc),
		frame("textDocument/definition", 2, map[string]any{
			"textDocument": map[string]any{"uri": parentURI},
			"position":     map[string]any{"line": 7, "character": 17},
		}),
		frame("exit", nil, nil))

	var loc location
	if err := jsonUnmarshal(msgs[len(msgs)-1]["result"], &loc); err != nil {
		t.Fatalf("definition result: %v", err)
	}
	if loc.URI != workerURI {
		t.Errorf("a child_map entry names a process too; got %q", loc.URI)
	}
}
