package validation

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/shape"
)

// One definition reaching every phase: an action with typed responses (so `self.status` and
// `self.headers` exist), an output map, a switch, two on_error rules with different declared
// payloads, a handler an error edge enters, a loop back to it, and a config namespace.
const slotFixture = `{
  "name": "p",
  "input_schema": {"type": "object", "properties": {"amount": {"type": "number"}}, "required": ["amount"]},
  "config_schema": {"type": "object", "properties": {"base": {"type": "string"}}, "required": ["base"]},
  "tasks": [
    {"id": "call",
     "action": {"type": "fetch", "url": "${config.base}/x", "method": "POST",
                "body": {"amount": "$: input.amount"},
                "responses": {"200": {"type": "object", "properties": {"fee": {"type": "number"}}, "required": ["fee"]},
                              "429": {"type": "object", "properties": {"wait": {"type": "number"}}, "required": ["wait"]},
                              "500": {"type": "object", "properties": {"why": {"type": "string"}}, "required": ["why"]}}},
     "output": {"fee": "$: self.result.fee"},
     "on_error": [{"code": ["http.429"], "retry": {"attempts": 2, "delay": "$: error.data.wait"}},
                  {"code": ["http.500"], "goto": "$handler"}],
     "switch": [{"case": "self.output.fee > 0", "goto": "end"}, {"goto": "end"}]},
    {"id": "handler",
     "output": {"why": "$: last_error.code"},
     "switch": [{"case": "outputs.handler != null", "goto": "end"}, {"goto": "$handler"}]}
  ],
  "output": {"fee": "$: outputs.call.fee ?? 0"}
}`

// The contexts `genctl schema context` reports are the ones the checker used, and the way that
// is guaranteed is that both go through taskScopes. What could still drift is what each FEEDS
// it: SlotContexts reads a finished SchemaFile (post-hoist defs, sf.Tasks, sf.ProcessInput,
// buildConfigSchema) where buildInputs reads the pool mid-flight. This runs the checker's own
// preparation and compares slot for slot, so a substitution that stops meaning the same thing
// fails here rather than being reported to an author as a context nothing checked.
func TestSlotContextsAreTheCheckersOwn(t *testing.T) {
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(slotFixture), &def); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	reported, err := SlotContexts(&def)
	if err != nil {
		t.Fatalf("SlotContexts: %v", err)
	}

	// buildInputs' own preparation, in its order: the pool as the check sees it, outputs
	// inferred in phase 1 so every context below reads final types.
	defs, tasks, processInput, configSchema, err := buildSchemaContext(&def)
	if err != nil {
		t.Fatalf("buildSchemaContext: %v", err)
	}
	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, defs)
	checker := taskScopes{
		tasks: tasks, processInput: processInput, configSchema: configSchema, defs: defs,
		required: required, optional: optional, errs: errs,
	}
	if err := inferOutputs(def.Tasks, checker); err != nil {
		t.Fatalf("inferOutputs: %v", err)
	}

	// The two agree on the context and on every definition it can reach. They are NOT
	// byte-identical: SlotContexts reads the pool after Generate hoisted the task inputs and
	// the process output into it, so the reported pool is a superset. That is the claim being
	// tested — it may GROW, but a name that resolved during the check must still resolve to
	// the same thing, or a reported `$ref` would mean something the author was never checked
	// against.
	same := func(address string, want schema.Schema) {
		t.Helper()
		got, ok := reported[address]
		if !ok {
			t.Fatalf("%s is not addressable, so nothing reports the context the checker used there", address)
		}
		sameContext(t, address, got, want)
	}

	for _, task := range def.Tasks {
		same("tasks."+task.ID+".action", checker.action(task))

		outputCtx, _, err := checker.outputMap(task)
		if err != nil {
			t.Fatalf("outputMap %s: %v", task.ID, err)
		}
		same("tasks."+task.ID+".output", outputCtx)

		switchCtx, err := checker.switchScope(task)
		if err != nil {
			t.Fatalf("switchScope %s: %v", task.ID, err)
		}
		same("tasks."+task.ID+".switch", switchCtx)

		for i, ec := range task.OnError {
			same(ruleSlot(task.ID, i), checker.rule(task, ec))
		}
	}

	same(SlotProcessOutput, checker.processOutputContext(&def))

	// Every phase of both tasks, and nothing else: a phase that stops being addressable is a
	// slot an author can no longer ask about, which no other test would notice.
	if len(reported) != 9 {
		t.Errorf("addresses = %d, want 9 (four phases across two tasks, plus the process output)", len(reported))
	}
}

// The process output's context is one arm per way the process can end, and the arms are what
// carry the correlation between path-exclusive outputs: where a path does not set one, its arm
// types it null. That is why `(outputs.left.v ?? outputs.right.v) + 1` types — under each arm
// the recovery is non-null. It used to be refused here while the checker accepted it, because
// the checker walked the paths itself and the context it handed out had been flattened.
func TestProcessOutputContextCarriesEveryEnding(t *testing.T) {
	const twoPaths = `{
	  "name": "p",
	  "input_schema": {"type": "object", "properties": {"go": {"type": "boolean"}}, "required": ["go"]},
	  "tasks": [
	    {"id": "pick", "switch": [{"case": "input.go", "goto": "$left"}, {"goto": "$right"}]},
	    {"id": "left", "output": {"v": "$: 1"}, "switch": [{"goto": "end"}]},
	    {"id": "right", "output": {"v": "$: 2"}, "switch": [{"goto": "end"}]}
	  ],
	  "output": {"v": "$: outputs.left.v ?? outputs.right.v"}
	}`
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(twoPaths), &def); err != nil {
		t.Fatalf("parse: %v", err)
	}
	slots, err := SlotContexts(&def)
	if err != nil {
		t.Fatalf("SlotContexts: %v", err)
	}
	ctx := slots[SlotProcessOutput]

	// One arm per ending, each naming its own, so a failure under one can say which.
	var doc struct {
		AnyOf []struct {
			Description string `json:"description"`
		} `json:"anyOf"`
	}
	if err := json.Unmarshal([]byte(marshal(t, ctx)), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.AnyOf) != 2 {
		t.Fatalf("arms = %d, want one per terminal", len(doc.AnyOf))
	}
	for _, arm := range doc.AnyOf {
		if !strings.Contains(arm.Description, "ending at task") {
			t.Errorf("an arm must name its ending, got %q", arm.Description)
		}
	}

	for _, tc := range []struct {
		expr     string
		accepted bool
		why      string
	}{
		{"outputs.left.v", true, "a path-exclusive output is readable"},
		{"outputs.left.v ?? outputs.right.v", true, "recovering from the branch not taken"},
		{"outputs.left.v + 1", false, "unrecovered, it is null on the path that skips it"},
		{"(outputs.left.v ?? outputs.right.v) + 1", true, "under each arm the recovery is non-null"},
		{"outputs.nope.v", false, "a task no terminal reaches is absent, not null"},
	} {
		shp := shape.Shape{Raw: tc.expr, Name: "output", Expr: true}
		_, err := shp.Check(ctx)
		if accepted := err == nil; accepted != tc.accepted {
			t.Errorf("%s: accepted = %v, want %v (%s)\n  %v", tc.expr, accepted, tc.accepted, tc.why, err)
		}
	}
}

// sameContext holds the reported context to the checker's: the same context, and every
// definition it can reach resolving to the same thing.
func sameContext(t *testing.T, label string, got, want schema.Schema) {
	t.Helper()
	gotBody, gotPool := splitPool(t, got)
	wantBody, wantPool := splitPool(t, want)
	if gotBody != wantBody {
		t.Errorf("%s: reported context is not the checker's\n reported: %s\n checker:  %s",
			label, gotBody, wantBody)
	}
	for name, def := range wantPool {
		switch reported, ok := gotPool[name]; {
		case !ok:
			t.Errorf("%s: $defs/%s resolved during the check and is missing from the reported pool", label, name)
		case reported != def:
			t.Errorf("%s: $defs/%s is not what the check resolved\n reported: %s\n checker:  %s",
				label, name, reported, def)
		}
	}
}

// One ending, one context — and the join over a single terminal IS that path, so the reported
// answer is not merely the same scope there but the very context the checker walked. That is
// the common shape; the qualification in the test above only bites where a process branches.
func TestProcessOutputSingleTerminalIsTheCheckersPath(t *testing.T) {
	const oneEnd = `{
	  "name": "p",
	  "input_schema": {"type": "object", "properties": {"n": {"type": "number"}}, "required": ["n"]},
	  "tasks": [
	    {"id": "first", "output": {"v": "$: input.n"}, "switch": [{"goto": "next"}]},
	    {"id": "second", "output": {"w": "$: outputs.first.v"}, "switch": [{"goto": "end"}]}
	  ],
	  "output": {"v": "$: outputs.second.w"}
	}`
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(oneEnd), &def); err != nil {
		t.Fatalf("parse: %v", err)
	}
	reported, err := SlotContexts(&def)
	if err != nil {
		t.Fatalf("SlotContexts: %v", err)
	}

	defs, tasks, processInput, configSchema, err := buildSchemaContext(&def)
	if err != nil {
		t.Fatalf("buildSchemaContext: %v", err)
	}
	required, optional, mustErr, mayErr, errSrc := computeContextSets(def.Tasks)
	errs := errContexts(def.Tasks, mustErr, mayErr, errSrc, defs)
	checker := taskScopes{
		tasks: tasks, processInput: processInput, configSchema: configSchema, defs: defs,
		required: required, optional: optional, errs: errs,
	}
	if err := inferOutputs(def.Tasks, checker); err != nil {
		t.Fatalf("inferOutputs: %v", err)
	}
	terminals := outputTerminals(&def)
	if len(terminals) != 1 {
		t.Fatalf("terminals = %d, want 1 — the fixture is meant to have one ending", len(terminals))
	}
	everMay := map[string]bool{}
	for id := range terminals[0].may {
		everMay[id] = true
	}
	sameContext(t, "output", reported[SlotProcessOutput], checker.processOutputAt(terminals[0], everMay))
}

// splitPool separates a context document from the `$defs` it carries, each as JSON. The pool
// travels with every arm of a union, not only with the root, so this strips it wherever it
// appears and compares the definitions once.
func splitPool(t *testing.T, s schema.Schema) (body string, pool map[string]string) {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(marshal(t, s)), &doc); err != nil {
		t.Fatalf("decode context: %v", err)
	}
	pool = map[string]string{}
	stripped := stripDefs(t, doc, pool)
	rest, err := json.Marshal(stripped)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return string(rest), pool
}

func stripDefs(t *testing.T, v any, pool map[string]string) any {
	t.Helper()
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, sub := range node {
			if k == "$defs" {
				defs, _ := sub.(map[string]any)
				for name, def := range defs {
					pool[name] = marshal(t, def)
				}
				continue
			}
			out[k] = stripDefs(t, sub, pool)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, sub := range node {
			out[i] = stripDefs(t, sub, pool)
		}
		return out
	}
	return v
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The type view reports what the checker CHECKED. Nothing guarantees that by construction the
// way taskScopes does for contexts — TypeSlots reads a finished SchemaFile — so what could drift
// is a second computation of a value the checker already made. Every pair below is one value
// read twice: once as a type, once out of the context the checker built and typed expressions
// against. A `result` computed beside `self.result`, an `output` beside `outputs.<id>`, and this
// fails rather than an author generating a client from a shape nothing checked.
func TestTypeSlotsAreTheCheckersOwn(t *testing.T) {
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(slotFixture), &def); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	types, err := TypeSlots(&def)
	if err != nil {
		t.Fatalf("TypeSlots: %v", err)
	}
	contexts, err := SlotContexts(&def)
	if err != nil {
		t.Fatalf("SlotContexts: %v", err)
	}

	// A type is the same value the context shows at the path an expression would read it by.
	same := func(typeAddress, contextAddress, path string) {
		t.Helper()
		reported, ok := types[typeAddress]
		if !ok {
			t.Fatalf("%s is not addressable, so nothing reports the type there", typeAddress)
		}
		ctx, ok := contexts[contextAddress]
		if !ok {
			t.Fatalf("%s is not addressable", contextAddress)
		}
		checked, err := ctx.At(path)
		if err != nil {
			t.Fatalf("%s: %s is not readable there: %v", contextAddress, path, err)
		}
		sameContext(t, typeAddress+" vs "+contextAddress+"."+path, reported, checked)
	}

	// What the action hands back, as the output map reads it.
	same("tasks.call.result", "tasks.call.output", "self.result")
	// What the output map produces, as the switch reads it back.
	same("tasks.call.output", "tasks.call.switch", "self.output")
	same("tasks.handler.output", "tasks.handler.switch", "self.output")
	// The process input, as every expression reads it.
	same("input", "tasks.call.action", "input")
	// The payload of the failure that routed here. Read through the context it comes back
	// NULLABLE — `data` is optional there, because a failure may carry none — so the null the
	// read adds is stripped before comparing. What is being pinned is the payload either way.
	reportedErr, ok := types["tasks.handler.last_error"]
	if !ok {
		t.Fatal("tasks.handler.last_error is not addressable")
	}
	checkedErr, err := contexts["tasks.handler.action"].At("last_error.data")
	if err != nil {
		t.Fatalf("last_error.data is not readable at the handler: %v", err)
	}
	sameContext(t, "tasks.handler.last_error vs last_error.data", reportedErr, checkedErr.StripNull())
}
