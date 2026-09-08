package lsp

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/defdoc"
	"genroc/internal/model"
	"genroc/internal/numeric"
	"genroc/internal/validation"
)

func only(t *testing.T, text string) diagnostic {
	t.Helper()
	ds := analyse(text)
	if len(ds) != 1 {
		t.Fatalf("want one diagnostic, got %d: %+v", len(ds), ds)
	}
	return ds[0]
}

func TestAValidDefinitionHasNoDiagnostics(t *testing.T) {
	if ds := analyse(valid); len(ds) != 0 {
		t.Fatalf("a definition genroc accepts must underline nothing, got %+v", ds)
	}
}

// The typo that started this: the published JSON Schema accepted it, the server refused it,
// and the editor showed nothing. specs/language-server.md §5.
func TestAnUnknownKeyUnderlinesTheKeyItself(t *testing.T) {
	//  1 name: demo
	//  2 tasks:
	//  3   - id: a
	//  4     on_eror: []
	//  5     switch: end
	d := only(t, "name: demo\ntasks:\n  - id: a\n    on_eror: []\n    switch: end\n")
	if d.Code != "def.unknown_key" {
		t.Errorf("code = %q", d.Code)
	}
	if !strings.Contains(d.Message, "on_eror") {
		t.Errorf("the message must name the key that has no home: %q", d.Message)
	}
	if d.Range.Start.Line != 3 {
		t.Fatalf("`on_eror` is on line 4 (0-based 3), got %d", d.Range.Start.Line)
	}
	// The key, not its value: the spelling is what is wrong.
	if d.Range.Start.Character != 4 || d.Range.End.Character != 11 {
		t.Errorf("want the key's own columns 4-11, got %d-%d",
			d.Range.Start.Character, d.Range.End.Character)
	}
}

func TestAMissingRequiredFieldPointsAtTheNodeThatLacksIt(t *testing.T) {
	//  1 name: demo
	//  2 tasks:
	//  3   - switch: end
	d := only(t, "name: demo\ntasks:\n  - switch: end\n")
	if !strings.Contains(d.Message, "id is required") {
		t.Fatalf("message = %q", d.Message)
	}
	if d.Range.Start.Line != 2 {
		t.Errorf("the task that lacks an id is on line 3 (0-based 2), got %d", d.Range.Start.Line)
	}
}

func TestASyntaxErrorIsReportedOnItsOwnLine(t *testing.T) {
	d := only(t, "name: demo\ntasks:\n  - id: a\n   bad indent here\n")
	if d.Code != "def.syntax" {
		t.Errorf("code = %q", d.Code)
	}
	if d.Range.Start.Line == 0 {
		t.Errorf("a parse failure on line 4 must not be reported at the top of the file")
	}
}

func TestEveryBrokenSlotIsUnderlinedNotJustTheFirst(t *testing.T) {
	ds := analyse("name: demo\ntasks:\n" +
		"  - id: a\n    action:\n      type: fetch\n      url: \"$: nope.x\"\n    switch: next\n" +
		"  - id: b\n    action:\n      type: fetch\n      url: \"$: alsonope.y\"\n    switch: end\n")
	if len(ds) != 2 {
		t.Fatalf("two broken slots, got %d: %+v", len(ds), ds)
	}
	if ds[0].Range.Start.Line == ds[1].Range.Start.Line {
		t.Errorf("both diagnostics landed on line %d", ds[0].Range.Start.Line)
	}
}

// A `.genroc.yaml` may hold several definitions; each is indexed on its own, so a position in
// the second is not read out of the first's index.
func TestEachDocumentInAMultiDocumentFileIsAnalysed(t *testing.T) {
	ds := analyse(valid + "---\n" + "name: other\ntasks:\n  - id: z\n    action:\n      type: fetch\n      url: \"$: nope.x\"\n    switch: end\n")
	if len(ds) != 1 {
		t.Fatalf("the second document is broken and the first is not, got %d: %+v", len(ds), ds)
	}
	if ds[0].Range.Start.Line < 7 {
		t.Errorf("the diagnostic belongs to the second document, below line 8; got line %d",
			ds[0].Range.Start.Line+1)
	}
}

// The protocol counts UTF-16 code units, so a byte column past a multi-byte character would
// underline the wrong span in every editor that honours the spec.
func TestColumnsAreCountedInUTF16CodeUnits(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
		col  int // 1-based byte column
		want int // 0-based UTF-16 offset
	}{
		{"ascii", `  url: "x"`, 3, 2},
		{"two-byte runes before the column", `  "é": 1`, 7, 5},
		{"astral plane counts as a surrogate pair", `  "🙂": 1`, 9, 6},
		{"past the end of the line clamps", "abc", 99, 3},
		{"before the start clamps", "abc", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := utf16Column(c.line, c.col); got != c.want {
				t.Errorf("utf16Column(%q, %d) = %d, want %d", c.line, c.col, got, c.want)
			}
		})
	}
}

// The claim §5 rests on: what the editor underlines is what an apply would refuse. A
// definition the server accepts must produce nothing, and one it refuses must produce
// something — checked against the server's own two calls rather than against a schema.
func TestTheEditorAgreesWithTheServerOnWhatIsRejected(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"valid", valid},
		{"unknown key on a task", "name: demo\ntasks:\n  - id: a\n    on_eror: []\n    switch: end\n"},
		{"unknown key at the root", "name: demo\ntsaks: []\ntasks:\n  - id: a\n    switch: end\n"},
		{"a goto naming no task", "name: demo\ntasks:\n  - id: a\n    switch: \"$nope\"\n"},
		{"an expression that does not type", brokenURL},
		{"a raise with a misspelled key", "name: demo\ntasks:\n  - id: a\n    switch:\n      - raise:\n          code: c\n          mesage: m\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if serverRefuses(t, c.text) != (len(analyse(c.text)) > 0) {
				t.Errorf("the editor and the server disagree: server refuses = %v, "+
					"diagnostics = %+v", serverRefuses(t, c.text), analyse(c.text))
			}
		})
	}
}

// serverRefuses is exactly what putDefinition does before it saves anything.
func serverRefuses(t *testing.T, text string) bool {
	t.Helper()
	var def model.ProcessDefinition
	raw, err := yamlToJSON(text)
	if err != nil {
		return true
	}
	if err := numeric.DecodeStrict(raw, &def); err != nil {
		return true
	}
	if err := def.Validate(); err != nil {
		return true
	}
	_, err = validation.Generate(&def)
	return err != nil
}

func yamlToJSON(text string) ([]byte, error) {
	docs, err := defdoc.ParseAll([]byte(text))
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return []byte("null"), nil
	}
	return json.Marshal(docs[0].Value)
}

// A diagnostic underlines the FIELD, not the block it sits in: the address is the scope, the
// location is the line. specs/language-server.md §7b.
func TestADiagnosticUnderlinesTheFieldNotTheWholeSlot(t *testing.T) {
	//	 1 name: demo
	//	 2 tasks:
	//	 3   - id: a
	//	 4     action:
	//	 5       type: fetch
	//	 6       url: "$: nope.x"
	//	 7     switch: end
	d := only(t, "name: demo\ntasks:\n  - id: a\n    action:\n      type: fetch\n      url: \"$: nope.x\"\n    switch: end\n")
	if d.Range.Start.Line != 5 {
		t.Fatalf("the url is on line 6 (0-based 5); the action block starts on line 5, and "+
			"underlining that is what this test exists to prevent. got line %d", d.Range.Start.Line)
	}
	if d.Range.Start.Character != 11 {
		t.Errorf("want the value's own columns, got %d-%d", d.Range.Start.Character, d.Range.End.Character)
	}
}

// A check that knows no field still underlines its slot, which is where the reader looks anyway.
func TestASlotWithNoFinerFieldStillUnderlinesTheSlot(t *testing.T) {
	//	 5     output:
	//	 6       v: "$: nope.x"
	d := only(t, "name: demo\ntasks:\n  - id: a\n    switch: end\n    output:\n      v: \"$: nope.x\"\n")
	if d.Range.Start.Line != 5 {
		t.Errorf("the output map is on line 6 (0-based 5), got %d", d.Range.Start.Line)
	}
}
