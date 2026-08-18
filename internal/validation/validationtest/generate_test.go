package validationtest

import (
	"strings"
	"testing"
)

func TestGenerate_NoSchemas(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [{"id":"s1","action":{"type":"fetch","url":"http://x"}}]
	}`)
	if out.Process != "p" {
		t.Errorf("metadata: got process=%q", out.Process)
	}
	if !out.ProcessInput.IsZero() {
		t.Error("process_input should be absent")
	}
	if len(out.Tasks) != 0 {
		t.Errorf("tasks should be empty, got %v", out.Tasks)
	}
	if out.Defs.Len() != 0 {
		t.Errorf("$defs should be empty, got %v", out.Defs)
	}
}

func TestGenerate_ProcessInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "order",
		"tasks": [{"id":"s1","action":{"type":"fetch","url":"http://x"}}],
		"input_schema": {
			"type": "object",
			"properties": { "order_id": { "type": "integer" } },
			"required": ["order_id"]
		}
	}`)
	assertJSON(t, out.ProcessInput, `{"$ref": "#/$defs/input"}`)
	assertJSON(t, defOf(out, "input"), `{
		"type": "object",
		"properties": { "order_id": { "type": "integer" } },
		"required": ["order_id"]
	}`)
}

func TestGenerate_TaskOutput(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "responses": { "200": {
          "type": "object",
          "properties": {
            "charged": {
              "type": "boolean"
            }
          }
        } }
      },
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "notify",
      "action": {
        "type": "fetch",
        "url": "http://x"
      },
      "switch": "end"
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, defOf(out, "charge_output"), `{
		"type": "object",
		"properties": { "charged": { "type": "boolean" } }
	}`)
	if _, ok := out.Tasks["notify"]; ok {
		t.Error("notify has no schemas and should not appear in tasks")
	}
}

func TestGenerate_FlatStepsWithOutputs(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "responses": { "200": {
          "type": "object",
          "properties": {
            "charged": {
              "type": "boolean"
            }
          }
        } }
      },
      "switch": [
        {
          "case": "self.output.charged == true",
          "goto": "$ship"
        },
        {
          "goto": "$refund"
        }
      ],
      "output": "$: self.result"
    },
    {
      "id": "ship",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "responses": { "200": {
          "type": "object",
          "properties": {
            "tracking": {
              "type": "string"
            }
          }
        } }
      },
      "switch": "end",
      "output": "$: self.result"
    },
    {
      "id": "refund",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "responses": { "200": {
          "type": "object",
          "properties": {
            "refunded": {
              "type": "boolean"
            }
          }
        } }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, out.Tasks["ship"].Output, `{"$ref": "#/$defs/ship_output"}`)
	assertJSON(t, out.Tasks["refund"].Output, `{"$ref": "#/$defs/refund_output"}`)
}

func TestGenerate_InnerDefsPromotedToRoot(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [{"id":"s1","action":{"type":"fetch","url":"http://x"}}],
		"input_schema": {
			"type": "object",
			"$defs": {
				"Address": {
					"type": "object",
					"properties": { "street": { "type": "string" } }
				}
			},
			"properties": {
				"addr": { "$ref": "#/$defs/Address" }
			}
		}
	}`)
	assertJSON(t, out.ProcessInput, `{"$ref": "#/$defs/input"}`)
	assertJSON(t, defOf(out, "input"), `{
		"type": "object",
		"properties": { "addr": { "$ref": "#/$defs/Address" } }
	}`)
	assertJSON(t, defOf(out, "Address"), `{
		"type": "object",
		"properties": { "street": { "type": "string" } }
	}`)
}

func TestGenerate_InnerDefsConflictRenamed(t *testing.T) {
	// Two distinct, same-named recursive $defs (one in input_schema, one inferred
	// from a task output) must be uniquified into two root defs. Recursive defs
	// survive normalization (they cannot be inlined), so they reach the conflict.
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "$defs": {
      "Item": {
        "type": "object",
        "properties": { "a": { "type": "string" }, "next": { "$ref": "#/$defs/Item" } },
        "required": ["a"]
      }
    },
    "properties": {
      "x": {
        "$ref": "#/$defs/Item"
      }
    },
    "required": ["x"]
  },
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "responses": { "200": {
          "type": "object",
          "$defs": {
            "Item": {
              "type": "object",
              "properties": { "b": { "type": "integer" }, "next": { "$ref": "#/$defs/Item" } },
              "required": ["b"]
            }
          },
          "properties": {
            "y": {
              "$ref": "#/$defs/Item"
            }
          },
          "required": ["y"]
        } }
      },
      "output": "$: self.result"
    }
  ]
}`)
	// The two distinct recursive defs coexist: input's keeps the name "Item", the
	// task's is carried under its output def name "charge_output" — no clobbering.
	if !out.Defs.Has("Item") {
		t.Errorf("input's recursive Item def should be present (keys: %v)", defKeys(out))
	}
	if !out.Defs.Has("charge_output") {
		t.Errorf("charge's recursive output def should be present and distinct (keys: %v)", defKeys(out))
	}
}

func TestGenerate_ChildMapSingleEntry_WithOutputSchema_ExposesTypedOutput(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_map",
        "children": {
          "out": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": { "count": { "type": "integer" } },
              "required": ["count"]
            }
          }
        }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	// spawn should appear in tasks with a typed, keyed output
	if out.Tasks["spawn"].Output.IsZero() {
		t.Fatal("spawn should have a typed output in tasks")
	}
	assertJSON(t, defOf(out, "spawn_output"), `{
		"type": "object",
		"properties": {
			"out": {
				"type": "object",
				"properties": { "count": { "type": "integer" } },
				"required": ["count"]
			}
		},
		"required": ["out"]
	}`)
}

func TestGenerate_ChildMapSingleEntry_WithoutOutputSchema_NoOutput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [{
			"id": "spawn",
			"action": { "type": "child_map", "children": { "out": { "name": "worker" } } },
			"switch": "end"
		}]
	}`)
	if _, ok := out.Tasks["spawn"]; ok {
		t.Error("spawn without result_schema should not appear in tasks")
	}
	if out.Defs.Has("spawn_output") {
		t.Error("spawn_output def should be absent")
	}
}

func TestGenerate_ChildMapSingleEntry_OutputAvailableInDownstreamStep(t *testing.T) {
	// outputs.spawn.out.count should be typed as integer in a subsequent step's input.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_map",
        "children": {
          "out": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": { "count": { "type": "integer" } },
              "required": ["count"]
            }
          }
        }
      },
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "report",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "n": "$: outputs.spawn.out.count"
        }
      }
    }
  ]
}`)
	reportInput := defOf(out, "report_input")
	if reportInput.IsZero() || !reportInput.HasProperties() {
		t.Fatal("report input should have properties")
	}
	assertJSON(t, reportInput.Properties()["n"], `{"type": "integer"}`)
}

func TestGenerate_Child_WithResultSchema_ExposesUnwrappedOutput(t *testing.T) {
	// A single child exposes the child's output DIRECTLY as self.result — not keyed
	// (child_map) and not wrapped in an array (child_list). spawn_output must therefore be
	// the result_schema itself.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child",
        "name": "worker",
        "result_schema": {
          "type": "object",
          "properties": { "count": { "type": "integer" } },
          "required": ["count"]
        }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	if out.Tasks["spawn"].Output.IsZero() {
		t.Fatal("spawn should have a typed output in tasks")
	}
	assertJSON(t, defOf(out, "spawn_output"), `{
		"type": "object",
		"properties": { "count": { "type": "integer" } },
		"required": ["count"]
	}`)
}

func TestGenerate_Child_OutputAvailableInDownstreamStep(t *testing.T) {
	// outputs.spawn.count is typed directly (no intermediate key) in a later step's input —
	// the unwrapped analogue of child_map's outputs.spawn.out.count.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child",
        "name": "worker",
        "result_schema": {
          "type": "object",
          "properties": { "count": { "type": "integer" } },
          "required": ["count"]
        }
      },
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "report",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": { "n": "$: outputs.spawn.count" }
      }
    }
  ]
}`)
	reportInput := defOf(out, "report_input")
	if reportInput.IsZero() || !reportInput.HasProperties() {
		t.Fatal("report input should have properties")
	}
	assertJSON(t, reportInput.Properties()["n"], `{"type": "integer"}`)
}

func TestGenerate_Child_WithoutResultSchema_ResultNotAccessible(t *testing.T) {
	// A child's output is only accessible once its result_schema is declared — no permissive
	// fallback and no untyped/transient value. Without one, self.result does not exist:
	// referencing it anywhere (output OR switch) is a "not in schema" error.
	err := runGenerateErr(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": { "type": "child", "name": "worker" },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	if err == nil {
		t.Fatal("expected an error exporting a child self.result without a result_schema")
	}
	if !strings.Contains(err.Error(), "result_schema") {
		t.Errorf("error should point at the missing result_schema, got: %v", err)
	}

	// Member access under an output map is the same error.
	if err := runGenerateErr(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": { "type": "child", "name": "worker" },
      "switch": "end",
      "output": { "v": "$: self.result.x" }
    }
  ]
}`); err == nil {
		t.Error("expected an error exporting self.result.x without a result_schema")
	}

	// Routing on self.result in a switch is ALSO an error — the untyped result does not
	// exist in the context.
	if err := runGenerateErr(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": { "type": "child", "name": "worker" },
      "switch": [{ "case": "self.result == null", "goto": "end" }]
    }
  ]
}`); err == nil {
		t.Error("expected an error routing on a child self.result in a switch without a result_schema")
	}
}

func TestGenerate_ChildParallel_WithOutputSchemas_ExposesKeyedOutput(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_map",
        "children": {
          "left": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          },
          "right": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          }
        }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	// spawn should appear in tasks
	if out.Tasks["spawn"].Output.IsZero() {
		t.Fatal("spawn should have a typed output in tasks")
	}
	// spawn_output should be an object with left/right keys
	spawnOutput := defOf(out, "spawn_output")
	if spawnOutput.IsZero() {
		t.Fatal("spawn_output def missing")
	}
	if !spawnOutput.HasProperties() {
		t.Fatal("spawn_output should have properties")
	}
	if spawnOutput.Properties()["left"].IsZero() {
		t.Error("spawn_output should have property 'left'")
	}
	if spawnOutput.Properties()["right"].IsZero() {
		t.Error("spawn_output should have property 'right'")
	}
}

func TestGenerate_ChildParallel_KeyedOutputAvailableInDownstreamStep(t *testing.T) {
	// outputs.spawn.left.num should be typed as integer in a subsequent step.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_map",
        "children": {
          "left": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          },
          "right": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          }
        }
      },
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "aggregate",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "a": "$: outputs.spawn.left.num",
          "b": "$: outputs.spawn.right.num"
        }
      }
    }
  ]
}`)
	aggInput := defOf(out, "aggregate_input")
	if aggInput.IsZero() || !aggInput.HasProperties() {
		t.Fatal("aggregate input should have properties")
	}
	assertJSON(t, aggInput.Properties()["a"], `{"type": "integer"}`)
	assertJSON(t, aggInput.Properties()["b"], `{"type": "integer"}`)
}

func TestGenerate_ChildParallel_MixedOutputSchemas_UntypedKeyOmitted(t *testing.T) {
	// Per-key rule: a child WITHOUT result_schema is omitted from the output type entirely —
	// its output is not accessible or exportable (no permissive fallback). Only the schema-
	// bearing key survives.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_map",
        "children": {
          "typed": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "ok": {
                  "type": "boolean"
                }
              },
              "required": [
                "ok"
              ]
            }
          },
          "untyped": {
            "name": "other"
          }
        }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	spawnOutput := defOf(out, "spawn_output")
	if spawnOutput.IsZero() || !spawnOutput.HasProperties() {
		t.Fatal("spawn_output def missing or has no properties")
	}
	if spawnOutput.Properties()["typed"].IsZero() {
		t.Error("spawn_output should have property 'typed'")
	}
	if !spawnOutput.Properties()["untyped"].IsZero() {
		t.Error("spawn_output should NOT expose 'untyped' (no result_schema → omitted)")
	}
}

// A child_map in which NO child declares a result_schema has an untyped result: self.result
// does not exist, so referencing it — in an output OR a switch — is an error.
func TestGenerate_ChildMap_NoResultSchemas_ResultNotAccessible(t *testing.T) {
	err := runGenerateErr(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": { "type": "child_map", "children": { "a": { "name": "x" }, "b": { "name": "y" } } },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	if err == nil {
		t.Fatal("expected an error exporting a child_map result with no result_schemas")
	}
	if !strings.Contains(err.Error(), "result_schema") {
		t.Errorf("error should point at the missing result_schema, got: %v", err)
	}

	// Routing on it in a switch is the same error.
	if err := runGenerateErr(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": { "type": "child_map", "children": { "a": { "name": "x" }, "b": { "name": "y" } } },
      "switch": [{ "case": "self.result == null", "goto": "end" }]
    }
  ]
}`); err == nil {
		t.Error("expected an error routing on a schema-less child_map self.result in a switch")
	}
}

// A child_list without a result_schema has an untyped array result: self.result does not
// exist, so referencing it — in an output OR a switch — is an error.
func TestGenerate_ChildList_NoResultSchema_ResultNotAccessible(t *testing.T) {
	err := runGenerateErr(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": { "items": { "type": "array", "items": { "type": "object" } } },
    "required": ["items"]
  },
  "tasks": [
    {
      "id": "spread",
      "action": { "type": "child_list", "name": "worker", "over": "$: input.items" },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	if err == nil {
		t.Fatal("expected an error exporting a child_list result with no result_schema")
	}
	if !strings.Contains(err.Error(), "result_schema") {
		t.Errorf("error should point at the missing result_schema, got: %v", err)
	}

	// Routing on it in a switch is the same error.
	if err := runGenerateErr(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": { "items": { "type": "array", "items": { "type": "object" } } },
    "required": ["items"]
  },
  "tasks": [
    {
      "id": "spread",
      "action": { "type": "child_list", "name": "worker", "over": "$: input.items" },
      "switch": [{ "case": "self.result == null", "goto": "end" }]
    }
  ]
}`); err == nil {
		t.Error("expected an error routing on a schema-less child_list self.result in a switch")
	}
}

func TestGenerate_UnusedDefsRemoved(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"$defs": {
				"Used":   { "type": "string" },
				"Unused": { "type": "integer" }
			},
			"properties": { "x": { "$ref": "#/$defs/Used" } }
		},
		"tasks": [{"id":"s1","action":{"type":"fetch","url":"http://x"}}]
	}`)
	if !out.Defs.Has("Used") {
		t.Error("Used def should be present in $defs")
	}
	if out.Defs.Has("Unused") {
		t.Error("Unused def should have been removed")
	}
}

func TestGenerate_ChildFromArray_OutputIsTypedArray(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {
      "items": {
        "type": "array",
        "items": {"type": "object", "properties": {"n": {"type": "integer"}}, "required": ["n"]}
      }
    },
    "required": ["items"]
  },
  "tasks": [
    {
      "id": "spread",
      "action": {
        "type": "child_list",
        "name": "worker",
        "over": "$: input.items",
        "result_schema": {
          "type": "object",
          "properties": {"doubled": {"type": "integer"}},
          "required": ["doubled"]
        }
      },
      "switch": "end",
      "output": "$: self.result"
    }
  ]
}`)
	spreadOutput := defOf(out, "spread_output")
	if spreadOutput.IsZero() {
		t.Fatal("spread_output def missing")
	}
	if !spreadOutput.IsType("array") {
		t.Fatalf("spread_output should be an array, got %q", spreadOutput.TypeName())
	}
	item := spreadOutput.Items()
	if item.IsZero() || !item.HasProperties() {
		t.Fatal("spread_output items should be a typed object")
	}
	if item.Properties()["doubled"].IsZero() {
		t.Error("spread_output item should carry the result_schema property 'doubled'")
	}
}

func TestGenerate_ChildFromArray_ArrayElementTypedInDownstream(t *testing.T) {
	// outputs.spread.0.doubled must be typed as integer via array index access.
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {
      "items": {
        "type": "array",
        "items": {"type": "object", "properties": {"n": {"type": "integer"}}, "required": ["n"]}
      }
    },
    "required": ["items"]
  },
  "tasks": [
    {
      "id": "spread",
      "action": {
        "type": "child_list",
        "name": "worker",
        "over": "$: input.items",
        "result_schema": {
          "type": "object",
          "properties": {"doubled": {"type": "integer"}},
          "required": ["doubled"]
        }
      },
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "use",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {"first": "$: outputs.spread[0].doubled"}
      }
    }
  ]
}`)
	useInput := defOf(out, "use_input")
	if useInput.IsZero() || !useInput.HasProperties() {
		t.Fatal("use input should have properties")
	}
}

func TestGenerate_ChildFromArray_OverMustBeArray(t *testing.T) {
	err := runGenerateErr(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {"n": {"type": "integer"}},
    "required": ["n"]
  },
  "tasks": [
    {
      "id": "spread",
      "action": {"type": "child_list", "name": "worker", "over": "$: input.n"},
      "switch": "end"
    }
  ]
}`)
	if err == nil {
		t.Fatal("expected error: over must evaluate to an array")
	}
}
