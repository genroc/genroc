package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryGenrocUIFlagAndKeyReachesThePage(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "genroc-ui")
	if out, err := exec.Command("go", "build", "-o", bin, "genroc/ui").CombinedOutput(); err != nil {
		t.Fatalf("build genroc-ui: %v\n%s", err, out)
	}
	dir := t.TempDir()
	if err := writeUIReference(dir, bin); err != nil {
		t.Fatalf("writeUIReference: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(dir, "genroc-ui.md"))
	if err != nil {
		t.Fatal(err)
	}
	flags, err := readServerFlags(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		if !strings.Contains(string(page), "\n### --"+f.name+"\n") {
			t.Errorf("--%s is in `genroc-ui -h` but has no heading on the page", f.name)
		}
	}
	out, err := exec.Command(bin, "config-schema").Output()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := parseSchema(out, "ui.yaml")
	if err != nil {
		t.Fatal(err)
	}
	objects := []jsonSchema{schema}
	for _, def := range schema["$defs"].(map[string]any) {
		objects = append(objects, def.(map[string]any))
	}
	for _, obj := range objects {
		for _, f := range fieldsOf(obj) {
			if !strings.Contains(string(page), "| `"+f.name+"` |") {
				t.Errorf("ui.yaml key %q has no row: a struct grew a field no section renders", f.name)
			}
		}
	}
}
