package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"genroc/internal/api"
)

// The docs site publishes the process schema by running this binary into
// docs/public/ (Makefile: docs-schema), so a caller that asked for a file and got
// none would ship a site whose $schema URL 404s.
func TestWriteSchemaFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process-schema.json")
	write(path, api.ProcessSchema)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("-schema wrote no file at %s: %v", path, err)
	}
	if !bytes.Equal(got, api.ProcessSchema()) {
		t.Errorf("published bytes differ from api.ProcessSchema(); the site would serve a stale schema")
	}
	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatalf("published file is not JSON: %v", err)
	}
	if _, ok := root["$defs"]; !ok {
		t.Errorf("published schema has no $defs, so every $ref in it dangles; got keys %v", keysOf(root))
	}
}

// "" is how the Makefile asks for the schema without also clobbering openapi.json.
func TestWriteEmptyPathSkipsBuild(t *testing.T) {
	built := false
	write("", func() []byte { built = true; return nil })
	if built {
		t.Error(`write("") built the spec anyway; an empty path must skip the work, not just the file`)
	}
	if _, err := os.Stat(""); err == nil {
		t.Error(`write("") created a file`)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
