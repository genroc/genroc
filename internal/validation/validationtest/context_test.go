package validationtest

import (
	"testing"
)

func TestGenerate_ContextSets_LinearChain_RequiredOutputNonNullable(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "A",
      "action": {
        "type": "fetch",
        "url": "http://x",
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
      "output": "$: self.result"
    },
    {
      "id": "B",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "flag": "$: outputs.A.ok"
        }
      }
    }
  ]
}`)
	assertJSON(t, out.Tasks["B"].Input, `{"$ref": "#/$defs/B_input"}`)
	bInput := defOf(out, "B_input")
	if bInput.IsZero() || !bInput.HasProperties() {
		t.Fatal("B input should have properties")
	}
	assertJSON(t, bInput.Properties()["flag"], `{"type": "boolean"}`)
}

func TestGenerate_ContextSets_ExclusiveBranch_SkippedStepOutputNullable(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {
      "take_fast": {
        "type": "boolean"
      }
    },
    "required": [
      "take_fast"
    ]
  },
  "tasks": [
    {
      "id": "gate",
      "switch": [
        {
          "case": "input.take_fast",
          "goto": "$fast"
        },
        {
          "goto": "$slow"
        }
      ]
    },
    {
      "id": "fast",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "speed": {
              "type": "number"
            }
          },
          "required": [
            "speed"
          ]
        }
      },
      "output": "$: self.result"
    },
    {
      "id": "slow",
      "action": {
        "type": "fetch",
        "url": "http://x"
      }
    },
    {
      "id": "merge",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "s": "$: outputs.fast.speed"
        }
      }
    }
  ]
}`)
	assertJSON(t, out.Tasks["merge"].Input, `{"$ref": "#/$defs/merge_input"}`)
	mergeInput := defOf(out, "merge_input")
	if mergeInput.IsZero() || !mergeInput.HasProperties() {
		t.Fatal("merge input should have properties")
	}
	assertJSON(t, mergeInput.Properties()["s"], `{"type": ["number", "null"]}`)
}

func TestGenerate_ContextSets_PreBranchStepRequiredAtAllMergePoints(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "pre",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "id": {
              "type": "integer"
            }
          },
          "required": [
            "id"
          ]
        }
      },
      "output": "$: self.result"
    },
    {
      "id": "gate",
      "switch": [
        {
          "case": "outputs.pre.id == 1",
          "goto": "$path_a"
        },
        {
          "goto": "$path_b"
        }
      ]
    },
    {
      "id": "path_a",
      "action": {
        "type": "fetch",
        "url": "http://x"
      }
    },
    {
      "id": "path_b",
      "action": {
        "type": "fetch",
        "url": "http://x"
      }
    },
    {
      "id": "post",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "pre_id": "$: outputs.pre.id"
        }
      }
    }
  ]
}`)
	assertJSON(t, out.Tasks["post"].Input, `{"$ref": "#/$defs/post_input"}`)
	postInput := defOf(out, "post_input")
	if postInput.IsZero() || !postInput.HasProperties() {
		t.Fatal("post input should have properties")
	}
	assertJSON(t, postInput.Properties()["pre_id"], `{"type": "integer"}`)
}

func TestGenerate_ContextSets_DefaultEndSwitch_SuccessorRequiredNotOptional(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "decide",
      "action": {
        "type": "fetch",
        "url": "http://x",
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
      "switch": [
        {
          "case": "self.result.ok",
          "goto": "$work"
        },
        {
          "goto": "end"
        }
      ],
      "output": "$: self.result"
    },
    {
      "id": "work",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "flag": "$: outputs.decide.ok"
        }
      }
    }
  ]
}`)
	assertJSON(t, out.Tasks["work"].Input, `{"$ref": "#/$defs/work_input"}`)
	workInput := defOf(out, "work_input")
	if workInput.IsZero() || !workInput.HasProperties() {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, workInput.Properties()["flag"], `{"type": "boolean"}`)
}

func TestGenerate_OnError_MixedPath_FailingStepOutputNullable(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "start",
      "action": {
        "type": "fetch",
        "url": "http://x",
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
      "on_error": [
        {
          "goto": "$finale"
        }
      ],
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "finale",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "val": "$: outputs.start.ok",
          "errCode": "$: error.code"
        }
      },
      "switch": "end"
    }
  ]
}`)
	finaleInput := defOf(out, "finale_input")
	if finaleInput.IsZero() || !finaleInput.HasProperties() {
		t.Fatal("finale input should have properties")
	}
	// On the normal path start produced output; on the error path it did not.
	assertJSON(t, finaleInput.Properties()["val"], `{"type": ["boolean", "null"]}`)
	// error is only present on the error path, so it is nullable.
	assertJSON(t, finaleInput.Properties()["errCode"], `{"type": ["string", "null"]}`)
}

func TestGenerate_OnError_ExclusivePath_ErrorRequiredOutputAbsent(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "worker",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "result": {
              "type": "string"
            }
          },
          "required": [
            "result"
          ]
        }
      },
      "switch": [
        {
          "goto": "end"
        }
      ],
      "on_error": [
        {
          "goto": "$handler"
        }
      ],
      "output": "$: self.result"
    },
    {
      "id": "handler",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "code": "$: error.code"
        }
      },
      "switch": "end"
    }
  ]
}`)
	handlerInput := defOf(out, "handler_input")
	if handlerInput.IsZero() || !handlerInput.HasProperties() {
		t.Fatal("handler input should have properties")
	}
	// Every path to handler is an error path, so error.code is always a string.
	assertJSON(t, handlerInput.Properties()["code"], `{"type": "string"}`)
}

func TestGenerate_Switch_ScalarNext_CreatesSequentialEdge(t *testing.T) {
	// "switch": "next" (scalar) must behave identically to [{"goto": "next"}].
	// a always precedes b, so a.ok is required (non-nullable) at b.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "a",
      "action": {
        "type": "fetch",
        "url": "http://x",
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
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "b",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "flag": "$: outputs.a.ok"
        }
      },
      "switch": "end"
    }
  ]
}`)
	bInput := defOf(out, "b_input")
	if bInput.IsZero() || !bInput.HasProperties() {
		t.Fatal("b input should have properties")
	}
	assertJSON(t, bInput.Properties()["flag"], `{"type": "boolean"}`)
}

func TestGenerate_Switch_ScalarStepRef_CreatesJumpEdge(t *testing.T) {
	// "switch": "$fast" (scalar) must behave identically to [{"goto": "$fast"}].
	// gate unconditionally jumps to fast, so fast always runs and its output
	// is required (non-nullable) at merge — in contrast to a conditional branch
	// where fast could be skipped (making it nullable).
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "gate",
      "switch": "$fast"
    },
    {
      "id": "fast",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "speed": {
              "type": "number"
            }
          },
          "required": [
            "speed"
          ]
        }
      },
      "switch": "next",
      "output": "$: self.result"
    },
    {
      "id": "merge",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "body": {
          "s": "$: outputs.fast.speed"
        }
      },
      "switch": "end"
    }
  ]
}`)
	mergeInput := defOf(out, "merge_input")
	if mergeInput.IsZero() || !mergeInput.HasProperties() {
		t.Fatal("merge input should have properties")
	}
	assertJSON(t, mergeInput.Properties()["s"], `{"type": "number"}`)
}

func TestGenerate_OnError_EndTerminal_RecognisedAsTerminal(t *testing.T) {
	// runGenerate fails the test on any Generate error, so a clean return is sufficient.
	runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "task",
      "action": {
        "type": "fetch",
        "url": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "result": {
              "type": "string"
            }
          },
          "required": [
            "result"
          ]
        }
      },
      "on_error": [
        {
          "goto": "end"
        }
      ],
      "output": "$: self.result"
    }
  ],
  "output": {
    "result": "$: outputs.task.result"
  }
}`)
}
