package validationtest

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// Compare is a shape check over two documents, and its whole value is catching the
// accidental break: a required input appearing, an output whose type changed. Two of the
// cases here pin hazards the design ACCEPTS (a dropped `secret`, a flipped `only_once`)
// as compatible-with-a-changed-slot, so a later change cannot quietly turn them into
// refusals. See specs/version-compatibility.md §7.

func defFrom(t *testing.T, src string) *model.ProcessDefinition {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(src), &def); err != nil {
		t.Fatalf("unmarshal definition: %v\n%s", err, src)
	}
	return &def
}

func compareDefs(t *testing.T, oldSrc, newSrc string) validation.Report {
	t.Helper()
	r, err := validation.Compare(defFrom(t, oldSrc), defFrom(t, newSrc))
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return r
}

// taskVerdict returns the verdict for one task id, failing when the report has none —
// an absent task is a different bug than an incompatible one and should not read as a
// nil-value pass.
func taskVerdict(t *testing.T, r validation.Report, id string) validation.TaskVerdict {
	t.Helper()
	for _, v := range r.Tasks {
		if v.Task == id {
			return v
		}
	}
	t.Fatalf("report has no verdict for task %q", id)
	return validation.TaskVerdict{}
}

func hasSlot(changed []string, want string) bool {
	for _, s := range changed {
		if s == want {
			return true
		}
	}
	return false
}

// pipeline is the fixture every pair below is built from: charge projects an output that
// ship can read, so a change to charge's shape shows up as a context difference AT ship —
// never at charge itself, whose own output is not yet in its entry context.
func pipeline(amountType, extra, processOutput string) string {
	return `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"order_id":{"type":"integer"}},"required":["order_id"]},
		"tasks": [
			{"id":"charge",
			 "action":{"type":"fetch","url":"http://x/charge",
			           "result_schema":{"type":"object","properties":{"amount":{"type":"` + amountType + `"}},"required":["amount"]}},
			 "output":{"amount":"$: self.result.amount"},
			 "switch":"next"},
			` + extra + `
			{"id":"ship","action":{"type":"fetch","url":"http://x/ship"},"switch":"end"}
		]` + processOutput + `
	}`
}

func TestCompare_IdenticalDefinitionsAreCompatible(t *testing.T) {
	r := compareDefs(t, pipeline("number", "", ""), pipeline("number", "", ""))
	if !r.Compatible || !r.OutputCompatible {
		t.Fatalf("a definition compared against itself must be compatible, got %+v", r)
	}
	if len(r.Changed) != 0 || len(r.RemovedTasks) != 0 || len(r.AddedTasks) != 0 {
		t.Fatalf("nothing should differ, got changed=%v removed=%v added=%v", r.Changed, r.RemovedTasks, r.AddedTasks)
	}
	for _, v := range r.Tasks {
		if len(v.Changed) != 0 {
			t.Fatalf("task %q reports changed slots %v against itself", v.Task, v.Changed)
		}
	}
}

// The motivating change: a fixed URL strands every running instance today, and must not.
func TestCompare_ChangedURLIsCompatibleAndReportedAsASlot(t *testing.T) {
	newDef := strings.Replace(pipeline("number", "", ""), "http://x/ship", "http://y/ship", 1)
	r := compareDefs(t, pipeline("number", "", ""), newDef)
	if !r.Compatible {
		t.Fatalf("a changed URL changes no shape and must stay compatible: %+v", r)
	}
	if v := taskVerdict(t, r, "ship"); !hasSlot(v.Changed, "action.url") {
		t.Fatalf("the changed URL must be reported as action.url, got %v", v.Changed)
	}
}

func TestCompare_OptionalInputPropertyAddedIsCompatible(t *testing.T) {
	withOptional := strings.Replace(pipeline("number", "", ""),
		`"properties":{"order_id":{"type":"integer"}},"required":["order_id"]`,
		`"properties":{"order_id":{"type":"integer"},"note":{"type":"string"}},"required":["order_id"]`, 1)
	r := compareDefs(t, pipeline("number", "", ""), withOptional)
	if !r.Compatible {
		t.Fatalf("an optional input property is exactly the change this feature exists for: %+v", r.Input)
	}
	if !hasSlot(r.Changed, "input_schema") {
		t.Fatalf("input_schema must be reported as a definition-level slot, got %v", r.Changed)
	}
}

func TestCompare_RequiredInputPropertyAddedIsRefusedAndNamed(t *testing.T) {
	withRequired := strings.Replace(pipeline("number", "", ""),
		`"properties":{"order_id":{"type":"integer"}},"required":["order_id"]`,
		`"properties":{"order_id":{"type":"integer"},"currency":{"type":"string"}},"required":["order_id","currency"]`, 1)
	r := compareDefs(t, pipeline("number", "", ""), withRequired)
	if r.Compatible || r.Input.Compatible {
		t.Fatalf("an input property the old instances never had must be refused: %+v", r.Input)
	}
	if want := "input.currency: newly required"; r.Input.Reason != want {
		t.Fatalf("the reason must name the property and what changed;\n got:  %q\n want: %q", r.Input.Reason, want)
	}
}

// The input verdict is hoisted precisely so a single break is not reported once per task.
func TestCompare_InputBreakIsReportedOnceNotPerTask(t *testing.T) {
	withRequired := strings.Replace(pipeline("number", "", ""),
		`"required":["order_id"]`, `"required":["order_id","currency"]`, 1)
	withRequired = strings.Replace(withRequired,
		`"order_id":{"type":"integer"}`, `"order_id":{"type":"integer"},"currency":{"type":"string"}`, 1)
	r := compareDefs(t, pipeline("number", "", ""), withRequired)
	for _, v := range r.Tasks {
		if !v.Compatible {
			t.Fatalf("task %q reports the input break too (%q); input is compared once, at definition level", v.Task, v.Reason)
		}
	}
}

func TestCompare_ChangedOutputTypeIsRefusedAtTheTaskThatReadsIt(t *testing.T) {
	r := compareDefs(t, pipeline("number", "", ""), pipeline("string", "", ""))
	if r.Compatible {
		t.Fatal("an output whose type changed under a running instance must be refused")
	}
	if v := taskVerdict(t, r, "charge"); !v.Compatible {
		t.Fatalf("charge's own output is not in its entry context, so charge must stay compatible: %q", v.Reason)
	}
	v := taskVerdict(t, r, "ship")
	if want := "outputs.charge.amount: number → string"; v.Reason != want {
		t.Fatalf("the reason must name the path and both types;\n got:  %q\n want: %q", v.Reason, want)
	}
}

func TestCompare_RemovedTaskIsRefusedBecauseAnInstanceThereHasNowhereToGo(t *testing.T) {
	withoutShip := `{"name":"p","tasks":[{"id":"charge","action":{"type":"fetch","url":"http://x/charge"},"switch":"end"}]}`
	withShip := `{"name":"p","tasks":[
		{"id":"charge","action":{"type":"fetch","url":"http://x/charge"},"switch":"next"},
		{"id":"ship","action":{"type":"fetch","url":"http://x/ship"},"switch":"end"}]}`
	r := compareDefs(t, withShip, withoutShip)
	if r.Compatible {
		t.Fatal("removing a task an instance can be sitting on must be refused")
	}
	if len(r.RemovedTasks) != 1 || r.RemovedTasks[0] != "ship" {
		t.Fatalf("the removed task must be listed separately, got %v", r.RemovedTasks)
	}
}

// A rename has no handle to match on, so it reads as one removal plus one addition. Tasks
// match by ID because it is the only handle there is; nothing could validate a claim that
// two differently-named tasks are the same one.
func TestCompare_RenamedTaskReadsAsRemovalPlusAddition(t *testing.T) {
	before := `{"name":"p","tasks":[{"id":"ship","action":{"type":"fetch","url":"http://x"},"switch":"end"}]}`
	after := `{"name":"p","tasks":[{"id":"dispatch","action":{"type":"fetch","url":"http://x"},"switch":"end"}]}`
	r := compareDefs(t, before, after)
	if r.Compatible {
		t.Fatal("a rename must be refused: nothing validates that the two ids are the same task")
	}
	if len(r.RemovedTasks) != 1 || r.RemovedTasks[0] != "ship" || len(r.AddedTasks) != 1 || r.AddedTasks[0] != "dispatch" {
		t.Fatalf("removed=%v added=%v; a rename is one of each", r.RemovedTasks, r.AddedTasks)
	}
}

// The comparison is the conservative FLOOR: it never sees an instance, so a main-line
// task carrying an output makes that output required at every later task and every old
// context lacking it is reported different — even where nothing reads it. Pinned as
// expected, not as a bug: demand-pruning is filed as a gate refinement (§10), and every
// later correction may only turn "different" into "tolerable".
func TestCompare_InsertedTaskWithAnOutputIsTheDocumentedFalseAlarm(t *testing.T) {
	audit := `{"id":"audit","action":{"type":"fetch","url":"http://x/audit"},
	           "output":{"seen":"$: true"},"switch":"next"},`
	r := compareDefs(t, pipeline("number", "", ""), pipeline("number", audit, ""))
	if r.Compatible {
		t.Fatal("the floor must report the inserted output as a difference; the gate is what refines it")
	}
	v := taskVerdict(t, r, "ship")
	if want := "outputs.audit: newly required"; v.Reason != want {
		t.Fatalf("got %q, want %q", v.Reason, want)
	}
}

// §3b runs the other way round and is never folded into the continuation verdict.
func TestCompare_OutputContractIsCheckedInReverse(t *testing.T) {
	const out = `, "output": {"total": "$: outputs.charge.amount"}`
	r := compareDefs(t, pipeline("number", "", out), pipeline("string", "", out))
	if r.OutputCompatible {
		t.Fatal("a consumer written against a number output must not silently start receiving a string")
	}
	if !strings.Contains(r.OutputReason, "number") || !strings.Contains(r.OutputReason, "string") {
		t.Fatalf("the reason must name both shapes, got %q", r.OutputReason)
	}
}

func TestCompare_UnchangedOutputContractHolds(t *testing.T) {
	const out = `, "output": {"total": "$: outputs.charge.amount"}`
	r := compareDefs(t, pipeline("number", "", out), pipeline("number", "", out))
	if !r.OutputCompatible {
		t.Fatalf("an unchanged output must satisfy its own contract: %q", r.OutputReason)
	}
}

// §7.7, accepted: isSubset never inspects `secret`, so a property that stops being
// secret compares equal and becomes visible over the API for data stored long before the
// upgrade. The definition-level changed slot is the ONLY thing that reports it, which is
// what this test holds in place.
func TestCompare_DroppedSecretIsCompatibleAndOnlyTheSlotReportsIt(t *testing.T) {
	secret := `{"name":"p",
		"config_schema":{"type":"object","properties":{"token":{"type":"string","secret":true}},"required":["token"]},
		"tasks":[{"id":"go","action":{"type":"fetch","url":"http://x/${ config.token }"},"switch":"end"}]}`
	plain := strings.Replace(secret, `,"secret":true`, "", 1)
	r := compareDefs(t, secret, plain)
	if !r.Compatible {
		t.Fatalf("redaction is a display concern and no verdict sees it; this must stay compatible: %+v", r)
	}
	if !hasSlot(r.Changed, "config_schema") {
		t.Fatalf("config_schema is the only report a dropped secret gets, and it is missing: %v", r.Changed)
	}
}

// §7.8, accepted: the new definition is the author's stated policy, so a flipped
// only_once applies and is reported as a slot rather than refused.
func TestCompare_FlippedOnlyOnceIsCompatibleAndReportedAsASlot(t *testing.T) {
	once := `{"name":"p","tasks":[{"id":"charge","only_once":true,
		"action":{"type":"fetch","url":"http://x"},"switch":"end"}]}`
	notOnce := strings.Replace(once, `"only_once":true,`, "", 1)
	r := compareDefs(t, once, notOnce)
	if !r.Compatible {
		t.Fatal("only_once is a policy, not a shape; flipping it must not refuse the comparison")
	}
	if v := taskVerdict(t, r, "charge"); !hasSlot(v.Changed, "only_once") {
		t.Fatalf("the flip must be reported so an operator reads the entry before moving a crashed worker's instance, got %v", v.Changed)
	}
}

// ── CompareSet ────────────────────────────────────────────────────────────────

func compareSetDefs(t *testing.T, old, new map[string]string) validation.SetReport {
	t.Helper()
	conv := func(m map[string]string) map[string]*model.ProcessDefinition {
		out := map[string]*model.ProcessDefinition{}
		for name, src := range m {
			out[name] = defFrom(t, src)
		}
		return out
	}
	r, err := validation.CompareSet(conv(old), conv(new))
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	// A fixture that stops analysing silently drops every verdict that would have used
	// it, and the report still comes back well-formed — so an unexpected entry here is
	// a broken fixture wearing the costume of a passing test.
	if len(r.Unanalysable) != 0 {
		t.Fatalf("fixture failed to analyse: %+v", r.Unanalysable)
	}
	return r
}

// parentDef declares a result_schema naming exactly one of the child's output fields.
const compatParentDef = `{"name":"parent","tasks":[
	{"id":"work","action":{"type":"child","name":"child",
	 "result_schema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
	 "switch":"end"}]}`

// compatChildDef outputs {id} plus, when withNote, a second field the strict parent in
// TestCompareSet_ChildDroppingAFieldTheParentReadsIsRefused names and the lenient one
// does not. Dropping it is the whole experiment.
func compatChildDef(withNote bool) string {
	prop, taskOut, procOut := "", "", ""
	if withNote {
		prop = `,"note":{"type":"string"}`
		taskOut = `,"note":"$: self.result.note"`
		procOut = `,"note":"$: outputs.run.note"`
	}
	return `{"name":"child","tasks":[
		{"id":"run","action":{"type":"fetch","url":"http://x",
		 "result_schema":{"type":"object","properties":{"id":{"type":"string"}` + prop + `},"required":["id"` + noteRequired(withNote) + `]}},
		 "output":{"id":"$: self.result.id"` + taskOut + `},
		 "switch":"end"}],
		"output":{"id":"$: outputs.run.id"` + procOut + `}}`
}

func noteRequired(withNote bool) string {
	if withNote {
		return `,"note"`
	}
	return ""
}

// The pairing check earns its place by being TIGHTER than the general output contract: a
// child that drops a field breaks §3b, but a parent whose result_schema never named that
// field is unaffected — and only a check holding both documents in one frame can say so.
func TestCompareSet_PairingIsTighterThanTheOutputContract(t *testing.T) {
	old := map[string]string{"parent": compatParentDef, "child": compatChildDef(true)}
	new := map[string]string{"parent": compatParentDef, "child": compatChildDef(false)}
	r := compareSetDefs(t, old, new)

	for _, c := range r.Children {
		if !c.Compatible {
			t.Fatalf("the parent's result_schema never named the dropped field, so this pair must hold: %+v", c)
		}
	}
	if len(r.Children) != 2 {
		t.Fatalf("both mixed rows must be reported (old parent + new child, new parent + old child), got %d: %+v", len(r.Children), r.Children)
	}
	sides := map[validation.Side]bool{}
	for _, c := range r.Children {
		sides[c.ParentSide] = true
	}
	if !sides[validation.SideFrom] || !sides[validation.SideTo] {
		t.Fatalf("the two rows must name different parent sides, got %+v", r.Children)
	}
}

func TestCompareSet_ChildDroppingAFieldTheParentReadsIsRefused(t *testing.T) {
	strictParent := strings.Replace(compatParentDef,
		`"properties":{"id":{"type":"string"}},"required":["id"]`,
		`"properties":{"id":{"type":"string"},"note":{"type":"string"}},"required":["id","note"]`, 1)
	old := map[string]string{"parent": strictParent, "child": compatChildDef(true)}
	new := map[string]string{"parent": strictParent, "child": compatChildDef(false)}
	r := compareSetDefs(t, old, new)
	if r.Compatible {
		t.Fatal("a child that stops producing a field its waiting parent reads must be refused")
	}
	var refused []validation.ChildPairVerdict
	for _, c := range r.Children {
		if !c.Compatible {
			refused = append(refused, c)
		}
	}
	// Only the row where the child moved: the old child still produces the field, so
	// pairing it with either parent holds.
	if len(refused) != 1 || refused[0].ParentSide != validation.SideFrom {
		t.Fatalf("exactly the child-moves-only row must fail, got %+v", refused)
	}
}

func TestCompareSet_UnpairedNameIsReportedNeverDropped(t *testing.T) {
	r := compareSetDefs(t,
		map[string]string{"parent": compatParentDef, "child": compatChildDef(false)},
		map[string]string{"parent": compatParentDef})
	if r.Compatible {
		t.Fatal("a name compared against nothing must not roll up as compatible")
	}
	if len(r.Unpaired) != 1 || r.Unpaired[0].Name != "child" || r.Unpaired[0].Side != validation.SideFrom {
		t.Fatalf("the missing side must be named, got %+v", r.Unpaired)
	}
}

// Old rows were validated under the rules of their day, so one may no longer analyse.
// That is a per-version verdict, and it must never roll up as "checked, and fine".
func TestCompareSet_UnanalysableVersionMakesTheRollupFalse(t *testing.T) {
	broken := `{"name":"p","tasks":[{"id":"go","action":{"type":"fetch","url":"http://x/${ outputs.nope.v }"},"switch":"end"}]}`
	ok := `{"name":"p","tasks":[{"id":"go","action":{"type":"fetch","url":"http://x"},"switch":"end"}]}`
	// Called directly: compareSetDefs rejects an unanalysable entry, which is what this
	// test is about.
	r, err := validation.CompareSet(
		map[string]*model.ProcessDefinition{"p": defFrom(t, broken)},
		map[string]*model.ProcessDefinition{"p": defFrom(t, ok)})
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	if r.Compatible {
		t.Fatal("a version compared against nothing must not roll up as compatible")
	}
	if len(r.Unanalysable) != 1 || r.Unanalysable[0].Side != validation.SideFrom {
		t.Fatalf("the unanalysable side must be named, got %+v", r.Unanalysable)
	}
	if len(r.Processes) != 0 {
		t.Fatalf("a pair with an unanalysable side has no verdict to report, got %+v", r.Processes)
	}
}

// ── TaskContexts ──────────────────────────────────────────────────────────────

func TestTaskContexts_StripsConfigBecauseItIsNotInstanceState(t *testing.T) {
	def := defFrom(t, `{"name":"p",
		"config_schema":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]},
		"tasks":[{"id":"go","action":{"type":"fetch","url":"${ config.url }"},"switch":"end"}]}`)
	ctxs, err := validation.TaskContexts(def)
	if err != nil {
		t.Fatalf("TaskContexts: %v", err)
	}
	if _, ok := ctxs["go"].Properties()["config"]; ok {
		t.Fatal("config is re-resolved from the environment every tick; nothing in the row corresponds to it")
	}
}
