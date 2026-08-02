package validationtest

import (
	"strings"
	"testing"
)

// The process output expression is typed once per TERMINAL PATH and the results joined,
// instead of once against a context that has already intersected the paths' must-sets.
//
// The intersection is what used to lose the correlation: two task outputs that between
// them cover every way of ending both came out merely "optional", so
// `outputs.a.v ?? outputs.b.v` was nullable even though exactly one of them is always set.
// Per terminal each side is either its real type or null, `??` resolves as it does at
// runtime, and the join is non-null.

// twoWayDef is the canonical shape: `a` succeeds and ends, or routes to `b` on error and
// `b` ends. Exactly one of the two outputs exists at the boundary, never both, never
// neither. outExpr is the process output expression under test.
func twoWayDef(outExpr string) string {
	return `{
		"name": "two_way",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}
				},
				"on_error": [{"code":["http.422"],"goto":"$b"}],
				"output": {"v": "$: self.result.v"},
				"switch": "end"
			},
			{"id": "b", "output": {"v": false}, "switch": "end"}
		],
		"output": {"ok": ` + outExpr + `}
	}`
}

func outputProp(t *testing.T, defJSON, prop string) string {
	t.Helper()
	out := runGenerate(t, defJSON)
	got, err := defOf(out, "output").At(prop)
	if err != nil {
		t.Fatalf("navigate output.%s: %v", prop, err)
	}
	return jsonString(t, got)
}

func TestPathSensitive_CoveringPairIsNonNull(t *testing.T) {
	got := outputProp(t, twoWayDef(`"$: outputs.a.v ?? outputs.b.v"`), "ok")
	if got != `{"type":"boolean"}` {
		t.Errorf("ok = %s, want {\"type\":\"boolean\"}\n"+
			"a and b are the only two terminals, so one of them is always set — coalescing "+
			"across both can never produce null", got)
	}
}

func TestPathSensitive_OrderOfOperandsDoesNotMatter(t *testing.T) {
	got := outputProp(t, twoWayDef(`"$: outputs.b.v ?? outputs.a.v"`), "ok")
	if got != `{"type":"boolean"}` {
		t.Errorf("ok = %s, want non-null; coverage is a property of the pair, not of which side is written first", got)
	}
}

func TestPathSensitive_SingleOperandStaysNullable(t *testing.T) {
	// Precision must not leak into unsoundness: one branch alone really can be absent.
	got := outputProp(t, twoWayDef(`"$: outputs.a.v"`), "ok")
	if got != `{"type":["boolean","null"]}` {
		t.Errorf("ok = %s, want boolean|null\n"+
			"reading one branch's output alone must stay nullable — it is genuinely absent "+
			"on the other terminal", got)
	}
}

func TestPathSensitive_UncoveredTerminalKeepsItNullable(t *testing.T) {
	// A third way to end, on which NEITHER output is set. The pair no longer covers every
	// terminal, so the result must go back to nullable.
	def := `{
		"name": "three_way",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}
				},
				"on_error": [{"code":["http.422"],"goto":"$b"},{"code":["http.500"],"goto":"$c"}],
				"output": {"v": "$: self.result.v"},
				"switch": "end"
			},
			{"id": "b", "output": {"v": false}, "switch": "end"},
			{"id": "c", "switch": "end"}
		],
		"output": {"ok": "$: outputs.a.v ?? outputs.b.v"}
	}`
	got := outputProp(t, def, "ok")
	if got != `{"type":["boolean","null"]}` {
		t.Errorf("ok = %s, want boolean|null\n"+
			"task c ends without setting either output, so the pair does not cover every "+
			"terminal and the result really can be null", got)
	}
}

func TestPathSensitive_GenuinelyNullableOutputStaysNullable(t *testing.T) {
	// Coverage says an output is PRESENT, not that it is non-null. If a branch can itself
	// produce null, `??` falls through to the other side — which is absent on that
	// terminal — so the result is nullable and must be typed that way.
	def := `{
		"name": "nullable_branch",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":["boolean","null"]}},"required":["v"]}
				},
				"on_error": [{"code":["http.422"],"goto":"$b"}],
				"output": {"v": "$: self.result.v"},
				"switch": "end"
			},
			{"id": "b", "output": {"v": false}, "switch": "end"}
		],
		"output": {"ok": "$: outputs.a.v ?? outputs.b.v"}
	}`
	got := outputProp(t, def, "ok")
	if got != `{"type":["boolean","null"]}` {
		t.Errorf("ok = %s, want boolean|null\n"+
			"a.v can be a real null, and on a's terminal b.v is absent — so the coalesce "+
			"can still yield null. Covering the paths must not be mistaken for non-nullability", got)
	}
}

func TestPathSensitive_TrailingDefaultOnAnUncoveredPairIsNonNull(t *testing.T) {
	// The author's escape hatch when coverage is genuinely incomplete.
	def := `{
		"name": "three_way_default",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}
				},
				"on_error": [{"code":["http.422"],"goto":"$b"},{"code":["http.500"],"goto":"$c"}],
				"output": {"v": "$: self.result.v"},
				"switch": "end"
			},
			{"id": "b", "output": {"v": false}, "switch": "end"},
			{"id": "c", "switch": "end"}
		],
		"output": {"ok": "$: outputs.a.v ?? outputs.b.v ?? false"}
	}`
	got := outputProp(t, def, "ok")
	if got != `{"type":"boolean"}` {
		t.Errorf("ok = %s, want boolean; a non-null literal ends the chain on every terminal", got)
	}
}

func TestPathSensitive_ErrorEndTerminalCounts(t *testing.T) {
	// `on_error: goto end` is a terminal too, and no task output is set on it. It must be
	// enumerated, or the analysis would claim coverage it does not have.
	def := `{
		"name": "err_end",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}
				},
				"on_error": [{"code":["http.500"],"goto":"end"}],
				"output": {"v": "$: self.result.v"},
				"switch": "end"
			}
		],
		"output": {"ok": "$: outputs.a.v"}
	}`
	got := outputProp(t, def, "ok")
	if got != `{"type":["boolean","null"]}` {
		t.Errorf("ok = %s, want boolean|null\n"+
			"the on_error end terminal produces no output for a, so a.v is genuinely absent there", got)
	}
}

func TestPathSensitive_ReferenceToATaskNoPathProducesIsStillAnError(t *testing.T) {
	// Absent-on-this-terminal is typed null so `??` can fall through. That must NOT
	// degrade into "any unknown task id reads as null" — a reference nothing can produce
	// has to stay an error, or a typo becomes a silent null.
	err := runGenerateErr(t, twoWayDef(`"$: outputs.nosuch.v ?? outputs.a.v"`))
	if err == nil {
		t.Fatal("a reference to a task no terminal produces must stay an error, not read as null")
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error %q must name the unknown task so the typo is findable", err)
	}
}

func TestPathSensitive_SingleTerminalBehaviourUnchanged(t *testing.T) {
	// One terminal takes the un-partitioned path; it must produce exactly what it always did.
	def := `{
		"name": "one_way",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}
				},
				"output": {"v": "$: self.result.v"},
				"switch": "end"
			}
		],
		"output": {"ok": "$: outputs.a.v"}
	}`
	got := outputProp(t, def, "ok")
	if got != `{"type":"boolean"}` {
		t.Errorf("ok = %s, want boolean; a's output is guaranteed on the only terminal", got)
	}
}

func TestPathSensitive_ErrorOnEveryTerminalKeepsItsPlainMessage(t *testing.T) {
	// An expression that is simply wrong fails everywhere. Prefixing it with a path would
	// be misdirection — the path is not what is wrong.
	err := runGenerateErr(t, twoWayDef(`"$: outputs.a.v + 1"`))
	if err == nil {
		t.Fatal("adding 1 to a boolean must fail")
	}
	if strings.Contains(err.Error(), "on the path ending at") {
		t.Errorf("error %q was prefixed with a path, but it fails on every terminal — the "+
			"prefix should be reserved for genuinely path-specific failures", err)
	}
}

func TestPathSensitive_PathSpecificErrorNamesThePath(t *testing.T) {
	// `b.v` is a string, `a.v` a boolean. Comparing a.v to a string fails only on b's
	// terminal, where a.v is null. Naming the path is the difference between a usable
	// message and a baffling one.
	def := `{
		"name": "path_specific",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}
				},
				"on_error": [{"code":["http.422"],"goto":"$b"}],
				"output": {"n": "$: self.result.n"},
				"switch": "end"
			},
			{"id": "b", "output": {"n": 0}, "switch": "end"}
		],
		"output": {"ok": "$: outputs.a.n > 3"}
	}`
	err := runGenerateErr(t, def)
	if err == nil {
		t.Fatal("comparing a possibly-absent output must fail on the terminal where it is absent")
	}
	if !strings.Contains(err.Error(), "on the path ending at") {
		t.Errorf("error %q must name the terminal it failed on; the expression is fine on "+
			"the other one, so an unqualified message sends the author looking in the wrong place", err)
	}
}

func TestPathSensitive_TaskContextsAreStillCollapsed(t *testing.T) {
	// Documents the deliberate limit. Mid-process contexts come from a fixpoint whose
	// lattice element is ONE must-set; making them path-sensitive needs a DNF lattice and
	// is deferred (see docs/path-sensitive-output.md). Inside `c`, the same coalesce that
	// is non-null at the output boundary is still nullable here.
	def := `{
		"name": "task_ctx",
		"tasks": [
			{
				"id": "a",
				"action": {
					"type": "fetch", "url": "http://x/",
					"result_schema": {"type":"object","properties":{"v":{"type":"boolean"}},"required":["v"]}
				},
				"on_error": [{"code":["http.422"],"goto":"$b"}],
				"output": {"v": "$: self.result.v"},
				"switch": "$c"
			},
			{"id": "b", "output": {"v": false}, "switch": "$c"},
			{"id": "c", "output": {"joined": "$: outputs.a.v ?? outputs.b.v ?? false"}, "switch": "end"}
		],
		"output": {"ok": "$: outputs.c.joined"}
	}`
	// It must at least still COMPILE — the trailing default is how an author works around
	// the collapsed context today, and that has to keep working.
	got := outputProp(t, def, "ok")
	if got != `{"type":"boolean"}` {
		t.Errorf("ok = %s, want boolean via the trailing default", got)
	}
}
