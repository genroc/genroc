package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"genroc/internal/defschema"
)

func generatedDefinitionPages(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := writeDefinitionReference(dir); err != nil {
		t.Fatalf("writeDefinitionReference: %v", err)
	}
	// The project-file page is rendered by the same renderer and is filed elsewhere only
	// because `.genroc` configures the tooling rather than the language — the page standards
	// below are about the renderer, so it is swept with the rest.
	if err := writeConfigReference(dir); err != nil {
		t.Fatalf("writeConfigReference: %v", err)
	}
	pages := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		pages[e.Name()] = string(b)
	}
	return pages
}

// The pages are assembled from a hand-written list of $defs, so a type the schema grows reaches
// no page until someone adds it here — and an action type is the one that grows. Nothing else
// fails when it does: the reference simply stops mentioning it.
func TestEveryActionTypeReachesAPage(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(defschema.Process(), &root); err != nil {
		t.Fatal(err)
	}
	defs := root["$defs"].(map[string]any)
	arms := defs["ModelAction"].(map[string]any)["oneOf"].([]any)
	if len(arms) < 5 {
		t.Fatalf("the schema declares %d action types, which cannot be right", len(arms))
	}

	page := generatedDefinitionPages(t)["actions.md"]
	for _, raw := range arms {
		props := raw.(map[string]any)["properties"].(map[string]any)
		name := props["type"].(map[string]any)["const"].(string)
		if !strings.Contains(page, "\n## "+name+"\n") {
			t.Errorf("action type %q has no section, so the reference does not know it exists", name)
		}
	}
}

// Every row's Description comes from a `description:` struct tag. A field that loses its tag
// still renders — as a row with an empty cell, which reads as an omission rather than as the
// missing tag it is.
func TestEveryFieldIsDescribed(t *testing.T) {
	for name, page := range generatedDefinitionPages(t) {
		for _, line := range strings.Split(page, "\n") {
			if !strings.HasPrefix(line, "| `") {
				continue
			}
			cells := strings.Split(line, " | ")
			if desc := strings.TrimSuffix(strings.TrimSpace(cells[len(cells)-1]), "|"); strings.TrimSpace(desc) == "" {
				t.Errorf("%s: %s has no description", name, cells[0])
			}
		}
	}
}

// A pipe ends a table cell, and backticks do not protect it: an unescaped union type silently
// shifts every column after it, so Required lands under Description and the last cell is lost.
func TestUnionTypesSurviveATableCell(t *testing.T) {
	page := renderDefPage(defPage{
		title: "T", slug: "t", blurb: "b",
		sections: []defSection{{title: "S", fields: []defField{
			{name: "headers", typ: "object | string", desc: "Request headers."},
		}}},
	}, 1)
	row := ""
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "| `headers`") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no row rendered:\n%s", page)
	}
	if !strings.Contains(row, `\|`) {
		t.Errorf("the union's pipe is unescaped, so the row loses a column: %s", row)
	}
	if got := strings.Count(row, " | "); got != 3 {
		t.Errorf("row has %d cell separators, want 3: %s", got, row)
	}
}

// A union arm can carry a shape of its own -- `retry`'s long form IS its four slots, a switch
// case IS its `case`/`goto` pair. Rendering the arm as the bare word `object` builds a page
// that looks complete and answers nothing, which is how this shipped the first time.
func TestObjectArmsRenderTheirShape(t *testing.T) {
	pages := generatedDefinitionPages(t)
	for _, c := range []struct{ page, union string }{
		{"error-handling.md", "ModelRetry"},
		{"task.md", "ModelSwitchMap"},
	} {
		var root map[string]any
		if err := json.Unmarshal(defschema.Process(), &root); err != nil {
			t.Fatal(err)
		}
		def := root["$defs"].(map[string]any)[c.union].(map[string]any)
		shaped := 0
		for _, arm := range armsOf(def) {
			for _, f := range arm.fields {
				shaped++
				if !strings.Contains(pages[c.page], "| `"+f.name+"` |") {
					t.Errorf("%s: %s's %q slot reaches no page", c.page, c.union, f.name)
				}
			}
		}
		if shaped == 0 {
			t.Errorf("%s carries no shaped arm, so this test asserts nothing", c.union)
		}
	}
}
