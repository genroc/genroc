package main

import (
	"os"
	"path/filepath"
	"testing"
)

// `genctl apply` with no paths reads `definitions:` from the nearest .genroc, and resolves it
// against THAT file — so the command behaves the same from any directory in the project.
func TestDefaultDefinitionPaths_ResolveAgainstTheConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".genroc"), []byte("definitions: [definitions/]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "definitions", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, from := range []string{root, nested} {
		got := defaultDefinitionPaths(from)
		if len(got) != 1 {
			t.Fatalf("from %s: got %v, want one path", from, got)
		}
		if want := filepath.Join(root, "definitions"); got[0] != want {
			t.Errorf("from %s: got %q, want %q — a relative entry must resolve against .genroc, not the cwd", from, got[0], want)
		}
	}
}

func TestDefaultDefinitionPaths_AbsentIsEmpty(t *testing.T) {
	if got := defaultDefinitionPaths(t.TempDir()); got != nil {
		t.Errorf("got %v with no .genroc; the caller reports that, not this", got)
	}
}
