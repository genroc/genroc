package validationtest

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/validation"
)

// Inference used to return the first failure with its location in prose, so a client could not
// say which field was wrong and an author fixed one thing per round trip.
// specs/language-server.md §2.

func check(t *testing.T, defJSON string) validation.Diagnostics {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	_, ds := validation.Check(&def)
	return ds
}

func addresses(ds validation.Diagnostics) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Address
	}
	return out
}

func assertAddresses(t *testing.T, ds validation.Diagnostics, want ...string) {
	t.Helper()
	got := addresses(ds)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("addresses = %v, want %v\n  diagnostics: %v", got, want, ds)
	}
}

const twoBrokenActions = `{"name":"p","tasks":[
	{"id":"a","switch":"next","action":{"type":"fetch","url":"$: nope.x"}},
	{"id":"b","switch":"end","action":{"type":"fetch","url":"$: alsonope.y"}}]}`

func TestEveryBrokenSlotIsReported(t *testing.T) {
	assertAddresses(t, check(t, twoBrokenActions), "tasks.a.action", "tasks.b.action")
}

// One task can be wrong in three places, and they are three slots rather than three tries.
func TestTheSlotsOfOneTaskAreReportedSeparately(t *testing.T) {
	ds := check(t, `{"name":"p","tasks":[
		{"id":"a","action":{"type":"fetch","url":"$: nope.x"},
		 "switch":[{"case":"$: alsonope.y","goto":"end"},{"goto":"end"}],
		 "on_error":[{"case":"$: thirdnope.z","goto":"end"}]}]}`)
	assertAddresses(t, ds, "tasks.a.action", "tasks.a.on_error.0", "tasks.a.switch")
}

// The rule index is in the address, so two broken rules are two findings — an `items` schema
// could not carry a different context per rule, which is why ruleSlot indexes by hand.
func TestTwoBrokenRulesInOneTaskAreTwoDiagnostics(t *testing.T) {
	ds := check(t, `{"name":"p","tasks":[
		{"id":"a","action":{"type":"fetch","url":"u"},"switch":"end",
		 "on_error":[{"case":"$: nope.x","goto":"end"},{"case":"$: alsonope.y","goto":"end"}]}]}`)
	assertAddresses(t, ds, "tasks.a.on_error.0", "tasks.a.on_error.1")
}

const brokenOutputThenBrokenAction = `{"name":"p","tasks":[
	{"id":"a","switch":"next","action":{"type":"fetch","url":"u"},"output":{"v":"$: self.result.missing"}},
	{"id":"b","switch":"end","action":{"type":"fetch","url":"$: nope.x"}}]}`

// Inference is sequential, so a failed output slot used to cost every diagnostic below it.
// It recovers as {} instead, which is what makes the second finding reachable.
func TestAFailedOutputDoesNotHideALaterTasksOwnError(t *testing.T) {
	assertAddresses(t, check(t, brokenOutputThenBrokenAction), "tasks.a.output", "tasks.b.action")
}

// The other half of recovery: the {} it leaves behind must not be reported as a second
// problem. A task reading a poisoned output is a consequence, not a finding.
func TestReadingAPoisonedOutputIsNotItsOwnDiagnostic(t *testing.T) {
	ds := check(t, `{"name":"p","tasks":[
		{"id":"a","switch":"next","action":{"type":"fetch","url":"u",
		 "responses":{"200":{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}}},
		 "output":{"v":"$: self.result.missing"}},
		{"id":"b","switch":"end","action":{"type":"fetch","url":"${ outputs.a.v }"}}]}`)
	assertAddresses(t, ds, "tasks.a.output")
}

// Suppression is scoped to tasks that can SEE a poisoned output. A blanket "any poison hides
// every unknown read" would hide real errors in tasks that never read the broken one.
func TestAnUnknownReadInATaskThatSeesNoPoisonSurvives(t *testing.T) {
	// `a` exports its opaque result deliberately, so `outputs.a.v` IS {} and reading through
	// it is the refusal {} exists for. Nothing here is poisoned.
	ds := check(t, `{"name":"p","tasks":[
		{"id":"a","switch":"next","action":{"type":"fetch","url":"u","responses":{"200":{}}},
		 "output":{"v":"$: self.result"}},
		{"id":"b","switch":"end","action":{"type":"fetch","url":"${ outputs.a.v.x }"}}]}`)
	if len(ds) != 1 || ds[0].Code != validation.CodeUnknownRead {
		t.Fatalf("reading through a {} the author wrote is the answer, not an artefact of "+
			"recovery; got %v", ds)
	}
}

// The case the visibility map exists for: `first` reads through a {} it was handed, and
// `later` — which it cannot see — is poisoned. A blanket rule would drop a real finding here.
func TestPoisonElsewhereDoesNotHideAnUnknownReadThatCannotSeeIt(t *testing.T) {
	ds := check(t, `{"name":"p",
		"input_schema":{"type":"object","properties":{"opaque":{}},"required":["opaque"]},
		"tasks":[
		{"id":"first","switch":"next","action":{"type":"fetch","url":"${ input.opaque.field }"}},
		{"id":"later","switch":"end","action":{"type":"fetch","url":"u"},
		 "output":{"v":"$: self.result.missing"}}]}`)
	assertAddresses(t, ds, "tasks.first.action", "tasks.later.output")
}

// The address is the machine location; the message keeps the prose it always had, which
// already names the task and the slot.
func TestADiagnosticCarriesACodeAndKeepsItsMessage(t *testing.T) {
	ds := check(t, twoBrokenActions)
	if ds[0].Code != validation.CodeExpression {
		t.Errorf("an expression that did not type-check is %q, got %q", validation.CodeExpression, ds[0].Code)
	}
	if !strings.Contains(ds[0].Error(), `task "a" url`) {
		t.Errorf("the message lost its prose: %q", ds[0].Error())
	}
	if strings.HasPrefix(ds[0].Error(), ds[0].Address) {
		t.Errorf("the address is structured data and must not be prefixed onto prose that "+
			"already names the slot: %q", ds[0].Error())
	}
}

// Generate is the gate — "is this registrable" — and must answer exactly as before.
func TestGenerateStillFailsOnTheFirstDiagnostic(t *testing.T) {
	err := runGenerateErr(t, twoBrokenActions)
	if err == nil {
		t.Fatal("a definition with two broken expressions must not be registrable")
	}
	if !strings.Contains(err.Error(), `field "nope" not found`) {
		t.Errorf("Generate must still report what is wrong: %v", err)
	}
}

func TestAValidDefinitionHasNoDiagnostics(t *testing.T) {
	ds := check(t, `{"name":"p","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","url":"u"}}]}`)
	if len(ds) != 0 {
		t.Fatalf("a valid definition produced %v", ds)
	}
}

// The test that keeps the two grammars one grammar: every address a diagnostic carries is a
// slot address of specs/schema-command.md §2 — `tasks.<id>.<phase>`, `tasks.<id>.on_error.<n>`,
// `output`, or empty for a whole-definition failure — and its task segment names a real task.
//
// It asserts the SPELLING rather than round-tripping through SlotContexts, because that walks
// through Generate and so answers nothing about a definition that does not infer. Which is
// most of the ones an editor sees; see specs/language-server.md §2.
func TestEveryDiagnosticAddressIsASlotAddress(t *testing.T) {
	phases := map[string]bool{"action": true, "output": true, "switch": true}
	for _, defJSON := range []string{twoBrokenActions, brokenOutputThenBrokenAction,
		`{"name":"p","tasks":[{"id":"a","action":{"type":"fetch","url":"$: nope.x"},
		  "switch":[{"case":"$: alsonope.y","goto":"end"},{"goto":"end"}],
		  "on_error":[{"case":"$: thirdnope.z","goto":"end"}]}]}`,
	} {
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ids := map[string]bool{}
		for _, task := range def.Tasks {
			ids[task.ID] = true
		}
		ds := check(t, defJSON)
		if len(ds) == 0 {
			t.Fatalf("fixture is not broken: %s", defJSON)
		}
		for _, d := range ds {
			if d.Address == "" || d.Address == "output" {
				continue
			}
			seg := strings.Split(d.Address, ".")
			if len(seg) < 3 || seg[0] != "tasks" {
				t.Errorf("address %q is not tasks.<id>.<phase>", d.Address)
				continue
			}
			if !ids[seg[1]] {
				t.Errorf("address %q names no task; the definition has %v", d.Address, ids)
			}
			switch {
			case phases[seg[2]] && len(seg) == 3:
			case seg[2] == "on_error" && len(seg) == 4:
				if _, err := strconv.Atoi(seg[3]); err != nil {
					t.Errorf("address %q must index the rule: %v", d.Address, err)
				}
			default:
				t.Errorf("address %q names no phase of the slot grammar", d.Address)
			}
		}
	}
}
