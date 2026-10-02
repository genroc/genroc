package validationtest

import (
	"testing"
)

// Tests for `map` expressions. Definition fixtures and builders live in
// mapcases_test.go, so each test here is: build a shape, generate, assert.

// A child_list fanning out over a mapped array is the headline use case: no
// per-element task, the reshape happens in the `over` expression itself.
func TestGenerateMap_ChildListOverMappedInput(t *testing.T) {
	runGenerate(t, mapFanoutDef("map-line-worker"))
}

// If map's result type leaked the source element, this child (which knows nothing of
// `code`/`count`) would be reported incompatible.
func TestGenerateMap_ChildListDerivedInputIsMappedElement(t *testing.T) {
	getter := stubGetter{
		"map-line-worker": childDef(t, "map-line-worker", `{
			"type": "object",
			"properties": {"sku": {"type": "string"}, "qty": {"type": "integer"}},
			"required": ["sku", "qty"]
		}`),
	}
	assertMapChildRefsOK(t, mapFanoutDef("map-line-worker"), getter, "mapped element type")
}

// The derived element type is checked, or every child_list over a map would go unchecked.
func TestGenerateMap_ChildListDerivedInputMismatchRejected(t *testing.T) {
	getter := stubGetter{
		"map-line-worker": childDef(t, "map-line-worker", `{
			"type": "object",
			"properties": {"sku": {"type": "string"}, "qty": {"type": "string"}},
			"required": ["sku", "qty"]
		}`),
	}
	assertMapChildRefsIncompatible(t, mapFanoutDef("map-line-worker"), getter,
		"qty is integer, child wants string")
}

// A map over a nullable source panics in the evaluator, so it must fail at registration.
func TestGenerateMap_OverNullableSourceRejected(t *testing.T) {
	got := mapGenerateErr(t, mapDef("map-nullable-over", mapNullableRowsInput,
		mapChildListTask("fanout", "map-line-worker", "$: map(input.rows, r => {sku: r.code})")),
		"a map over a nullable source")
	mapErrMentions(t, got, "may be null", "point at the null source")
	mapErrMentions(t, got, "??", "point at the ?? fix")
}

// The empty-array variant is provably empty, so `?? []` must not degrade the element type.
func TestGenerateMap_OverNullableSourceWithCoalesceOK(t *testing.T) {
	out := runGenerate(t, mapDef("map-coalesce-over", mapNullableRowsInput,
		mapChildListEchoTask("fanout", "map-line-worker", "$: map(input.rows ?? [], r => {sku: r.code})")))
	// The output mirrors `over`, so this pins the element type the ?? [] form
	// preserves: string, not an unconstrained any.
	assertJSON(t, defOf(out, "fanout_output"), `{
		"type": "array",
		"items": {"type": "object", "properties": {"sku": {"type": "string"}}, "required": ["sku"]}
	}`)
}

// The generated input schema is what the UI and external consumers see, so the array and its
// element must both be typed.
func TestGenerateMap_FetchBodyFromMapAndObjectLiterals(t *testing.T) {
	out := runGenerate(t, mapRowsFetchDef("map-body", `{
		"type": "fetch",
		"method": "post",
		"url": "http://x",
		"body": {
			"lines": "$: map(input.rows, r => {sku: r.code, qty: r.count + 1})",
			"meta": "$: {total: 1, kind: \"order\"}"
		}
	}`))
	assertJSON(t, out.Tasks["push"].Input, `{"$ref": "#/$defs/push_input"}`)
	assertJSON(t, defOf(out, "push_input"), `{
		"type": "object",
		"properties": {
			"lines": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {"qty": {"type": "integer"}, "sku": {"type": "string"}},
					"required": ["qty", "sku"]
				}
			},
			"meta": {
				"type": "object",
				"properties": {"kind": {"type": "string"}, "total": {"type": "integer"}},
				"required": ["kind", "total"]
			}
		},
		"required": ["lines", "meta"]
	}`)
}

// resolveURL renders with %v, so an array-valued url would go out as "[a b c]".
func TestGenerateMap_FetchURLFromMapRejected(t *testing.T) {
	got := mapGenerateErr(t, mapRowsFetchDef("map-url",
		`{"type": "fetch", "method": "post", "url": "$: map(input.rows, r => r.code)"}`),
		"an array-valued fetch url")
	mapErrMentions(t, got, "push", "name the offending task")
}

// resolveMethod upper-cases %v, so an array method would put the verb "[A B]" on the wire.
func TestGenerateMap_FetchMethodFromMapRejected(t *testing.T) {
	got := mapGenerateErr(t, mapRowsFetchDef("map-method",
		`{"type": "fetch", "url": "http://x", "method": "$: map(input.rows, r => r.code)"}`),
		"an array-valued fetch method")
	mapErrMentions(t, got, "push", "name the offending task")
}

// Headers must resolve to an object; a map yields an array, which would fail at
// runtime in resolveHeaders. The error names the task so the author can find it.
func TestGenerateMap_FetchHeadersFromMapRejected(t *testing.T) {
	got := mapGenerateErr(t, mapRowsFetchDef("map-headers",
		`{"type": "fetch", "method": "post", "url": "http://x", "headers": "$: map(input.rows, r => r.code)"}`),
		"array-valued headers")
	mapErrMentions(t, got, `task "push" headers`, "name the task and the headers position")
}

// An object literal is the natural way to build headers from the context, and
// it satisfies the non-null-object requirement without a literal shape map.
func TestGenerateMap_FetchHeadersFromObjectLiteralOK(t *testing.T) {
	credsInput := `{
		"type": "object",
		"properties": {"token": {"type": "string"}, "tenant": {"type": "string"}},
		"required": ["token", "tenant"]
	}`
	runGenerate(t, mapDef("map-headers-ok", credsInput, mapFetchTask("push", `{
		"type": "fetch",
		"method": "post",
		"url": "http://x",
		"headers": "$: {Authorization: input.token, Tenant: input.tenant}"
	}`)))
}

// The exported output is the array of lambda bodies; downstream tasks type-check against it.
func TestGenerateMap_OutputOverSelfResult(t *testing.T) {
	out := runGenerate(t, `{
		"name": "map-self-result",
		"tasks": [
			{
				"id": "load",
				"action": {
					"type": "fetch",
					"method": "post",
					"url": "http://x",
					"responses": { "200": {
						"type": "object",
						"properties": {
							"items": {
								"type": "array",
								"items": {
									"type": "object",
									"properties": {"id": {"type": "string"}, "price": {"type": "number"}},
									"required": ["id", "price"]
								}
							}
						},
						"required": ["items"]
					} }
				},
				"switch": "end",
				"output": {"skus": "$: map(self.result.items, i => {ref: i.id, cents: i.price * 100})"}
			}
		]
	}`)
	assertJSON(t, defOf(out, "load_output"), `{
		"type": "object",
		"properties": {
			"skus": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {"cents": {"type": "number"}, "ref": {"type": "string"}},
					"required": ["cents", "ref"]
				}
			}
		},
		"required": ["skus"]
	}`)
}

// outputs.<id> is a $ref, so map must resolve it to read `items`.
func TestGenerateMap_OutputOverOtherTaskOutput(t *testing.T) {
	out := runGenerate(t, mapRowsDef("map-other-output", `
		{
			"id": "load",
			"output": {"rows": "$: map(input.rows, r => {code: r.code, n: r.count})"},
			"switch": "next"
		},
		{
			"id": "summarise",
			"output": {"codes": "$: map(outputs.load.rows, r => r.code)"},
			"switch": "end"
		}`))
	assertJSON(t, defOf(out, "summarise_output"), `{
		"type": "object",
		"properties": {"codes": {"type": "array", "items": {"type": "string"}}},
		"required": ["codes"]
	}`)
}

// The process output is the public result of an instance, so a map there must
// produce the same typed array as anywhere else.
func TestGenerateMap_ProcessOutput(t *testing.T) {
	out := runGenerate(t, mapProcessOutputDef("map-process-output", mapRowsInput,
		`{"skus": "$: map(input.rows, r => {sku: r.code})"}`))
	assertJSON(t, out.ProcessOutput, `{"$ref": "#/$defs/output"}`)
	assertJSON(t, defOf(out, "output"), `{
		"type": "object",
		"properties": {
			"skus": {
				"type": "array",
				"items": {"type": "object", "properties": {"sku": {"type": "string"}}, "required": ["sku"]}
			}
		},
		"required": ["skus"]
	}`)
}

// The subset check against the child's input_schema is the only place this is verified.
func TestGenerateMap_ChildMapInputFromMap(t *testing.T) {
	getter := stubGetter{
		"map-batch-worker": childDef(t, "map-batch-worker", `{
			"type": "object",
			"properties": {
				"lines": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {"sku": {"type": "string"}, "qty": {"type": "integer"}},
						"required": ["sku", "qty"]
					}
				},
				"source": {"type": "string"}
			},
			"required": ["lines", "source"]
		}`),
	}
	def := mapChildMapDef("map-child-map", "map-batch-worker", `{
		"lines": "$: map(input.rows, r => {sku: r.code, qty: r.count})",
		"source": "upload"
	}`)
	assertMapChildRefsOK(t, def, getter, "mapped child_map input")
}

// ...and an incompatible one must not. Here the lambda body drops `qty`, which
// the child requires.
func TestGenerateMap_ChildMapInputFromMapMismatchRejected(t *testing.T) {
	getter := stubGetter{
		"map-batch-worker": childDef(t, "map-batch-worker", `{
			"type": "object",
			"properties": {
				"lines": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {"sku": {"type": "string"}, "qty": {"type": "integer"}},
						"required": ["sku", "qty"]
					}
				}
			},
			"required": ["lines"]
		}`),
	}
	def := mapChildMapDef("map-child-map-bad", "map-batch-worker",
		`{"lines": "$: map(input.rows, r => {sku: r.code})"}`)
	assertMapChildRefsIncompatible(t, def, getter, "mapped element is missing qty")
}

// An array is neither true nor false, so accepting it would leave the branch undefined.
func TestGenerateMap_SwitchCaseFromMapRejected(t *testing.T) {
	got := mapGenerateErr(t, mapRowsDef("map-switch", `
		{
			"id": "route",
			"switch": [
				{"case": "map(input.rows, r => r.code)", "goto": "$work"},
				{"goto": "end"}
			]
		},
		{"id": "work", "action": {"type": "fetch", "method": "post", "url": "http://x"}, "switch": "end"}`),
		"a non-boolean (array) switch case")
	mapErrMentions(t, got, "boolean", "say a case must be boolean")
	mapErrMentions(t, got, `task "route"`, "name the offending task")
}

// Without the element type bound, a typo in a lambda body would surface as a runtime null in a
// request body.
func TestGenerateMap_UnknownFieldInLambdaBodyRejected(t *testing.T) {
	got := mapGenerateErr(t, mapRowsFetchDef("map-bad-field", `{
		"type": "fetch",
		"method": "post",
		"url": "http://x",
		"body": {"lines": "$: map(input.rows, r => {qty: r.total})"}
	}`), "an unknown field in the lambda body")
	mapErrMentions(t, got, "total", "name the unknown field")
	mapErrMentions(t, got, `task "push" body`, "attribute the failure to the task and the body position")
}

// An error that does not name the task is unusable. Each definition starts with a healthy
// "prepare" task, so only a message naming the SECOND task passes.

func TestGenerateMap_ErrorNamesTask_OverPosition(t *testing.T) {
	got := mapGenerateErr(t, mapDef("map-attr-over", mapNullableRowsInput,
		mapPrepareTask+","+
			mapChildListTask("fanout", "map-line-worker", "$: map(input.rows, r => {sku: r.code})")),
		"a map over a nullable source in the second task")
	mapErrMentions(t, got, `task "fanout" over`, "name the task and the over position")
}

func TestGenerateMap_ErrorNamesTask_HeadersPosition(t *testing.T) {
	got := mapGenerateErr(t, mapRowsDef("map-attr-headers",
		mapPrepareTask+","+mapFetchTask("push",
			`{"type": "fetch", "method": "post", "url": "http://x", "headers": "$: map(input.rows, r => r.code)"}`)),
		"array-valued headers on the second task")
	mapErrMentions(t, got, `task "push" headers`, "name the task and the headers position")
}

func TestGenerateMap_ErrorNamesTask_SwitchPosition(t *testing.T) {
	got := mapGenerateErr(t, mapRowsDef("map-attr-switch",
		mapPrepareTask+`,
		{"id": "route", "switch": [{"case": "map(input.rows, r => r.code)", "goto": "end"}, {"goto": "end"}]}`),
		"a non-boolean switch case on the second task")
	mapErrMentions(t, got, `task "route" switch case`, "name the task and the switch position")
}

// An output map is inferred in phase 1 (inferOutputs), not buildInputs, so it must label its
// own errors with the task id.
func TestGenerateMap_OutputMapErrorNamesTask(t *testing.T) {
	got := mapGenerateErr(t, mapRowsDef("map-attr-output",
		mapPrepareTask+`,
		{"id": "shape", "output": {"skus": "$: map(input.rows, r => r.nope)"}, "switch": "end"}`),
		"an unknown field in an output map")
	mapErrMentions(t, got, "nope", "name the unknown field")
	mapErrMentions(t, got, "shape", `name the offending task "shape"`)
}
