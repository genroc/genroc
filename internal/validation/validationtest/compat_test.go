package validationtest

import (
	"encoding/json"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// What is left here is what is NOT a case: the resolution semantics CompareSet owns, which have
// no rendered form. Every comparison CASE lives in tests/cli/testdata/compat/*.yaml, asserted
// as the report an operator reads — because that report is the deliverable.

func defFrom(t *testing.T, src string) *model.ProcessDefinition {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(src), &def); err != nil {
		t.Fatalf("unmarshal definition: %v\n%s", err, src)
	}
	return &def
}

// compatParentDef declares a result_schema naming one of the child's output fields.
const compatParentDef = `{"name":"parent","tasks":[
	{"id":"work","action":{"type":"child","name":"child",
	 "result_schema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
	 "switch":"end"}]}`

// compatChildDef is a minimal child: one task, one projected output.
func compatChildDef() string {
	return `{"name":"child","tasks":[
		{"id":"run","action":{"type":"fetch","url":"http://x",
		 "result_schema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
		 "output":{"id":"$: self.result.id"},
		 "switch":"end"}],
		"output":{"id":"$: outputs.run.id"}}`
}

func TestCompareSet_NameOnOneSideOnlyIsReportedNeverDropped(t *testing.T) {
	parent := validation.SideEntry{Def: defFrom(t, compatParentDef), Version: 1}
	r, err := validation.CompareSet(
		map[string]validation.SideEntry{
			"parent": parent,
			"child":  {Def: defFrom(t, compatChildDef()), Version: 1},
		},
		map[string]validation.SideEntry{"parent": parent})
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	// The caller reconciles before calling — a child the target side does not name is
	// normally carried over at its current version. Reaching here uncarried, it still gets
	// a row saying there was nothing to compare it against, rather than vanishing.
	var child *validation.Report
	for i := range r.Processes {
		if r.Processes[i].Name == "child" {
			child = &r.Processes[i]
		}
	}
	if child == nil {
		t.Fatal("the child must still be reported; a silent disappearance is what a report must never do")
	}
	if child.Status != validation.StatusNothingToCompare {
		t.Fatalf("got status %q, want %q", child.Status, validation.StatusNothingToCompare)
	}
	// It cannot break anything, so it must not drag the roll-up down: otherwise almost
	// every real comparison reports false, since a deployed channel always carries
	// processes a bundle does not.
	if !r.Compatible {
		t.Fatal("a process with nothing to compare must not make the whole report incompatible")
	}
}

func TestCompareSet_SameVersionOnBothSidesHasNothingToCompare(t *testing.T) {
	def := validation.SideEntry{Def: defFrom(t, compatChildDef()), Version: 4}
	r, err := validation.CompareSet(
		map[string]validation.SideEntry{"child": def},
		map[string]validation.SideEntry{"child": def})
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	if r.Processes[0].Status != validation.StatusNothingToCompare {
		t.Fatalf("comparing a version with itself is a tautology, got status %q", r.Processes[0].Status)
	}
}

func TestCompareSet_ProcessOnlyOnTheTargetSideIsNew(t *testing.T) {
	r, err := validation.CompareSet(
		map[string]validation.SideEntry{},
		map[string]validation.SideEntry{"child": {Def: defFrom(t, compatChildDef()), Version: 1}})
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	if r.Processes[0].Status != validation.StatusNew {
		t.Fatalf("no previous version exists, so nothing is being upgraded; got status %q", r.Processes[0].Status)
	}
	if !r.Compatible {
		t.Fatal("a process nothing is running cannot break anything")
	}
}

func TestCompareSet_UnanalysableVersionMakesTheRollupFalse(t *testing.T) {
	broken := `{"name":"p","tasks":[{"id":"go","action":{"type":"fetch","url":"http://x/${ outputs.nope.v }"},"switch":"end"}]}`
	ok := `{"name":"p","tasks":[{"id":"go","action":{"type":"fetch","url":"http://x"},"switch":"end"}]}`
	// Called directly: compareSetDefs rejects an unanalysable entry, which is what this
	// test is about.
	r, err := validation.CompareSet(
		map[string]validation.SideEntry{"p": {Def: defFrom(t, broken), Version: 1}},
		map[string]validation.SideEntry{"p": {Def: defFrom(t, ok), Version: 2}})
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	if r.Compatible {
		t.Fatal("a version compared against nothing must not roll up as compatible")
	}
	// It is a row like any other, not a separate list: every process has one place to
	// look, and the side names which of the two versions failed.
	if len(r.Processes) != 1 {
		t.Fatalf("expected exactly one row, got %+v", r.Processes)
	}
	row := r.Processes[0]
	if row.Status != validation.StatusUnanalysable || row.Side != validation.SideFrom {
		t.Fatalf("got status %q side %q, want unanalysable on the from side", row.Status, row.Side)
	}
	if row.Reason == "" {
		t.Fatal("an unanalysable row must carry why it could not be analysed")
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
