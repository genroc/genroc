package model

import (
	"testing"

	"genroc/internal/schema"
)

// mustSchemaPtr parses a JSON schema fixture as-is (unnormalized — Normalize
// tests rely on nested $defs surviving the parse), panicking on error.
func mustSchemaPtr(src string) *schema.Schema {
	raw, err := schema.Parse([]byte(src))
	if err != nil {
		panic(err)
	}
	s := raw.AssumeNormalized()
	return &s
}

func TestProcessDefinition_Normalize(t *testing.T) {
	validTask := func(id string) *Task {
		return &Task{ID: id, Action: &Action{Type: ActionTypeFetch, URL: "http://localhost/x"}}
	}

	t.Run("no schemas is a no-op", func(t *testing.T) {
		d := ProcessDefinition{Name: "p", Tasks: []*Task{validTask("s1")}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("simple InputSchema without refs is unchanged", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Tasks: []*Task{validTask("s1")},
			InputSchema: mustSchemaPtr(`{"type":"object","properties":{"id":{"type":"integer"}}}`),
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !d.InputSchema.HasProperties() {
			t.Fatal("properties missing after normalize")
		}
	})

	t.Run("InputSchema $defs are flattened to root", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Tasks: []*Task{validTask("s1")},
			InputSchema: mustSchemaPtr(`{
				"type": "object",
				"properties": {"addr": {"$ref": "#/$defs/Address"}},
				"$defs": {"Address": {"type": "object", "properties": {"street": {"type": "string"}}}}
			}`),
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !d.InputSchema.DefsHandle().Has("Address") {
			t.Fatal("$defs/Address missing after normalize")
		}
		if !d.InputSchema.HasProperties() {
			t.Fatal("properties missing after normalize")
		}
	})

	t.Run("task action.result_schema $defs are flattened to root", func(t *testing.T) {
		task := validTask("charge")
		task.Action.ResultSchema = mustSchemaPtr(`{
			"type": "object",
			"$defs": {"Result": {"type": "object", "properties": {"ok": {"type": "boolean"}}}},
			"properties": {"result": {"$ref": "#/$defs/Result"}}
		}`)
		d := ProcessDefinition{Name: "p", Tasks: []*Task{task}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !task.Action.ResultSchema.DefsHandle().Has("Result") {
			t.Fatal("$defs/Result missing in action.result_schema after normalize")
		}
	})

	t.Run("all task action.result_schemas are normalized", func(t *testing.T) {
		step1 := validTask("charge")
		step1.Action.ResultSchema = mustSchemaPtr(`{
			"type": "object",
			"$defs": {"Tracking": {"type": "object"}},
			"properties": {"tracking": {"$ref": "#/$defs/Tracking"}}
		}`)
		step2 := validTask("notify")
		step2.Action.ResultSchema = mustSchemaPtr(`{
			"type": "object",
			"$defs": {"Result": {"type": "object"}},
			"properties": {"result": {"$ref": "#/$defs/Result"}}
		}`)
		d := ProcessDefinition{Name: "p", Tasks: []*Task{step1, step2}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !step1.Action.ResultSchema.DefsHandle().Has("Tracking") {
			t.Fatal("step1 $defs/Tracking missing after normalize")
		}
		if !step2.Action.ResultSchema.DefsHandle().Has("Result") {
			t.Fatal("step2 $defs/Result missing after normalize")
		}
	})

	t.Run("invalid $ref in InputSchema returns error", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Tasks: []*Task{validTask("s1")},
			InputSchema: mustSchemaPtr(`{"type":"object","properties":{"x":{"$ref":"#/$defs/Missing"}}}`),
		}
		if err := d.Normalize(); err == nil {
			t.Fatal("expected error for unresolved $ref, got nil")
		}
	})

	t.Run("invalid $ref in task action.result_schema returns error with task ID", func(t *testing.T) {
		task := validTask("charge")
		task.Action.ResultSchema = mustSchemaPtr(`{"type":"object","properties":{"x":{"$ref":"#/$defs/Missing"}}}`)
		d := ProcessDefinition{Name: "p", Tasks: []*Task{task}}
		err := d.Normalize()
		if err == nil {
			t.Fatal("expected error for unresolved $ref, got nil")
		}
		if !containsStr(err.Error(), "charge") {
			t.Errorf("error %q should mention task ID %q", err.Error(), "charge")
		}
	})

	t.Run("child_map children result_schemas are normalized", func(t *testing.T) {
		task := &Task{
			ID: "spawn",
			Action: &Action{
				Type: ActionTypeChildMap,
				Children: map[string]ChildEntry{
					"a": {Name: "worker", ResultSchema: mustSchemaPtr(`{
						"type": "object",
						"$defs": {"Result": {"type": "object"}},
						"properties": {"r": {"$ref": "#/$defs/Result"}}
					}`)},
				},
			},
			Switch: SwitchMap{{Goto: GotoEnd}},
		}
		d := ProcessDefinition{Name: "p", Tasks: []*Task{task}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		entry := task.Action.Children["a"]
		if entry.ResultSchema == nil || !entry.ResultSchema.DefsHandle().Has("Result") {
			t.Fatal("$defs/Result missing in children[a].result_schema after normalize")
		}
	})

	t.Run("unused $defs are removed from InputSchema", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Tasks: []*Task{validTask("s1")},
			InputSchema: mustSchemaPtr(`{
				"type": "object",
				"$defs": {"Used": {"type": "string"}, "Unused": {"type": "integer"}},
				"properties": {"name": {"$ref": "#/$defs/Used"}}
			}`),
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !d.InputSchema.DefsHandle().Has("Used") {
			t.Fatal("$defs/Used should be present")
		}
		if d.InputSchema.DefsHandle().Has("Unused") {
			t.Fatal("$defs/Unused should have been removed as unused")
		}
	})
}

func TestProcessDefinition_Validate(t *testing.T) {
	restTaskEnd := func(id, endpoint string) *Task {
		return &Task{ID: id, Action: &Action{Type: ActionTypeFetch, URL: endpoint}, Switch: SwitchMap{{Goto: GotoEnd}}}
	}

	tests := []struct {
		name    string
		def     ProcessDefinition
		wantErr string
	}{
		{
			name:    "valid rest call task",
			def:     ProcessDefinition{Name: "p", Tasks: []*Task{restTaskEnd("step1", "http://localhost/action")}},
			wantErr: "",
		},
		{
			// accepted_status is a shape; model.Validate accepts it structurally. Its
			// array<string> conformance and per-literal format are enforced at registration
			// (validation package) — see validationtest/accepted_status_test.go.
			name: "accepted_status shape is accepted by model validation",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{{
				ID:     "call",
				Action: &Action{Type: ActionTypeFetch, URL: "http://x", AcceptedStatus: &Shape{Raw: []any{"2xx", "404"}}},
				Switch: SwitchMap{{Goto: GotoEnd}},
			}}},
			wantErr: "",
		},
		{
			name: "valid switch-only task",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "router", Switch: SwitchMap{
					{Case: "input.ok == true", Goto: "$act"},
					{Goto: GotoEnd},
				}},
				restTaskEnd("act", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "valid task with both call and switch",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch: SwitchMap{
						{Case: "self.ok == true", Goto: "$ship"},
						{Goto: GotoEnd},
					},
				},
				restTaskEnd("ship", "http://x"),
			}},
			wantErr: "",
		},
		{
			name:    "missing name",
			def:     ProcessDefinition{Tasks: []*Task{restTaskEnd("step1", "http://x")}},
			wantErr: "name is required",
		},
		{
			name:    "no tasks",
			def:     ProcessDefinition{Name: "p"},
			wantErr: "tasks",
		},
		{
			name: "task without switch is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "s", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}},
			}},
			wantErr: "switch is required",
		},
		{
			name: "task with no call and no switch is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "empty"},
			}},
			wantErr: "switch is required",
		},
		{
			name: "rest call missing endpoint",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "s1", Action: &Action{Type: ActionTypeFetch}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "action.url is required",
		},
		{
			name: "valid child call",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{Type: ActionTypeChild, Name: "worker"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "",
		},
		{
			name: "child call missing name",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{Type: ActionTypeChild}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "action.name is required",
		},
		{
			name: "valid child_list call",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{Type: ActionTypeChildList, Name: "worker", Over: "$: input.items"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "",
		},
		{
			name: "child_list call missing name",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{Type: ActionTypeChildList, Over: "$: input.items"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "action.name is required",
		},
		{
			name: "child_list call missing over",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{Type: ActionTypeChildList, Name: "worker"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "action.over is required",
		},
		{
			name: "valid child_map call",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{
					Type: ActionTypeChildMap,
					Children: map[string]ChildEntry{
						"left":  {Name: "worker"},
						"right": {Name: "worker"},
					},
				}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "",
		},
		{
			name: "child_map call missing children",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{Type: ActionTypeChildMap}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "action.children is required",
		},
		{
			name: "child_map entry missing name",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "spawn", Action: &Action{
					Type:     ActionTypeChildMap,
					Children: map[string]ChildEntry{"left": {Name: ""}},
				}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: `action.children["left"].name is required`,
		},
		{
			name: "unknown call type",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "s1", Action: &Action{Type: "ftp", URL: "ftp://x"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: "action.type must be one of: fetch, child, child_map, child_list",
		},
		{
			name: "switch missing catch-all is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch: SwitchMap{
						{Case: "self.ok == true", Goto: "$ship"},
					},
				},
				restTaskEnd("ship", "http://x"),
			}},
			wantErr: `last case must be a catch-all`,
		},
		{
			name: "switch catch-all not last is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch: SwitchMap{
						{Goto: GotoEnd},
						{Case: "self.ok == true", Goto: "$ship"},
					},
				},
				restTaskEnd("ship", "http://x"),
			}},
			wantErr: `catch-all at index 0 must be the last case`,
		},
		{
			name: "switch end in non-catch-all case is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch: SwitchMap{
						{Case: "self.error == true", Goto: GotoEnd},
						{Goto: "$ship"},
					},
				},
				restTaskEnd("ship", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "switch goto references unknown task",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch: SwitchMap{
						{Case: "self.ok == true", Goto: "$nonexistent"},
						{Goto: GotoEnd},
					},
				},
			}},
			wantErr: `goto "$nonexistent" is not a known task`,
		},
		{
			name: "switch: next on last task is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "only", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}, Switch: SwitchMap{{Goto: GotoNext}}},
			}},
			wantErr: "'next' is not allowed on the last task",
		},
		{
			name: "switch: next on non-last task is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "first", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}, Switch: SwitchMap{{Goto: GotoNext}}},
				restTaskEnd("second", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "switch: scalar next on non-last task is valid",
			def: func() ProcessDefinition {
				s := &Task{ID: "first", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}}
				if err := s.Switch.UnmarshalJSON([]byte(`"next"`)); err != nil {
					panic(err)
				}
				return ProcessDefinition{Name: "p", Tasks: []*Task{s, restTaskEnd("second", "http://x")}}
			}(),
			wantErr: "",
		},
		{
			name: "task ID 'end' is reserved",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "end", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: `task ID "end" is reserved`,
		},
		{
			name: "task ID 'next' is reserved",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{ID: "next", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}, Switch: SwitchMap{{Goto: GotoEnd}}},
			}},
			wantErr: `task ID "next" is reserved`,
		},

		// ── only_once: true static validation ───────────────────────────────

		{
			name: "only_once:true — retries on pre.% is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"pre.%"}, Retries: 3}},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — retries on exact pre.* codes is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"pre.error", "pre.timeout"}, Retries: 3}},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — retries:0 with http.% next is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"http.%"}, Goto: "handler"}},
				},
				restTaskEnd("handler", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — not_reached:true overrides http.422 retry",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"http.422"}, NotReached: boolPtr(true), Retries: 2}},
				},
			}},
			wantErr: "",
		},
		{
			// A catch-all matches the unknowable codes too, and not_reached cannot
			// assert anything about an error that never came back — so unlike the
			// http.422 case above, this one is refused however it is annotated.
			name: "only_once:true — not_reached:true does not rescue a catch-all retry",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{NotReached: boolPtr(true), Retries: 2}},
				},
			}},
			wantErr: "a catch-all rule cannot have retries on an only_once task",
		},
		{
			name: "only_once:true — retries on http.% is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"http.%"}, Retries: 3}},
				},
			}},
			wantErr: `pattern "http.%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — retries on exact http.500 is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"http.500"}, Retries: 1}},
				},
			}},
			wantErr: `pattern "http.500" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — retries on http.% is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"http.%"}, Retries: 1}},
				},
			}},
			wantErr: `pattern "http.%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — catch-all with retries is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Retries: 2}},
				},
			}},
			wantErr: "a catch-all rule cannot have retries on an only_once task",
		},
		{
			name: "only_once:true — wildcard crossing namespaces is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"s%"}, Retries: 3}},
				},
			}},
			wantErr: `pattern "s%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — mixed pre and non-pre patterns in one rule is rejected",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(true),
					OnError:  []ErrorCase{{Code: []string{"pre.%", "http.%"}, Retries: 1}},
				},
			}},
			wantErr: `pattern "http.%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:false (explicit) — retries on http.% is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID: "charge", Action: &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:   SwitchMap{{Goto: GotoEnd}},
					OnlyOnce: boolPtr(false),
					OnError:  []ErrorCase{{Code: []string{"http.%"}, Retries: 3}},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once nil (default) — retries on http.% is valid",
			def: ProcessDefinition{Name: "p", Tasks: []*Task{
				{
					ID:      "charge",
					Action:  &Action{Type: ActionTypeFetch, URL: "http://x"},
					Switch:  SwitchMap{{Goto: GotoEnd}},
					OnError: []ErrorCase{{Code: []string{"http.%"}, Retries: 3}},
				},
			}},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.def.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// A delay carries exactly one of for / until, and a tz that resolves. The slot contents
// themselves are checked at registration (validationtest/delay_test.go) — this is only the
// structural arity, which is what stops "no slot set" from ever reaching the engine.
func TestProcessDefinition_Validate_DelaySlots(t *testing.T) {
	delay := func(a *Action) ProcessDefinition {
		return ProcessDefinition{Name: "p", Tasks: []*Task{{ID: "wait", Action: a, Switch: SwitchMap{{Goto: GotoEnd}}}}}
	}
	tests := []struct {
		name    string
		action  *Action
		wantErr string
	}{
		{"for alone", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "2h30m"}}, ""},
		{"until alone", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{Until: "+2d 08:00"}}, ""},
		{"bare number for", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: float64(5000)}}, ""},
		{"for with tz", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1d", TZ: "Europe/Prague"}}, ""},
		{"for with fixed-offset tz", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1d", TZ: "+02:00"}}, ""},

		{"neither slot", &Action{Type: ActionTypeDelay}, "for or action.until is required"},
		{"both slots", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1h", Until: "+1d 08:00"}}, "mutually exclusive"},
		// An abbreviation means the wrong thing for half the year and resolves per host.
		{"abbreviated tz", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1d", TZ: "CET"}}, "abbreviations"},
		{"unknown tz", &Action{Type: ActionTypeDelay, DelaySpec: DelaySpec{For: "1d", TZ: "Europe/Nowhere"}}, "unknown IANA location"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := delay(tt.action)
			err := def.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestProcessDefinition_ValidateInput_Nullable(t *testing.T) {
	def := ProcessDefinition{
		Name: "p",
		InputSchema: mustSchemaPtr(`{
			"type": "object",
			"required": ["id"],
			"properties": {
				"id":      {"type": "integer"},
				"comment": {"type": ["string", "null"]}
			}
		}`),
		Tasks: []*Task{{ID: "s", Action: &Action{Type: ActionTypeFetch, URL: "http://x"}}},
	}

	tests := []struct {
		name    string
		input   any
		wantErr bool
	}{
		{
			name:  "required field present, nullable field absent",
			input: map[string]any{"id": 1},
		},
		{
			name:  "required field present, nullable field is null",
			input: map[string]any{"id": 1, "comment": nil},
		},
		{
			name:  "required field present, nullable field has value",
			input: map[string]any{"id": 1, "comment": "hello"},
		},
		{
			name:    "required field missing",
			input:   map[string]any{"comment": "hello"},
			wantErr: true,
		},
		{
			name:    "required field is null (non-nullable)",
			input:   map[string]any{"id": nil},
			wantErr: true,
		},
		{
			name:    "nullable field has wrong type",
			input:   map[string]any{"id": 1, "comment": 42},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := def.ValidateInput(tt.input)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestStep_ValidateOutput_Nullable(t *testing.T) {
	task := &Task{
		ID: "charge",
		Action: &Action{
			Type: ActionTypeFetch,
			URL:  "http://x",
			ResultSchema: mustSchemaPtr(`{
				"type": "object",
				"required": ["charged"],
				"properties": {
					"charged": {"type": "boolean"},
					"receipt": {"type": ["string", "null"]}
				}
			}`),
		},
	}

	tests := []struct {
		name    string
		output  map[string]any
		wantErr bool
	}{
		{
			name:   "required field present, nullable field absent",
			output: map[string]any{"charged": true},
		},
		{
			name:   "required field present, nullable field is null",
			output: map[string]any{"charged": true, "receipt": nil},
		},
		{
			name:   "required field present, nullable field has value",
			output: map[string]any{"charged": true, "receipt": "REC-001"},
		},
		{
			name:    "required field missing",
			output:  map[string]any{"receipt": "REC-001"},
			wantErr: true,
		},
		{
			name:    "required field is null (non-nullable)",
			output:  map[string]any{"charged": nil},
			wantErr: true,
		},
		{
			name:    "nullable field has wrong type",
			output:  map[string]any{"charged": true, "receipt": 123},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := task.Action.ValidateOutput(tt.output)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestSwitchMap_MarshalUnmarshal(t *testing.T) {
	t.Run("array form round-trips", func(t *testing.T) {
		original := SwitchMap{
			{Case: "self.paid == true", Goto: "$ship"},
			{Goto: GotoEnd},
		}
		data, err := original.MarshalJSON()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `[{"case":"self.paid == true","goto":"$ship"},{"goto":"end"}]`
		if string(data) != want {
			t.Errorf("marshal: got %s, want %s", data, want)
		}
		var decoded SwitchMap
		if err := decoded.UnmarshalJSON(data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(decoded) != len(original) {
			t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(original))
		}
		for i := range original {
			if decoded[i] != original[i] {
				t.Errorf("case %d: got %+v, want %+v", i, decoded[i], original[i])
			}
		}
	})

	t.Run("scalar 'end' desugars to catch-all with GotoEnd", func(t *testing.T) {
		var s SwitchMap
		if err := s.UnmarshalJSON([]byte(`"end"`)); err != nil {
			t.Fatalf("unmarshal scalar: %v", err)
		}
		if len(s) != 1 || s[0].Case != "" || s[0].Goto != GotoEnd {
			t.Errorf("expected [{Case:\"\", Goto:\"end\"}], got %+v", s)
		}
	})

	t.Run("scalar 'next' desugars to catch-all with GotoNext", func(t *testing.T) {
		var s SwitchMap
		if err := s.UnmarshalJSON([]byte(`"next"`)); err != nil {
			t.Fatalf("unmarshal scalar: %v", err)
		}
		if len(s) != 1 || s[0].Case != "" || s[0].Goto != GotoNext {
			t.Errorf("expected [{Case:\"\", Goto:\"next\"}], got %+v", s)
		}
	})

	t.Run("scalar '$task-id' desugars to task reference", func(t *testing.T) {
		var s SwitchMap
		if err := s.UnmarshalJSON([]byte(`"$my-task"`)); err != nil {
			t.Fatalf("unmarshal scalar: %v", err)
		}
		if len(s) != 1 || s[0].Case != "" || s[0].Goto != "$my-task" {
			t.Errorf("expected [{Case:\"\", Goto:\"$my-task\"}], got %+v", s)
		}
	})

	t.Run("scalar without sigil is rejected", func(t *testing.T) {
		var s SwitchMap
		if err := s.UnmarshalJSON([]byte(`"unknown"`)); err == nil {
			t.Fatal("expected error for unknown scalar, got nil")
		}
	})
}

func TestPatternOnlyMatchesPre(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		// exact pre.* codes
		{"pre.error", true},
		{"pre.timeout", true},
		{"pre.exec", true},
		{"pre.", true},
		// wildcards rooted at pre.
		{"pre.%", true},
		{"pre._%", true},
		{"pre._rror", true},
		// does not start with "pre." — no wildcard
		{"http.500", false},
		{"output.parse", false},
		{"child.failed", false},
		// wildcards not rooted at pre.
		{"%", false},
		{"http.%", false},
		// "pre" without dot: prefix is "pre", not "pre."
		{"pre%", false},
		// "p%" could match pre.* but also other codes
		{"p%", false},
		// empty string
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got := patternOnlyMatchesPre(tt.pattern)
			if got != tt.want {
				t.Errorf("patternOnlyMatchesPre(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstr(s, sub))
}

func containsSubstr(s, sub string) bool {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
