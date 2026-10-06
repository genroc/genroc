package defschema

import (
	"os"
	"path/filepath"
	"testing"

	"genroc/internal/sources"

	"github.com/xeipuuv/gojsonschema"
)

// The schema is reflected from the same structs genctl marshals, so what can drift is the rest:
// an enum, a required omitempty field, a null where the schema says array.
func TestManifestSchemaAcceptsWhatGenctlSends(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(".genroc", `resolvers:
  - name: imp
    phase: typed
    command: [sh, -c, "cat > typed.json"]
    types: { Input: task.action.input, Absent: task.output }
  - name: gen
    phase: structural
    command: [sh, -c, "cat > structural.json; echo '{\"values\": [{}]}'"]
`)
	child := write("child.genroc.yaml", `name: child
input_schema: { type: object, properties: { code: { type: string } } }
tasks:
  - id: t
    switch: [{ goto: end }]
`)
	parent := write("parent.genroc.yaml", `name: parent
output: "$imp: ./process.ts"
tasks:
  - id: call
    <<: "$gen:"
    action:
      type: child
      name: child
      input: { code: "$imp: ./action.ts" }
    switch: [{ goto: $plain }]
  - id: plain
    output: "$imp: './task level.ts'"
    switch: [{ goto: end }]
`)

	docs, err := sources.LoadDocs([]string{child, parent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sources.Generate(docs); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, sent := range []string{"typed.json", "structural.json"} {
		got, err := os.ReadFile(filepath.Join(dir, sent))
		if err != nil {
			t.Fatalf("the resolver was never run, so there is nothing to check: %v", err)
		}
		r, err := gojsonschema.Validate(gojsonschema.NewBytesLoader(Manifest()), gojsonschema.NewBytesLoader(got))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		for _, e := range r.Errors() {
			t.Errorf("%s: the published manifest schema refuses what genctl sends, so a resolver validating its input breaks: %s\n%s", sent, e, got)
		}
	}
}
