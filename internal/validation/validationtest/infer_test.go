package validationtest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// Inference tests: what Generate derives for a task's input, a task's output and
// the switch expressions in between. The definitions are assembled by the
// inferTask / inferDef builders in mapcases_test.go, so each test shows only the
// fields that matter to it.

// --- task input inference ---------------------------------------------------

func TestGenerate_Input_FirstTaskNoInput(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{id: "charge", result: inferObjSchema(`"ok":{"type":"boolean"}`), output: inferSelfResult},
	))
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_WithProcessInput(t *testing.T) {
	out := runGenerate(t, inferDef(inferObjSchema(`"order_id":{"type":"integer"}`),
		inferTask{id: "charge", result: inferObjSchema(`"ok":{"type":"boolean"}`), output: inferSelfResult},
	))
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_PrecedingTaskOutput(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{id: "charge", result: inferObjSchema(`"charged":{"type":"boolean"}`), sw: inferNext, output: inferSelfResult},
		inferTask{id: "notify", result: inferObjSchema(`"sent":{"type":"boolean"}`), sw: inferEnd, output: inferSelfResult},
	))
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_TaskWithNoOutputSkippedInContext(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{id: "log", fetch: true, sw: inferNext},
		inferTask{id: "notify", result: inferObjSchema(`"sent":{"type":"boolean"}`), sw: inferEnd, output: inferSelfResult},
	))
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_SwitchOnlyStepSkippedInContext(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{id: "charge", result: inferObjSchema(`"charged":{"type":"boolean"}`), sw: inferNext, output: inferSelfResult},
		inferTask{id: "route", sw: `[{"case":"outputs.charge.charged == true","goto":"$ship"},{"goto":"end"}]`},
		inferTask{id: "ship", result: inferObjSchema(`"tracking":{"type":"string"}`), output: inferSelfResult},
	))
	if _, ok := out.Tasks["route"]; ok {
		t.Error("switch-only task should not appear in tasks")
	}
	if out.Tasks["ship"].Output.IsZero() {
		t.Error("ship should have an output schema")
	}
}

func TestGenerate_Input_Params(t *testing.T) {
	out := runGenerate(t, inferDef(
		inferObjSchema(`"order_id":{"type":"integer"},"amount":{"type":"number"}`, "order_id", "amount"),
		inferTask{
			id:     "charge",
			result: inferObjSchema(`"ok":{"type":"boolean"}`),
			body:   `{"id":"$: input.order_id","sum":"$: input.amount"}`,
			output: inferSelfResult,
		},
	))
	assertJSON(t, out.Tasks["charge"].Input, `{"$ref": "#/$defs/charge_input"}`)
	input := defOf(out, "charge_input")
	if input.IsZero() {
		t.Fatal("charge_input not found in defs")
	}
	if !input.Type().Contains("object") {
		t.Errorf("input type: got %v, want object", input.Type())
	}
	assertJSON(t, input.Properties()["id"], `{"type": "integer"}`)
	assertJSON(t, input.Properties()["sum"], `{"type": "number"}`)
}

func TestGenerate_Input_InputOnlyTask(t *testing.T) {
	out := runGenerate(t, inferDef(inferObjSchema(`"user_id":{"type":"string"}`, "user_id"),
		inferTask{id: "log", body: `{"uid":"$: input.user_id"}`},
	))
	if _, ok := out.Tasks["log"]; !ok {
		t.Fatal("task with action input but no result_schema should appear in tasks")
	}
	assertJSON(t, out.Tasks["log"].Input, `{"$ref": "#/$defs/log_input"}`)
	assertJSON(t, defOf(out, "log_input"), `{
		"type": "object",
		"properties": { "uid": { "type": "string" } },
		"required": ["uid"]
	}`)
}

func TestGenerate_Input_OneOfOutputPropertyAccess(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{
			id:     "save_order",
			result: `{"oneOf":[{"type":"object","properties":{"valid":{"type":"boolean"}}},{"type":"string"}]}`,
			sw:     inferNext,
			output: inferSelfResult,
		},
		inferTask{id: "check_fraud", body: `{"result":"$: outputs.save_order.valid"}`, sw: inferEnd},
	))
	assertJSON(t, out.Tasks["check_fraud"].Input, `{"$ref": "#/$defs/check_fraud_input"}`)
	cfInput := defOf(out, "check_fraud_input")
	if cfInput.IsZero() {
		t.Fatal("check_fraud_input not found in defs")
	}
	assertJSON(t, cfInput.Properties()["result"], `{"type":["boolean","null"]}`)
}

func TestGenerate_RecursiveStep_OwnOutputOptionalInParams(t *testing.T) {
	out := runGenerate(t, inferDef(inferObjSchema(`"tasks":{"type":"array","items":{"type":"string"}}`, "tasks"),
		inferTask{
			id:     "loop",
			result: inferObjSchema(`"finished_index":{"type":"number"},"done":{"type":"boolean"}`, "finished_index", "done"),
			body:   `{"tasks":"$: input.tasks","task_index":"$: outputs.loop.finished_index ? outputs.loop.finished_index : 0"}`,
			sw:     `[{"case":"!self.result.done","goto":"$loop"},{"goto":"end"}]`,
			output: inferSelfResult,
		},
	))
	assertJSON(t, out.Tasks["loop"].Input, `{"$ref": "#/$defs/loop_input"}`)
	loopInput := defOf(out, "loop_input")
	if loopInput.IsZero() || !loopInput.HasProperties() {
		t.Fatal("loop input should have properties")
	}
	if loopInput.Properties()["task_index"].IsZero() {
		t.Error("task_index input field should be inferred")
	}
	if loopInput.Properties()["tasks"].IsZero() {
		t.Error("tasks input field should be inferred")
	}
}

func TestGenerate_SwitchStep_NextStepNotReachableViaFallthrough(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{
			id:     "decide",
			result: inferObjSchema(`"ok":{"type":"boolean"}`, "ok"),
			sw:     `[{"case":"self.result.ok","goto":"$work"},{"goto":"end"}]`,
			output: inferSelfResult,
		},
		inferTask{
			id:     "work",
			result: inferObjSchema(`"done":{"type":"boolean"}`),
			body:   `{"flag":"$: outputs.decide.ok"}`,
			output: inferSelfResult,
		},
	))
	assertJSON(t, out.Tasks["work"].Input, `{"$ref": "#/$defs/work_input"}`)
	workInput := defOf(out, "work_input")
	if workInput.IsZero() || !workInput.HasProperties() {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, workInput.Properties()["flag"], `{"type": "boolean"}`)
}

// --- templates and refs -----------------------------------------------------

func TestGenerate_MixedTemplate_NullableExpressionRejected(t *testing.T) {
	// error is nullable on finale (reachable via both normal and on_error paths).
	// Using it in a mixed template would silently produce "null_null" at runtime.
	err := runGenerateErr(t, inferDef("",
		inferTask{id: "start", fetch: true, onError: `[{"goto":"$finale"}]`, sw: inferNext},
		inferTask{id: "finale", body: `{"msg":"${ last_error.code }_${ last_error.message }"}`, sw: inferEnd},
	))
	if err == nil {
		t.Fatal("expected error for nullable expression in mixed template, got nil")
	}
	if !strings.Contains(err.Error(), "??") {
		t.Errorf("error should mention ?? operator, got: %v", err)
	}
}

func TestGenerate_MixedTemplate_NonNullableExpressionAccepted(t *testing.T) {
	// error is required (exclusive error path), so using it in a mixed template is fine.
	runGenerate(t, inferDef("",
		inferTask{id: "worker", fetch: true, sw: inferEnd, onError: `[{"goto":"$handler"}]`},
		inferTask{id: "handler", body: `{"msg":"${ last_error.code }_${ last_error.message }"}`, sw: inferEnd},
	))
}

func TestGenerate_InvalidRef(t *testing.T) {
	err := runGenerateErr(t, inferDef(`{"properties":{"x":{"$ref":"#/$defs/Missing"}}}`,
		inferTask{id: "s1", fetch: true},
	))
	if err == nil {
		t.Fatal("expected error for unresolved $ref, got nil")
	}
	if !strings.Contains(err.Error(), "Missing") {
		t.Errorf("error should mention the missing ref, got: %v", err)
	}
}

// --- self namespace scoping -------------------------------------------------

// self is the task's transient scope: self.result and (for a looping output task) self.previous
// are in the output map and the switch; self.output only in the switch, and only with an
// output. The next six tests walk those boundaries — crossing one is an error.

func TestGenerate_Self_OutputInSwitchWithoutProjectionRejected(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "s",
		result: inferObjSchema(`"x":{"type":"boolean"}`, "x"),
		sw:     `[{"case":"self.output.x","goto":"end"},{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err == nil {
		t.Errorf("self.output in a switch without a projection: expected error, got nil")
	}
}

func TestGenerate_Self_OutputInOutputMapRejected(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "s",
		result: inferObjSchema(`"x":{"type":"boolean"}`, "x"),
		output: `{"y":"$: self.output.x"}`,
		sw:     `[{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err == nil {
		t.Errorf("self.output in an output map: expected error, got nil")
	}
}

func TestGenerate_Self_ResultFieldWithoutResultSchemaRejected(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "s",
		fetch:  true,
		output: `{"id":"$: self.result.job_id"}`,
		sw:     `[{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err == nil {
		t.Errorf("self.result.field without result_schema: expected error, got nil")
	}
}

func TestGenerate_Self_PreviousInNonLoopingSwitchRejected(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "s",
		result: inferObjSchema(`"x":{"type":"boolean"}`, "x"),
		output: `{"y":"$: self.result.x"}`,
		sw:     `[{"case":"self.previous.y","goto":"end"},{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err == nil {
		t.Errorf("self.previous in a non-looping switch: expected error, got nil")
	}
}

func TestGenerate_Self_ResultInSwitchAccepted(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "s",
		result: inferObjSchema(`"x":{"type":"boolean"}`, "x"),
		sw:     `[{"case":"self.result.x","goto":"end"},{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err != nil {
		t.Errorf("self.result in a switch: expected no error, got %v", err)
	}
}

func TestGenerate_Self_PreviousInLoopingSwitchAccepted(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "loop",
		output: `{"n":"$: (self.previous.n ?? 0) + 1"}`,
		sw:     `[{"case":"(self.previous.n ?? 0) < 3","goto":"$loop"},{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err != nil {
		t.Errorf("self.previous in a looping output task's switch: expected no error, got %v", err)
	}
}

// --- output cycles ----------------------------------------------------------

// A task that references its own output but never loops back to itself has no
// prior iteration (it is not its own predecessor), so the self-reference must be
// rejected — through outputs.<id> here, and through self.previous below.
func TestGenerate_SelfReferenceViaOutputsRequiresLoop(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "loop",
		output: `{"num":"$: (outputs.loop.num ?? 0) + 1"}`,
		sw:     `[{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err == nil {
		t.Errorf("expected error for non-looping self-reference %q", "$: (outputs.loop.num ?? 0) + 1")
	}
}

func TestGenerate_SelfReferenceViaPreviousRequiresLoop(t *testing.T) {
	def := inferDef("", inferTask{
		id:     "loop",
		output: `{"num":"$: (self.previous.num ?? 0) + 1"}`,
		sw:     `[{"goto":"end"}]`,
	})
	if err := runGenerateErr(t, def); err == nil {
		t.Errorf("expected error for non-looping self-reference %q", "$: (self.previous.num ?? 0) + 1")
	}
}

func TestGenerate_ForwardCrossStepRefRequiresCycle(t *testing.T) {
	// a reads outputs.b, but b runs strictly after a and never loops back — b is not
	// a predecessor of a, so its output is unavailable. The cross-task analogue of
	// the self-reference-requires-loop rule.
	def := inferDef("",
		inferTask{id: "a", output: `{"n":"$: outputs.b.n"}`, sw: inferNext},
		inferTask{id: "b", output: `{"n":"$: 1"}`, sw: `[{"goto":"end"}]`},
	)
	if err := runGenerateErr(t, def); err == nil {
		t.Error("expected error: a cannot read outputs.b when b is not its predecessor")
	}
}

func TestGenerate_AcyclicOutputChain(t *testing.T) {
	// A linear chain of output-map tasks (first -> second -> third), each reading the
	// previous one's output. No cycle, so Tarjan emits singletons in dependency order
	// and each finalizes (non-null) before the next reads it.
	const intN = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`
	out := runGenerate(t, inferDef("",
		inferTask{id: "first", output: `{"n":"$: 1"}`, sw: inferNext},
		inferTask{id: "second", output: `{"n":"$: outputs.first.n + 1"}`, sw: inferNext},
		inferTask{id: "third", output: `{"n":"$: outputs.second.n + 1"}`, sw: `[{"goto":"end"}]`},
	))
	assertJSON(t, defOf(out, "first_output"), intN)
	assertJSON(t, defOf(out, "second_output"), intN)
	assertJSON(t, defOf(out, "third_output"), intN)
}

func TestGenerate_CrossStepMutualRecursion(t *testing.T) {
	// start and loop reference each other's output through a goto loop — a
	// cross-task (mutual) recursion. The joint SCC fixpoint resolves both: loop's
	// output is a plain integer; start mirrors loop and is nullable (null before
	// loop has run on the first pass).
	out := runGenerate(t, inferDefWithOutput(
		inferObjSchema(`"ttl":{"type":"integer"}`, "ttl"),
		`{"num":"$: outputs.start.num"}`,
		inferTask{id: "start", output: `{"num":"$: outputs.loop.num"}`, sw: inferNext},
		inferTask{id: "loop", output: `{"num":"$: (outputs.start.num ?? 0) + 1"}`,
			sw: `[{"case":"self.output.num < input.ttl","goto":"$start"},{"goto":"end"}]`},
	))
	assertJSON(t, defOf(out, "loop_output"), `{"type":"object","properties":{"num":{"type":"integer"}},"required":["num"]}`)
	assertJSON(t, defOf(out, "start_output"), `{"type":"object","properties":{"num":{"type":["integer","null"]}},"required":["num"]}`)
}

func TestGenerate_ThreeStepMutualRecursion(t *testing.T) {
	// A three-node output cycle (a reads c, b reads a, c reads b; closed by a goto
	// loop a->b->c->a) — exercises the joint SCC fixpoint beyond two members. c is
	// the base case (?? 0) so resolves to a plain integer; a and b mirror through
	// the cycle and are nullable (null before the cycle has produced a value).
	out := runGenerate(t, inferDef(inferObjSchema(`"ttl":{"type":"integer"}`, "ttl"),
		inferTask{id: "a", output: `{"n":"$: outputs.c.n"}`, sw: inferNext},
		inferTask{id: "b", output: `{"n":"$: outputs.a.n"}`, sw: inferNext},
		inferTask{id: "c", output: `{"n":"$: (outputs.b.n ?? 0) + 1"}`,
			sw: `[{"case":"self.output.n < input.ttl","goto":"$a"},{"goto":"end"}]`},
	))
	assertJSON(t, defOf(out, "c_output"), `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	assertJSON(t, defOf(out, "a_output"), `{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}`)
	assertJSON(t, defOf(out, "b_output"), `{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}`)
}

func TestGenerate_StructuralRecursionKeptAsRecursiveType(t *testing.T) {
	// `result: self.previous ?? input` nests one level deeper per iteration. It used to diverge (the
	// materialized estimate grew past the widening cap); with references honored it is a finite
	// recursive schema. The timeout guards against regressing to non-terminating inference.
	def := inferDefWithOutput(
		`{"type":"object",
		  "properties":{"ttl":{"type":"integer"},"rec":{"$ref":"#/$defs/recursive"}},
		  "required":["ttl"],
		  "$defs":{"recursive":{"type":"object","properties":{"num":{"type":"number"},"rec":{"$ref":"#/$defs/recursive"}}}}}`,
		`{"num":"$: outputs.loop"}`,
		inferTask{id: "start", sw: inferNext},
		inferTask{id: "loop", output: `{"result":"$: self.previous ?? input"}`,
			sw: `[{"case":"self.output.result != null","goto":"$start"},{"goto":"end"}]`},
	)

	type result struct {
		out validation.SchemaFile
		err error
	}
	done := make(chan result, 1)
	go func() {
		var parsed model.ProcessDefinition
		if err := json.Unmarshal([]byte(def), &parsed); err != nil {
			done <- result{err: err}
			return
		}
		out, err := validation.Generate(&parsed)
		done <- result{out: out, err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("structural recursion should infer as a recursive type, got error: %v", r.err)
		}
		loop := defOf(r.out, "loop_output").AsMap()
		if loop["type"] != "object" {
			t.Fatalf("loop_output is not an object: %s", mustMarshal(loop))
		}
		props, _ := loop["properties"].(map[string]any)
		res, _ := props["result"].(map[string]any)
		if res == nil {
			t.Fatalf("loop_output has no result property: %s", mustMarshal(loop))
		}
		variants, _ := res["oneOf"].([]any)
		if variants == nil {
			variants, _ = res["anyOf"].([]any)
		}
		selfRef := false
		for _, v := range variants {
			if vm, ok := v.(map[string]any); ok && vm["$ref"] == "#/$defs/loop_output" {
				selfRef = true
			}
		}
		if !selfRef {
			t.Errorf("loop_output.result does not keep the recursive $ref: %s", mustMarshal(res))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inference did not terminate: still running after 5s")
	}
}

// --- switch expressions -----------------------------------------------------

func TestGenerate_Switch_SelfExpressionTypeChecked(t *testing.T) {
	out := runGenerate(t, inferDef("",
		inferTask{
			id:     "charge",
			result: inferObjSchema(`"charged":{"type":"boolean"}`, "charged"),
			sw:     `[{"case":"self.result.charged == true","goto":"$ship"},{"case":"self.result.charged == false","goto":"$refund"}]`,
			output: inferSelfResult,
		},
		inferTask{id: "ship", fetch: true},
		inferTask{id: "refund", fetch: true},
	))
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
}

func TestGenerate_Switch_OutputsExpressionTypeChecked(t *testing.T) {
	// A later task's switch routes on a prior task's output (outputs.<priorTask>),
	// type-checked against that task's declared output schema.
	runGenerate(t, inferDef("",
		inferTask{
			id:     "charge",
			result: inferObjSchema(`"charged":{"type":"boolean"}`, "charged"),
			sw:     inferNext,
			output: inferSelfResult,
		},
		inferTask{id: "decide", sw: `[{"case":"outputs.charge.charged == true","goto":"$notify"},{"goto":"end"}]`},
		inferTask{id: "notify", fetch: true},
	))
}

func TestGenerate_Switch_OneOfAllBooleanAccepted(t *testing.T) {
	runGenerate(t, inferDef("",
		inferTask{
			id:     "check",
			result: inferObjSchema(`"ok":{"oneOf":[{"type":"boolean"},{"type":"boolean"}]}`, "ok"),
			sw:     `[{"case":"self.result.ok","goto":"$next"},{"goto":"end"}]`,
			output: inferSelfResult,
		},
		inferTask{id: "next", fetch: true},
	))
}

func TestGenerate_Switch_OneOfBooleanOptionalFieldRejected(t *testing.T) {
	err := runGenerateErr(t, inferDef(inferObjSchema(`"go_then":{"oneOf":[{"type":"boolean"}]}`),
		inferTask{id: "route", sw: `[{"case":"input.go_then","goto":"$next"},{"goto":"end"}]`},
		inferTask{id: "next", fetch: true},
	))
	if err == nil {
		t.Fatal("expected error for nullable oneOf boolean switch expression, got nil")
	}
}

func TestGenerate_Switch_NullableBooleanRejected(t *testing.T) {
	err := runGenerateErr(t, inferDef(inferObjSchema(`"go_then":{"type":"boolean"}`),
		inferTask{id: "route", sw: `[{"case":"input.go_then","goto":"$work"},{"goto":"end"}]`},
		inferTask{id: "work", fetch: true},
	))
	if err == nil {
		t.Fatal("expected error for nullable boolean switch expression, got nil")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention expected type, got: %v", err)
	}
}

func TestGenerate_Switch_StringExpressionRejectsNonBoolean(t *testing.T) {
	err := runGenerateErr(t, inferDef("",
		inferTask{
			id:     "check",
			result: inferObjSchema(`"label":{"type":"string"}`, "label"),
			sw:     `[{"case":"self.result.label","goto":"$next"},{"goto":"end"}]`,
			output: inferSelfResult,
		},
		inferTask{id: "next", fetch: true},
	))
	if err == nil {
		t.Fatal("expected error for non-boolean switch expression, got nil")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention expected type, got: %v", err)
	}
}
