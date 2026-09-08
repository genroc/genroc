package defdoc

import (
	"encoding/json"
	"strings"
	"testing"
)

func parse(t *testing.T, src string) *Doc {
	t.Helper()
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d
}

func span(t *testing.T, d *Doc, path string) Span {
	t.Helper()
	s, ok := d.Span(path)
	if !ok {
		t.Fatalf("no span for %q; have %v", path, d.Paths())
	}
	return s
}

const twoTasks = `
name: demo
tasks:
  - id: fetch
    action:
      type: fetch
      url: "https://example.com"
    switch: next
  - id: done
    switch: end
`

// The two spellings are the whole reason the index exists: a validator namespace produces the
// physical one, a diagnostic and `genctl schema context` produce the logical one, and a
// conversion between them would be a rule that can be wrong.
func TestATaskIsAddressableByIndexAndById(t *testing.T) {
	d := parse(t, twoTasks)
	byIndex := span(t, d, "tasks[0].action.url")
	byID := span(t, d, "tasks.fetch.action.url")
	if byIndex != byID {
		t.Fatalf("same node, different spans:\n by index: %+v\n by id:    %+v", byIndex, byID)
	}
	if byIndex.Value.Line != 7 {
		t.Errorf("url value is on line 7 of the fixture, got line %d", byIndex.Value.Line)
	}
}

func TestTheSecondTaskIsNotAddressedByTheFirstsId(t *testing.T) {
	d := parse(t, twoTasks)
	first := span(t, d, "tasks.fetch")
	second := span(t, d, "tasks.done")
	if first == second {
		t.Fatal("tasks.fetch and tasks.done resolved to one span; ids are not distinguishing elements")
	}
	if _, ok := d.Span("tasks.done.action"); ok {
		t.Error("tasks.done has no action, but an address into one resolved")
	}
}

// A diagnostic chooses which to underline: an unknown key underlines the key, a value that
// failed its rule underlines the value. One range could not serve both.
func TestKeyAndValueAreSeparatelyLocated(t *testing.T) {
	d := parse(t, "name: demo\n")
	s := span(t, d, "name")
	if s.Key.Col != 1 || s.Key.EndCol != 5 {
		t.Errorf("key `name` occupies columns 1-5, got %d-%d", s.Key.Col, s.Key.EndCol)
	}
	if s.Value.Col != 7 || s.Value.EndCol != 11 {
		t.Errorf("value `demo` occupies columns 7-11, got %d-%d", s.Value.Col, s.Value.EndCol)
	}
}

func TestTheRootHasNoKey(t *testing.T) {
	d := parse(t, "name: demo\n")
	s := span(t, d, "")
	if !s.Key.Empty() {
		t.Errorf("the document root is introduced by no key, got %+v", s.Key)
	}
}

const anchored = `
defaults: &base
  switch: end
  only_once: true
tasks:
  - id: a
    <<: *base
  - id: b
    <<: *base
    only_once: false
`

// The location a reader needs for a merged key is where it was written -- inside the anchor --
// not the `<<` line that pulled it in.
func TestAMergedKeyIsLocatedInsideTheAnchor(t *testing.T) {
	d := parse(t, anchored)
	s := span(t, d, "tasks.a.only_once")
	if s.Value.Line != 4 {
		t.Errorf("only_once was written on line 4, inside the anchor; located at line %d", s.Value.Line)
	}
}

// The span index and the value must agree on which key won, or a diagnostic underlines a line
// whose text is not the value it is complaining about.
func TestAnExplicitKeyBeatsAMergedOneForTheSpanToo(t *testing.T) {
	d := parse(t, anchored)
	m, _ := d.Value.(map[string]any)
	tasks, _ := m["tasks"].([]any)
	b, _ := tasks[1].(map[string]any)
	if b["only_once"] != false {
		t.Fatalf("explicit only_once:false must beat the merged true, got %v", b["only_once"])
	}
	s := span(t, d, "tasks.b.only_once")
	if s.Value.Line != 10 {
		t.Errorf("the winning only_once is the explicit one on line 10; located at line %d "+
			"(the merged one it beat)", s.Value.Line)
	}
}

func TestASequenceMergeIsRefusedRatherThanSilentlyReversed(t *testing.T) {
	_, err := Parse([]byte("a: &x {p: 1}\nb: &y {p: 2}\nc:\n  <<: [*x, *y]\n"))
	if err == nil {
		t.Fatal("a sequence merge reads backwards under YAML 1.1 precedence and must be refused")
	}
	if !strings.Contains(err.Error(), "EARLIER") {
		t.Errorf("the refusal must say why it reads backwards, got: %v", err)
	}
}

// Decoding into `any` floats big integers: this literal once left the CLI as 1.2e+53, having
// been corrupted before the server ever saw it.
func TestLargeIntegerLiteralsSurviveExactly(t *testing.T) {
	const big = "123748297583958759399485776859493938587768583992939858"
	d := parse(t, "id: "+big+"\n")
	b, err := json.Marshal(d.Value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"id":` + big + `}`; string(b) != want {
		t.Errorf("got %s\nwant %s", b, want)
	}
}

func TestNonJSONNumericSpellingsFallBackToYAML(t *testing.T) {
	d := parse(t, "a: 0x1F\nb: 1_000\n")
	m := d.Value.(map[string]any)
	if m["a"] != 31 {
		t.Errorf("0x1F is 31 to YAML and not a JSON literal, got %v", m["a"])
	}
	if m["b"] != 1000 {
		t.Errorf("1_000 is 1000 to YAML and not a JSON literal, got %v", m["b"])
	}
}

func TestParseAllDropsEmptyDocuments(t *testing.T) {
	docs, err := ParseAll([]byte("name: a\n---\n---\nname: b\n"))
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("a bare `---` is not a definition; want 2 documents, got %d", len(docs))
	}
	if docs[1].Value.(map[string]any)["name"] != "b" {
		t.Errorf("second document is b, got %v", docs[1].Value)
	}
}

// Each document is indexed on its own, or a path would resolve into whichever document
// happened to be parsed last.
func TestParseAllIndexesEachDocumentSeparately(t *testing.T) {
	docs, err := ParseAll([]byte("name: a\n---\nname: b\n"))
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if a, b := span(t, docs[0], "name"), span(t, docs[1], "name"); a.Value.Line == b.Value.Line {
		t.Errorf("both documents' `name` located at line %d", a.Value.Line)
	}
}

func TestASequenceElementWithoutAnIdIsAddressedByIndex(t *testing.T) {
	d := parse(t, "on_error:\n  - case: a\n  - case: b\n")
	if _, ok := d.Span("on_error.1.case"); !ok {
		t.Errorf("an element with no id keeps its index as its address; have %v", d.Paths())
	}
}

// A container's extent is its children's, so an error on a whole task underlines the task.
func TestAMappingSpansItsChildren(t *testing.T) {
	d := parse(t, twoTasks)
	s := span(t, d, "tasks.fetch")
	if s.Value.Line != 4 || s.Value.EndLine != 8 {
		t.Errorf("the fetch task runs from line 4 to line 8, got %d-%d", s.Value.Line, s.Value.EndLine)
	}
}
