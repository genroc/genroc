package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildGenroc(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "genroc")
	out, err := exec.Command("go", "build", "-o", path, "genroc/cmd/genroc").CombinedOutput()
	if err != nil {
		t.Fatalf("build genroc: %v\n%s", err, out)
	}
	return path
}

func TestEveryServerFlagReachesThePage(t *testing.T) {
	genroc := buildGenroc(t)
	flags, err := readServerFlags(genroc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeServerReference(dir, genroc); err != nil {
		t.Fatalf("writeServerReference: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(dir, "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		if !strings.Contains(string(page), "\n### --"+f.name+"\n") {
			t.Errorf("--%s is in `genroc -h` but has no heading on the page", f.name)
		}
		if f.usage == "" {
			t.Errorf("--%s parsed with no usage; the indentation the parser reads has moved", f.name)
		}
	}
	if !strings.Contains(string(page), "genroc token create") {
		t.Error("the page lost the genroc token subcommand")
	}
	if strings.Contains(string(page), "specs/") {
		t.Error("the page links into specs/, which the docs site never does; fix the help text")
	}
}

func TestAServerFlagInNoGroupFailsGeneration(t *testing.T) {
	var flags []serverFlag
	for _, g := range serverGroups {
		for _, n := range g.flags {
			flags = append(flags, serverFlag{name: n, usage: "x"})
		}
	}
	if _, err := renderServerReference(append(flags, serverFlag{name: "new-flag", usage: "x"}), "genroc token create"); err == nil ||
		!strings.Contains(err.Error(), "--new-flag") {
		t.Fatalf("an ungrouped flag must fail generation, naming it; got %v", err)
	}
	if _, err := renderServerReference(flags[1:], "genroc token create"); err == nil {
		t.Fatal("a group naming a flag genroc lacks must fail generation")
	}
}
