package validationtest

import (
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// The output analogue of the input subset check: a child's declared process output type
// must be a subset of the result_schema the parent declares for it. This catches a child
// whose output shape cannot satisfy the parent's assertion at registration, instead of
// only at runtime on collect — or never, if the child raises before producing output.

// outputtingChild builds a child whose process output is `outputRaw`, normalised as the
// stored definition would be.
func outputtingChild(t *testing.T, name string, taskOutput map[string]any, outputRaw any) *model.ProcessDefinition {
	t.Helper()
	task := &model.Task{ID: "compute", Switch: model.SwitchMap{{Goto: model.GotoEnd}}}
	if taskOutput != nil {
		task.Output = &model.Shape{Raw: taskOutput}
	}
	d := &model.ProcessDefinition{
		Name:   name,
		Tasks:  []*model.Task{task},
		Output: &model.Shape{Raw: outputRaw},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize child %q: %v", name, err)
	}
	return d
}

// childMapParentRS builds a parent whose single child_map entry declares `rs` as the
// child's result_schema.
func childMapParentRS(t *testing.T, childName string, rs *schema.Schema) *model.ProcessDefinition {
	t.Helper()
	d := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "spawn",
				Action: &model.Action{
					Type:     model.ActionTypeChildMap,
					Children: map[string]model.ChildEntry{"a": {Name: childName, ResultSchema: rs}},
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize parent: %v", err)
	}
	return d
}

// childActionParentRS builds a parent whose standalone `child` action declares `rs` as
// the child's result_schema — the unwrapped analogue of childMapParentRS.
func childActionParentRS(t *testing.T, childName string, rs *schema.Schema) *model.ProcessDefinition {
	t.Helper()
	d := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "spawn",
				Action: &model.Action{
					Type:         model.ActionTypeChild,
					Name:         childName,
					ResultSchema: rs,
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if err := d.Normalize(); err != nil {
		t.Fatalf("normalize parent: %v", err)
	}
	return d
}

func TestChildOutputType_ChildAction_MatchingObjectAccepted(t *testing.T) {
	child := outputtingChild(t, "obj-child2",
		map[string]any{"ok": "$: true"}, "$: outputs.compute")
	rs := normalizedSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	assertValidateOK(t, childActionParentRS(t, "obj-child2", rs), stubGetter{"obj-child2": child})
}

func TestChildOutputType_ChildAction_StringVsObjectRejected(t *testing.T) {
	// The child's process output is a plain string against an object result_schema — the
	// same static mismatch child_map rejects, checked here for the standalone `child`.
	child := outputtingChild(t, "str-child2", nil, `$: "hello"`)
	rs := normalizedSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	assertValidateErr(t, childActionParentRS(t, "str-child2", rs),
		stubGetter{"str-child2": child}, "result_schema")
}

func TestChildOutputType_MatchingObjectAccepted(t *testing.T) {
	// Child outputs { ok: boolean }; result_schema expects the same.
	child := outputtingChild(t, "obj-child",
		map[string]any{"ok": "$: true"}, "$: outputs.compute")
	rs := normalizedSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	assertValidateOK(t, childMapParentRS(t, "obj-child", rs), stubGetter{"obj-child": child})
}

func TestChildOutputType_StringVsObjectRejected(t *testing.T) {
	// The playground bug: child's process output is a plain string, result_schema is an
	// object — a static mismatch the check must reject.
	child := outputtingChild(t, "str-child", nil, `$: "hello"`)
	rs := normalizedSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	assertValidateErr(t, childMapParentRS(t, "str-child", rs),
		stubGetter{"str-child": child}, "result_schema")
}

func TestChildOutputType_MissingRequiredFieldRejected(t *testing.T) {
	// Child outputs { ok: boolean } but result_schema requires a field it never produces.
	child := outputtingChild(t, "partial-child",
		map[string]any{"ok": "$: true"}, "$: outputs.compute")
	rs := normalizedSchema(t, `{"type":"object","properties":{"missing":{"type":"string"}},"required":["missing"]}`)
	assertValidateErr(t, childMapParentRS(t, "partial-child", rs),
		stubGetter{"partial-child": child}, "result_schema")
}

// Subset, not exact match (1): a child that returns MORE than the parent declares is
// fine — the extra field is accepted (objects are open) and stripped at collect. The
// parent simply reads the subset it declared.
func TestChildOutputType_ChildReturnsMoreAccepted(t *testing.T) {
	child := outputtingChild(t, "rich-child",
		map[string]any{"a": `$: "x"`, "b": `$: "y"`}, "$: outputs.compute")
	rs := normalizedSchema(t, `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)
	assertValidateOK(t, childMapParentRS(t, "rich-child", rs), stubGetter{"rich-child": child})
}

// Subset, not exact match (2): a parent may declare an OPTIONAL field the child does not
// produce yet — the way to prepare a parent for a child that will add it. Only a missing
// *required* field is a real incompatibility.
func TestChildOutputType_OptionalNotYetProducedAccepted(t *testing.T) {
	child := outputtingChild(t, "lean-child",
		map[string]any{"a": `$: "x"`}, "$: outputs.compute")
	rs := normalizedSchema(t, `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`)
	assertValidateOK(t, childMapParentRS(t, "lean-child", rs), stubGetter{"lean-child": child})
}

func TestChildOutputType_NoResultSchemaSkipped(t *testing.T) {
	// No result_schema declared: nothing to check against, any output is fine.
	child := outputtingChild(t, "any-out", nil, `$: "whatever"`)
	parent := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "spawn",
				Action: &model.Action{
					Type:     model.ActionTypeChildMap,
					Children: map[string]model.ChildEntry{"a": {Name: "any-out"}},
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if err := parent.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	assertValidateOK(t, parent, stubGetter{"any-out": child})
}

func TestChildOutputType_ChildWithoutOutputSkipped(t *testing.T) {
	// The child declares no process output, so its output type is open — the check is
	// skipped and runtime validation (result.invalid) remains the backstop.
	child := &model.ProcessDefinition{
		Name:  "no-out",
		Tasks: []*model.Task{{ID: "noop", Switch: model.SwitchMap{{Goto: model.GotoEnd}}}},
	}
	if err := child.Normalize(); err != nil {
		t.Fatalf("normalize child: %v", err)
	}
	rs := normalizedSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	if err := validation.ValidateChildProcessRefs(childMapParentRS(t, "no-out", rs), 1, stubGetter{"no-out": child}); err != nil {
		t.Fatalf("a child with no output should be skipped, got: %v", err)
	}
}
