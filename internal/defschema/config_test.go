package defschema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/xeipuuv/gojsonschema"
	"gopkg.in/yaml.v3"
)

// The two `.genroc` files the repo ships -- the one `genctl init` writes and the playground's --
// are what the schema must accept; a schema that underlines the scaffold is worse than none.
func TestConfigSchemaAcceptsTheShippedProjectFiles(t *testing.T) {
	for _, path := range []string{"../../cmd/genctl/templates/scripts/.genroc", "../../tests/playground/.genroc"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if errs := configErrors(t, string(raw)); len(errs) > 0 {
			t.Errorf("%s: the shipped file fails its own schema: %v", path, errs)
		}
	}
}

// What the schema is FOR: the reader refuses these, and the editor should say so first.
func TestConfigSchemaRefusesWhatTheReaderRefuses(t *testing.T) {
	scaffold, err := os.ReadFile("../../cmd/genctl/templates/scripts/.genroc")
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"a misspelled key":        strings.Replace(string(scaffold), "definitions:", "definitons:", 1),
		"a phase that is not one": strings.Replace(string(scaffold), "phase: code", "phase: structrual", 1),
		"a resolver with no name": strings.Replace(string(scaffold), "  - name: import\n", "", 1),
		// The reader refuses an EMPTY command, and YAML's null decodes to one; a reflected slice
		// is nullable unless the schema says otherwise.
		"a null command": strings.Replace(string(scaffold), "command: [npx, genroc-import]", "command: null", 1),
	} {
		if errs := configErrors(t, doc); len(errs) == 0 {
			t.Errorf("%s: the schema accepted a file the reader refuses", name)
		}
	}
}

func configErrors(t *testing.T, doc string) []string {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	asJSON, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	r, err := gojsonschema.Validate(gojsonschema.NewBytesLoader(Config()), gojsonschema.NewBytesLoader(asJSON))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var out []string
	for _, e := range r.Errors() {
		out = append(out, e.String())
	}
	return out
}
