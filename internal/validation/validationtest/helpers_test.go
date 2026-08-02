package validationtest

import (
	"encoding/json"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

func runGenerate(t *testing.T, defJSON string) validation.SchemaFile {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	out, err := validation.Generate(&def)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return out
}

func runGenerateErr(t *testing.T, defJSON string) error {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	_, err := validation.Generate(&def)
	return err
}

func defKeys(out validation.SchemaFile) []string {
	return out.Defs.Names()
}

// defOf returns the named definition (the zero Schema when absent), so
// assertions can inspect it through the accessor API.
func defOf(out validation.SchemaFile, name string) schema.Schema {
	s, _ := out.Defs.Get(name)
	return s
}

func assertJSON(t *testing.T, got any, wantJSON string) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var gotParsed, wantParsed any
	if err := json.Unmarshal(raw, &gotParsed); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &wantParsed); err != nil {
		t.Fatalf("wantJSON is not valid JSON: %v\n%s", err, wantJSON)
	}
	ga, _ := json.MarshalIndent(gotParsed, "", "  ")
	gb, _ := json.MarshalIndent(wantParsed, "", "  ")
	if string(ga) != string(gb) {
		t.Errorf("schema mismatch:\n got:  %s\n want: %s", ga, gb)
	}
}

// jsonString renders a schema as compact JSON for equality assertions that want to show
// the whole type in the failure message rather than a diff of two trees.
func jsonString(t *testing.T, s schema.Schema) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return string(b)
}
