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
		"  - id: a\n    action:\n      type: fetch\n      method: post\n      url: \"$: nope.x\"\n    switch: next\n" +
		"  - id: b\n    action:\n      type: fetch\n      method: post\n      url: \"$: alsonope.y\"\n    switch: end\n")
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
	ds := analyse(valid + "---\n" + "name: other\ntasks:\n  - id: z\n    action:\n      type: fetch\n      method: post\n      url: \"$: nope.x\"\n    switch: end\n")
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
	//	 6       method: post
	//	 7       url: "$: nope.x"
	//	 8     switch: end
	d := only(t, "name: demo\ntasks:\n  - id: a\n    action:\n      type: fetch\n      method: post\n      url: \"$: nope.x\"\n    switch: end\n")
	if d.Range.Start.Line != 6 {
		t.Fatalf("the url is on line 7 (0-based 6); the action block starts on line 5, and "+
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

// A schema decodes before anything reads it, and encoding/json's own error named the outermost
// slot it was inside ("input_schema.properties") — so a mistake in one property underlined the
// whole schema, or, with nothing to resolve, the first line of the file.
func TestAPropertyThatIsNotASchemaUnderlinesThatProperty(t *testing.T) {
	//  1 name: demo
	//  2 input_schema:
	//  3   type: object
	//  4   properties:
	//  5     who: string
	d := only(t, "name: demo\ninput_schema:\n  type: object\n  properties:\n    who: string\ntasks:\n  - id: a\n    switch: end\n")
	if d.Message != "who: a schema must be an object, not a string" {
		t.Errorf("message = %q", d.Message)
	}
	if d.Range.Start.Line != 4 || d.Range.Start.Character != 9 || d.Range.End.Character != 15 {
		t.Errorf("want line 4 (0-based), columns 9-15 — the value written where a schema goes; got %d:%d-%d",
			d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Character)
	}
}

func TestAnUnsupportedKeywordUnderlinesTheKeyword(t *testing.T) {
	//  1 name: demo
	//  2 input_schema:
	//  3   type: object
	//  4   properties:
	//  5     who:
	//  6       pattern: "^a"
	d := only(t, "name: demo\ninput_schema:\n  type: object\n  properties:\n    who:\n      pattern: \"^a\"\ntasks:\n  - id: a\n    switch: end\n")
	if d.Code != "def.unknown_key" {
		t.Errorf("code = %q", d.Code)
	}
	// The key, not its value: the spelling is what is wrong.
	if d.Range.Start.Line != 5 || d.Range.Start.Character != 6 || d.Range.End.Character != 13 {
		t.Errorf("want line 5 (0-based), the keyword's own columns 6-13, got %d:%d-%d",
			d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Character)
	}
}

// The one failure a schema cannot place itself: the slot IS the mistake, so there is no path
// inside the schema to report and encoding/json adds context only to its own type errors.
func TestASchemaSlotThatIsNotAnObjectUnderlinesTheSlot(t *testing.T) {
	//  1 name: demo
	//  2 input_schema: object
	d := only(t, "name: demo\ninput_schema: object\ntasks:\n  - id: a\n    switch: end\n")
	if d.Message != "a schema must be an object, not a string" {
		t.Errorf("message = %q", d.Message)
	}
	if d.Range.Start.Line != 1 || d.Range.Start.Character != 14 || d.Range.End.Character != 20 {
		t.Errorf("want line 1 (0-based), columns 14-20, got %d:%d-%d",
			d.Range.Start.Line, d.Range.Start.Character, d.Range.End.Character)
	}
}

// Two schemas, one broken: the scan that places a slot-shaped failure must not settle on the
// first schema it meets.
func TestASchemaSlotIsPlacedAmongOtherSchemas(t *testing.T) {
	//  1 name: demo
	//  2 input_schema:
	//  3   type: object
	//  4 tasks:
	//  5   - id: a
	//  6     action:
	//  7       type: external
	//  8       result_schema: object
	d := only(t, "name: demo\ninput_schema:\n  type: object\ntasks:\n  - id: a\n    action:\n      type: external\n      result_schema: object\n    switch: end\n")
	if d.Range.Start.Line != 7 || d.Range.Start.Character != 21 {
		t.Errorf("want the task's result_schema on line 7 (0-based) at column 21, got %d:%d",
			d.Range.Start.Line, d.Range.Start.Character)
	}
}

// A response schema is declared as one arm of a nullable union, so a slot search that reads
// only the node it lands on does not recognise it.
func TestAResponseSchemaThatIsNotAnObjectUnderlinesThatResponse(t *testing.T) {
	//  1 name: demo
	//  2 tasks:
	//  3   - id: a
	//  4     action:
	//  5       type: fetch
	//  6       method: post
	//  7       url: "https://x"
	//  8       responses:
	//  9         "200": object
	d := only(t, "name: demo\ntasks:\n  - id: a\n    action:\n      type: fetch\n      method: post\n      url: \"https://x\"\n      responses:\n        \"200\": object\n    switch: end\n")
	if d.Range.Start.Line != 8 || d.Range.Start.Character != 15 {
		t.Errorf("want the response schema on line 8 (0-based) at column 15, got %d:%d",
			d.Range.Start.Line, d.Range.Start.Character)
	}
}

// A property with no schema under it is null, which decodes; it must not be mistaken for the
// slot that failed to.
func TestANullPropertyDoesNotStealTheSlotSearch(t *testing.T) {
	//  1 name: demo
	//  2 input_schema: object
	//  3 tasks:
	//  4   - id: a
	//  5     action:
	//  6       type: external
	//  7       result_schema:
	//  8         properties:
	//  9           who:
	d := only(t, "name: demo\ninput_schema: object\ntasks:\n  - id: a\n    action:\n      type: external\n      result_schema:\n        properties:\n          who:\n    switch: end\n")
	if d.Range.Start.Line != 1 || d.Range.Start.Character != 14 {
		t.Errorf("want input_schema's value on line 1 (0-based) at column 14, got %d:%d",
			d.Range.Start.Line, d.Range.Start.Character)
	}
}

// Two slots are scalars and the decoder stopped at one of them; which one it was is not in the
// error. Underlining either is a guess, so the search declines — the same rule solePathEndingIn
// follows.
func TestTwoSlotShapedFailuresAreNotGuessedBetween(t *testing.T) {
	//  1 name: demo
	//  2 input_schema: object
	//  3 tasks:
	//  4   - id: a
	//  5     action:
	//  6       type: external
	//  7       result_schema: object
	d := only(t, "name: demo\ninput_schema: object\ntasks:\n  - id: a\n    action:\n      type: external\n      result_schema: object\n    switch: end\n")
	if d.Range.Start.Line != 0 {
		t.Errorf("want the fallback to the first line, got line %d", d.Range.Start.Line)
	}
}

// encoding/json's prose names the Go type that could not hold the value ("of type bool") and a
// field stack that skips the list index, so it resolved to the whole `tasks:` block.
func TestAFieldGivenTheWrongKindOfValueSaysWhatItTakes(t *testing.T) {
	//  1 name: demo
	//  2 tasks:
	//  3   - id: a
	//  4     only_once: 5
	d := only(t, "name: demo\ntasks:\n  - id: a\n    only_once: 5\n    switch: end\n")
	if d.Message != "only_once must be a boolean, not a number" {
		t.Errorf("message = %q", d.Message)
	}
	if d.Range.Start.Line != 3 || d.Range.Start.Character != 15 {
		t.Errorf("want the value on line 3 (0-based) at column 15, got %d:%d",
			d.Range.Start.Line, d.Range.Start.Character)
	}
}

func TestATopLevelFieldGivenTheWrongKindOfValueIsPlacedToo(t *testing.T) {
	//  1 name: demo
	//  2 tasks: 5
	d := only(t, "name: demo\ntasks: 5\n")
	if d.Message != "tasks must be a list, not a number" {
		t.Errorf("message = %q", d.Message)
	}
	if d.Range.Start.Line != 1 || d.Range.Start.Character != 7 {
		t.Errorf("want the value on line 1 (0-based) at column 7, got %d:%d",
			d.Range.Start.Line, d.Range.Start.Character)
	}
}
