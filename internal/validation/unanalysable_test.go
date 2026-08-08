package validation

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/model"
)

// The one row the CLI fixtures cannot produce. A stored version that fails its own
// inference has to have been registered before the rule that now rejects it, and nothing a
// test can apply is in that state — so the `from` side of `unanalysable`, and §5's rule
// that it cannot be excused, live here.

func definitionFromJSON(t *testing.T, src string) *model.ProcessDefinition {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(src), &def); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &def
}

// legacyDefinition is a document that passed under the rules of its day and no longer
// analyses: it reads an output no task produces. Deliberately not run through Validate,
// which is what would refuse it today.
func legacyDefinition(t *testing.T, name string) *model.ProcessDefinition {
	return definitionFromJSON(t, `{"name":"`+name+`","tasks":[
		{"id":"go","action":{"type":"fetch","url":"http://x/go"},
		 "output":{"v":"$: outputs.gone.v"},"switch":"end"}]}`)
}

func analysableDefinition(t *testing.T, name string) *model.ProcessDefinition {
	return definitionFromJSON(t, `{"name":"`+name+`","tasks":[
		{"id":"go","action":{"type":"fetch","url":"http://x/go"},"switch":"end"}]}`)
}

func TestCompareSet_AnUnanalysableFromSideIsNamedAndCannotBeExcused(t *testing.T) {
	report, err := CompareSet(
		map[string]SideEntry{"legacy": {Def: legacyDefinition(t, "legacy"), Version: 1}},
		map[string]SideEntry{"legacy": {Def: analysableDefinition(t, "legacy"), Version: 2}},
	)
	if err != nil {
		t.Fatalf("CompareSet returned an error: %v — one version failing to analyse is a row, "+
			"not a failure of the whole report", err)
	}
	if len(report.Processes) != 1 {
		t.Fatalf("got %d rows, want 1: every name gets exactly one row whatever became of it",
			len(report.Processes))
	}

	row := report.Processes[0]
	if row.Status != StatusUnanalysable {
		t.Errorf("status %q, want %q", row.Status, StatusUnanalysable)
	}
	// The versions are on the row, so the side is what says WHICH of the two failed.
	if row.Side != SideFrom {
		t.Errorf("side %q, want %q — the old version is the one that does not analyse", row.Side, SideFrom)
	}
	if !strings.Contains(row.Reason, "gone") {
		t.Errorf("reason %q does not name what failed; an unanalysable row an operator "+
			"cannot act on is the answer the status exists to avoid", row.Reason)
	}
	if row.FromVersion != 1 || row.ToVersion != 2 {
		t.Errorf("versions %d → %d, want 1 → 2", row.FromVersion, row.ToVersion)
	}
	if report.Compatible {
		t.Error("roll-up is true: the pair was compared against nothing, and an answer " +
			"indistinguishable from \"checked, and fine\" is worse than no report")
	}

	if err := report.ApplySelection([]string{"contract"}); err != nil {
		t.Fatalf("ApplySelection: %v", err)
	}
	if report.Passes {
		t.Error("excused by --ignore contract: an unanalysable row is the ABSENCE of a verdict, " +
			"so no selection can excuse it (specs/compat-command.md §5)")
	}
}

// The other side of the same switch: a version that fails to analyse must drag down only
// the pair it belongs to. A registry accumulates definitions validated under the rules of
// their day, so a report about two OTHER versions cannot inherit their failure.
func TestCompareSet_AnUnanalysableVersionNobodyAskedAboutIsNotAnalysed(t *testing.T) {
	report, err := CompareSet(
		map[string]SideEntry{
			"legacy": {Def: legacyDefinition(t, "legacy"), Version: 3},
			"moving": {Def: analysableDefinition(t, "moving"), Version: 1},
		},
		map[string]SideEntry{
			// Same version on both sides: nothing to compare, so it is never analysed.
			"legacy": {Def: legacyDefinition(t, "legacy"), Version: 3},
			"moving": {Def: analysableDefinition(t, "moving"), Version: 2},
		},
	)
	if err != nil {
		t.Fatalf("CompareSet: %v", err)
	}
	if !report.Compatible {
		t.Error("roll-up is false: the only version that fails to analyse is one both sides " +
			"agree on, which is never compared and must not be analysed either")
	}
	for _, row := range report.Processes {
		if row.Name == "legacy" && row.Status != StatusNothingToCompare {
			t.Errorf("legacy is %q, want %q — status comes from the versions alone, before "+
				"anything is analysed", row.Status, StatusNothingToCompare)
		}
	}
}
