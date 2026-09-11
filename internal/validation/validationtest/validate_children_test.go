package validationtest

import (
	"fmt"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

// inputShape wraps a flat expression map as a model.Shape (object form), the way
// a child Input is authored in these tests.
func inputShape(m map[string]string) *model.Shape {
	raw := make(map[string]any, len(m))
	for k, v := range m {
		raw[k] = v
	}
	return &model.Shape{Raw: raw}
}

type stubGetter map[string]*model.ProcessDefinition

func (s stubGetter) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	d, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("%s not found", name)
	}
	return d, nil
}
func (s stubGetter) LatestVersion(name string) (int, error) {
	if _, ok := s[name]; !ok {
		return 0, fmt.Errorf("%s not found", name)
	}
	return 1, nil
}

// normalizedSchema parses a JSON schema string and normalises it, as the DB
// would store it after a successful putDefinition call.
func normalizedSchema(t *testing.T, raw string) *schema.Schema {
	t.Helper()
	parsed, err := schema.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	out, err := parsed.Normalize()
	if err != nil {
		t.Fatalf("normalize schema: %v", err)
	}
	return &out
}

// childDef builds a minimal child ProcessDefinition whose InputSchema is the
// normalised form of rawSchema. Pass "" for no InputSchema.
func childDef(t *testing.T, name string, rawSchema string) *model.ProcessDefinition {
	t.Helper()
	def := &model.ProcessDefinition{
		Name: name,
		Tasks: []*model.Task{
			{ID: "noop", Switch: model.SwitchMap{{Goto: model.GotoEnd}}},
		},
	}
	if rawSchema != "" {
		def.InputSchema = normalizedSchema(t, rawSchema)
	}
	return def
}

// parentDef builds a ProcessDefinition with a child_map task, normalises
// it (mirroring what Generate does), and returns it ready for
// ValidateChildProcessRefs. Each entry gets a key "child0", "child1", etc.
func parentDef(t *testing.T, inputSchemaRaw string, entries []model.ChildEntry) *model.ProcessDefinition {
	t.Helper()
	children := make(map[string]model.ChildEntry, len(entries))
	for i, e := range entries {
		children[fmt.Sprintf("child%d", i)] = e
	}
	def := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "spawn",
				Action: &model.Action{
					Type:     model.ActionTypeChildMap,
					Children: children,
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if inputSchemaRaw != "" {
		def.InputSchema = normalizedSchema(t, inputSchemaRaw)
	}
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize parent def: %v", err)
	}
	return def
}

// parentInputSchema is reused by most tests: an object with integer "amount"
// and string "name", both required.
const parentInputSchema = `{
	"type": "object",
	"properties": {
		"amount": {"type": "integer"},
		"name":   {"type": "string"}
	},
	"required": ["amount", "name"]
}`

func assertValidateOK(t *testing.T, def *model.ProcessDefinition, getter validation.DefinitionGetter) {
	t.Helper()
	if err := validation.ValidateChildProcessRefs(def, 1, getter); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func assertValidateErr(t *testing.T, def *model.ProcessDefinition, getter validation.DefinitionGetter, wantSubstr string) {
	t.Helper()
	err := validation.ValidateChildProcessRefs(def, 1, getter)
	if err == nil {
		t.Errorf("expected error containing %q, got nil", wantSubstr)
		return
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error %q does not contain %q", err.Error(), wantSubstr)
	}
}

// --- tests ---

func TestValidateChildProcessRefs_noChildProcessSteps(t *testing.T) {
	def := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{ID: "fetch", Action: &model.Action{Type: model.ActionTypeFetch, Method: "post", URL: "http://example.com"}},
		},
	}
	assertValidateOK(t, def, stubGetter{})
}

func TestValidateChildProcessRefs_childExistsNoInputSchema(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", ""),
	}
	def := parentDef(t, "", []model.ChildEntry{
		{Name: "worker", Version: 1},
	})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_childNotFound(t *testing.T) {
	def := parentDef(t, "", []model.ChildEntry{
		{Name: "missing", Version: 1},
	})
	assertValidateErr(t, def, stubGetter{}, "not found")
}

func TestValidateChildProcessRefs_versionZeroResolvesToLatest(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", ""),
	}
	def := parentDef(t, "", []model.ChildEntry{
		{Name: "worker", Version: 0}, // 0 = latest
	})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_versionZeroChildNotFound(t *testing.T) {
	def := parentDef(t, "", []model.ChildEntry{
		{Name: "ghost", Version: 0},
	})
	assertValidateErr(t, def, stubGetter{}, "ghost")
}

func TestValidateChildProcessRefs_compatibleInput(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "integer"}},
			"required": ["amount"]
		}`),
	}
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "worker", Version: 1, Input: inputShape(map[string]string{"amount": "$: input.amount"})},
	})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_integerSubsetOfNumber(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "number"}},
			"required": ["amount"]
		}`),
	}
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "worker", Version: 1, Input: inputShape(map[string]string{"amount": "$: input.amount"})},
	})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_missingRequiredField(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {
				"amount": {"type": "integer"},
				"label":  {"type": "string"}
			},
			"required": ["amount", "label"]
		}`),
	}
	// parent only passes "amount", but child also requires "label"
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "worker", Version: 1, Input: inputShape(map[string]string{"amount": "$: input.amount"})},
	})
	assertValidateErr(t, def, getter, "not compatible")
}

func TestValidateChildProcessRefs_wrongFieldType(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "string"}},
			"required": ["amount"]
		}`),
	}
	// input.amount is integer, child expects string
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "worker", Version: 1, Input: inputShape(map[string]string{"amount": "$: input.amount"})},
	})
	assertValidateErr(t, def, getter, "not compatible")
}

func TestValidateChildProcessRefs_booleanAdditionalPropertiesRejectedAtParse(t *testing.T) {
	// Only the typed (schema-object) form of additionalProperties is supported; the
	// boolean form fails to parse.
	_, err := schema.Parse([]byte(`{
		"type": "object",
		"properties": {"amount": {"type": "integer"}},
		"additionalProperties": false
	}`))
	if err == nil {
		t.Fatal("expected parse error for boolean additionalProperties, got nil")
	}
	if err.Error() != `additionalProperties must be a schema object; the boolean form is not supported` {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateChildProcessRefs_badExpression(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"x": {"type": "integer"}},
			"required": ["x"]
		}`),
	}
	// parent has no InputSchema, so "$: input.amount" cannot be resolved
	def := parentDef(t, "", []model.ChildEntry{
		{Name: "worker", Version: 1, Input: inputShape(map[string]string{"x": "$: input.amount"})},
	})
	if err := validation.ValidateChildProcessRefs(def, 1, getter); err == nil {
		t.Error("expected error for unresolvable expression, got nil")
	}
}

func TestValidateChildProcessRefs_emptyInputIncompatibleWithRequired(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "integer"}},
			"required": ["amount"]
		}`),
	}
	// p.Input is nil — inferred schema is {type:object} with no required fields
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "worker", Version: 1},
	})
	assertValidateErr(t, def, getter, "not compatible")
}

func TestValidateChildProcessRefs_multipleProcessEntries(t *testing.T) {
	getter := stubGetter{
		"ok":  childDef(t, "ok", ""),
		"bad": childDef(t, "bad", `{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`),
	}
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "ok", Version: 1},
		{Name: "bad", Version: 1, Input: inputShape(map[string]string{"x": "$: input.name"})}, // string passed for integer
	})
	assertValidateErr(t, def, getter, "not compatible")
}

func TestValidateChildProcessRefs_selfReference(t *testing.T) {
	// A process that spawns itself (e.g. recursive tree traversal).
	// The process does not exist in the DB yet, so the getter must not be called.
	// Both required fields (amount + name) are forwarded so the input is compatible.
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "parent", Version: 0, Input: inputShape(map[string]string{
			"amount": "$: input.amount",
			"name":   "$: input.name",
		})},
	})
	// getter is empty — any DB call would return "not found"
	assertValidateOK(t, def, stubGetter{})
}

func TestValidateChildProcessRefs_selfReferenceIncompatibleInput(t *testing.T) {
	// Self-reference with an input that doesn't satisfy the process's own InputSchema.
	// Parent requires {amount: integer, name: string}; child entry only passes "amount"
	// as a string (via input.name), which is the wrong type.
	def := parentDef(t, parentInputSchema, []model.ChildEntry{
		{Name: "parent", Version: 0, Input: inputShape(map[string]string{"amount": "$: input.name"})},
	})
	assertValidateErr(t, def, stubGetter{}, "not compatible")
}

func TestValidateChildProcessRefs_inputWithNestedRef(t *testing.T) {
	// Parent InputSchema has a nested type that would produce $ref values in the
	// inferred schema. After normalization the refs must resolve correctly.
	parentSchema := `{
		"type": "object",
		"properties": {
			"order": {
				"type": "object",
				"properties": {
					"amount": {"type": "integer"},
					"currency": {"type": "string"}
				},
				"required": ["amount", "currency"]
			}
		},
		"required": ["order"]
	}`
	getter := stubGetter{
		"billing": childDef(t, "billing", `{
			"type": "object",
			"properties": {
				"amount":   {"type": "integer"},
				"currency": {"type": "string"}
			},
			"required": ["amount", "currency"]
		}`),
	}
	def := parentDef(t, parentSchema, []model.ChildEntry{
		{Name: "billing", Version: 1, Input: inputShape(map[string]string{
			"amount":   "$: input.order.amount",
			"currency": "$: input.order.currency",
		})},
	})
	assertValidateOK(t, def, getter)
}

// ── single-child via a one-entry child_map ────────────────────────────────────

// singleChildDef builds a ProcessDefinition that spawns one child via a one-entry
// child_map. This still exercises the per-entry subset path; the standalone `child`
// action type has its own tests below (childActionDef).
func singleChildDef(t *testing.T, inputSchemaRaw string, entry model.ChildEntry) *model.ProcessDefinition {
	t.Helper()
	def := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "spawn",
				Action: &model.Action{
					Type:     model.ActionTypeChildMap,
					Children: map[string]model.ChildEntry{"out": entry},
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if inputSchemaRaw != "" {
		def.InputSchema = normalizedSchema(t, inputSchemaRaw)
	}
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize parent def: %v", err)
	}
	return def
}

func TestValidateChildProcessRefs_Child_ChildExistsNoInputSchema(t *testing.T) {
	getter := stubGetter{"worker": childDef(t, "worker", "")}
	def := singleChildDef(t, "", model.ChildEntry{Name: "worker", Version: 1})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_Child_ChildNotFound(t *testing.T) {
	def := singleChildDef(t, "", model.ChildEntry{Name: "missing", Version: 1})
	assertValidateErr(t, def, stubGetter{}, "not found")
}

func TestValidateChildProcessRefs_Child_CompatibleInput(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "integer"}},
			"required": ["amount"]
		}`),
	}
	def := singleChildDef(t, parentInputSchema, model.ChildEntry{
		Name:    "worker",
		Version: 1,
		Input:   inputShape(map[string]string{"amount": "$: input.amount"}),
	})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_Child_IncompatibleInput(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "string"}},
			"required": ["amount"]
		}`),
	}
	// input.amount is integer; child expects string
	def := singleChildDef(t, parentInputSchema, model.ChildEntry{
		Name:    "worker",
		Version: 1,
		Input:   inputShape(map[string]string{"amount": "$: input.amount"}),
	})
	assertValidateErr(t, def, getter, "not compatible")
}

func TestValidateChildProcessRefs_Child_SelfReference(t *testing.T) {
	def := singleChildDef(t, parentInputSchema, model.ChildEntry{
		Name:  "parent",
		Input: inputShape(map[string]string{"amount": "$: input.amount", "name": "$: input.name"}),
	})
	assertValidateOK(t, def, stubGetter{})
}

func TestValidateChildProcessRefs_Child_VersionZeroResolvesToLatest(t *testing.T) {
	getter := stubGetter{"worker": childDef(t, "worker", "")}
	def := singleChildDef(t, "", model.ChildEntry{Name: "worker", Version: 0})
	assertValidateOK(t, def, getter)
}

// ── standalone `child` action type ────────────────────────────────────────────

// childActionDef builds a ProcessDefinition that spawns one child via the standalone
// `child` action (Name/Version/Input/ResultSchema on the Action itself, no Children map).
// The subset checks are the same as a one-entry child_map, so it reuses model.ChildEntry
// only as a convenient carrier for the fields.
func childActionDef(t *testing.T, inputSchemaRaw string, entry model.ChildEntry) *model.ProcessDefinition {
	t.Helper()
	def := &model.ProcessDefinition{
		Name: "parent",
		Tasks: []*model.Task{
			{
				ID: "spawn",
				Action: &model.Action{
					Type:         model.ActionTypeChild,
					Name:         entry.Name,
					Version:      entry.Version,
					Input:        entry.Input,
					ResultSchema: entry.ResultSchema,
				},
				Switch: model.SwitchMap{{Goto: model.GotoEnd}},
			},
		},
	}
	if inputSchemaRaw != "" {
		def.InputSchema = normalizedSchema(t, inputSchemaRaw)
	}
	if err := def.Normalize(); err != nil {
		t.Fatalf("normalize parent def: %v", err)
	}
	return def
}

func TestValidateChildProcessRefs_ChildAction_ChildExistsNoInputSchema(t *testing.T) {
	getter := stubGetter{"worker": childDef(t, "worker", "")}
	def := childActionDef(t, "", model.ChildEntry{Name: "worker", Version: 1})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_ChildAction_ChildNotFound(t *testing.T) {
	def := childActionDef(t, "", model.ChildEntry{Name: "missing", Version: 1})
	assertValidateErr(t, def, stubGetter{}, "not found")
}

func TestValidateChildProcessRefs_ChildAction_CompatibleInput(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "integer"}},
			"required": ["amount"]
		}`),
	}
	def := childActionDef(t, parentInputSchema, model.ChildEntry{
		Name:    "worker",
		Version: 1,
		Input:   inputShape(map[string]string{"amount": "$: input.amount"}),
	})
	assertValidateOK(t, def, getter)
}

func TestValidateChildProcessRefs_ChildAction_IncompatibleInput(t *testing.T) {
	getter := stubGetter{
		"worker": childDef(t, "worker", `{
			"type": "object",
			"properties": {"amount": {"type": "string"}},
			"required": ["amount"]
		}`),
	}
	// input.amount is integer; child expects string.
	def := childActionDef(t, parentInputSchema, model.ChildEntry{
		Name:    "worker",
		Version: 1,
		Input:   inputShape(map[string]string{"amount": "$: input.amount"}),
	})
	assertValidateErr(t, def, getter, "not compatible")
}

func TestValidateChildProcessRefs_ChildAction_SelfReference(t *testing.T) {
	def := childActionDef(t, parentInputSchema, model.ChildEntry{
		Name:  "parent",
		Input: inputShape(map[string]string{"amount": "$: input.amount", "name": "$: input.name"}),
	})
	assertValidateOK(t, def, stubGetter{})
}

func TestValidateChildProcessRefs_ChildAction_VersionZeroResolvesToLatest(t *testing.T) {
	getter := stubGetter{"worker": childDef(t, "worker", "")}
	def := childActionDef(t, "", model.ChildEntry{Name: "worker", Version: 0})
	assertValidateOK(t, def, getter)
}
