package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A directory is refused rather than walked: implicit recursion hides both the depth and the
// filename filter, so it would silently differ from the pattern that replaces it.
func TestExpandPaths_DirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	_, err := expandPaths([]string{dir})
	if err == nil {
		t.Fatal("a directory was accepted; it must ask for a pattern instead")
	}
	if !strings.Contains(err.Error(), "**") {
		t.Errorf("error %q does not show the pattern to use", err)
	}
}

// A mistyped filename and a pattern that matched nothing are different mistakes, and the
// message has to say which — "matched no files" reads as a broken pattern when the name is
// simply wrong.
func TestExpandPaths_MissingFileVersusEmptyPattern(t *testing.T) {
	dir := t.TempDir()
	if _, err := expandPaths([]string{filepath.Join(dir, "typo.genroc.yaml")}); err == nil ||
		!strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing file gave %v, want it to say the file is not there", err)
	}
	if _, err := expandPaths([]string{filepath.Join(dir, "*.genroc.yaml")}); err == nil ||
		!strings.Contains(err.Error(), "matched no files") {
		t.Errorf("empty pattern gave %v, want it to say the pattern matched nothing", err)
	}
}

// A glob entry, for the subset a directory cannot express. `**` is deliberately unsupported:
// a directory entry already recurses, so it would be a second way to say the same thing.
func TestExpandPaths_Globs(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.genroc.yaml", "b.genroc.yaml", "skip.yaml"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := expandPaths([]string{filepath.Join(root, "*.genroc.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("glob matched %v, want the two definitions only", got)
	}
}

// `**` is the case a directory entry cannot express: recurse AND filter by name. filepath.Glob
// has no `**`, which is why this uses doublestar.
func TestExpandPaths_DoubleStar(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"defs/orders/order-new.genroc.yaml",
		"defs/orders/order-old.genroc.yaml",
		"defs/billing/invoice.genroc.yaml",
		"defs/deep/deeper/order-nested.genroc.yaml",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := expandPaths([]string{filepath.Join(root, "defs/**/order-*.genroc.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range got {
		names = append(names, filepath.Base(g))
	}
	want := "order-nested.genroc.yaml order-new.genroc.yaml order-old.genroc.yaml"
	if strings.Join(names, " ") != want {
		t.Errorf("`**` matched %v\n  want (any depth, name-filtered): %s", names, want)
	}
}

// Braces are doublestar's too, and a plain `*` must keep working.
func TestExpandPaths_SingleStarStillWorks(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.genroc.yaml", "b.genroc.json", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := expandPaths([]string{filepath.Join(root, "*.genroc.{yaml,json}")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want both definitions", got)
	}
}

// Everything is a pattern, so a plain filename is the trivial case — and a file whose NAME
// contains glob metacharacters must still be reachable without escaping.
func TestExpandPaths_LiteralNameWithGlobChars(t *testing.T) {
	root := t.TempDir()
	odd := filepath.Join(root, "order[1].genroc.yaml")
	if err := os.WriteFile(odd, []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := expandPaths([]string{odd})
	if err != nil {
		t.Fatalf("a real file was refused because its name looks like a pattern: %v", err)
	}
	if len(got) != 1 || got[0] != odd {
		t.Errorf("got %v, want %q", got, odd)
	}
}

// `-f` is literal FIRST: an existing path wins over any pattern reading of it. The rule earns
// itself on a filename that looks like a pattern AND collides with a real one — globbing
// `a[1].yaml` finds `a1.yaml`, so without literal-first the wrong file is used silently.
func TestDefinitionPaths_LiteralBeatsAnAmbiguousPattern(t *testing.T) {
	root := t.TempDir()
	odd := filepath.Join(root, "a[1].genroc.yaml")
	decoy := filepath.Join(root, "a1.genroc.yaml")
	for _, f := range []string{odd, decoy} {
		if err := os.WriteFile(f, []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Read as a pattern — what `definitions:` entries get — it resolves to the DECOY. That is
	// the trap `-f`'s literal-first rule exists to avoid.
	viaPattern, err := expandPaths([]string{odd})
	if err != nil {
		t.Fatal(err)
	}
	if len(viaPattern) != 1 || viaPattern[0] != decoy {
		t.Fatalf("setup wrong: pattern gave %v, expected it to match the decoy %q", viaPattern, decoy)
	}

	viaLiteral, err := definitionPaths([]string{odd})
	if err != nil {
		t.Fatal(err)
	}
	if len(viaLiteral) != 1 || viaLiteral[0] != odd {
		t.Errorf("-f gave %v, want the exact file %q — a literal must not be globbed", viaLiteral, odd)
	}
}

// Both forms compose, and neither is dropped.
func TestDefinitionPaths_LiteralAndPatternTogether(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.genroc.yaml", "b.genroc.yaml"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := definitionPaths([]string{
		filepath.Join(root, "a.genroc.yaml"),
		filepath.Join(root, "b*.genroc.yaml"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want the literal and the match", got)
	}
}

// `compat --from <sel>` with no -f uses the project's own definitions: the "does what I have
// here break what is running" question. Only when no other side was named, or it would hijack
// a stored-versus-stored comparison.
func TestCompatDefaultsToTheProject(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".genroc"),
		[]byte("definitions: [\"./defs/**/*.genroc.yaml\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defs := filepath.Join(root, "defs")
	if err := os.MkdirAll(defs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defs, "a.genroc.yaml"), []byte("name: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := expandPaths(defaultDefinitionPaths(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "a.genroc.yaml" {
		t.Errorf("got %v, want the project definition", got)
	}
}

// ...and the same flag still globs when the value names no file, which is the only door in a
// command whose positional arguments mean something else (compat).
func TestDefinitionPaths_FileFlagGlobsWhenNotALiteral(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.genroc.yaml", "b.genroc.yaml", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("name: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := definitionPaths([]string{filepath.Join(root, "*.genroc.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("-f with a pattern gave %v, want both definitions", got)
	}
}

// An unquoted `-f defs/*.yaml` expands to `-f a b c`, and compat reads the leftovers as a
// process name — matching nothing, reporting an empty comparison, exiting 0. A quiet wrong
// answer is the worst outcome, so a path-shaped positional is refused instead.
func TestLooksLikePath(t *testing.T) {
	for in, want := range map[string]bool{
		"definitions/a.genroc.yaml": true,
		"a.genroc.yaml":             true, // suffix alone
		"./x.json":                  true,
		"definitions/order":         true, // separator alone — no suffix to fall back on
		"order_pipeline":            false,
		"order@2":                   false,
		"latest":                    false,
	} {
		if got := looksLikePath(in); got != want {
			t.Errorf("looksLikePath(%q) = %v, want %v", in, got, want)
		}
	}
}

// `-f` takes several values, stopping at the next flag — so an unquoted `-f defs/*.yaml` is one
// flag with many values, while `-f a b --channel prod` leaves the flag to be parsed.
func TestTakeFileValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		files []string
		rest  []string
	}{
		{"several in a row", []string{"-f", "a", "b", "c"}, []string{"a", "b", "c"}, nil},
		{"stops at a flag", []string{"-f", "a", "b", "--channel", "prod"},
			[]string{"a", "b"}, []string{"--channel", "prod"}},
		{"repeats", []string{"-f", "a", "-f", "b"}, []string{"a", "b"}, nil},
		{"equals form", []string{"-f=a", "--channel", "prod"}, []string{"a"}, []string{"--channel", "prod"}},
		{"flag first", []string{"--channel", "prod", "-f", "a", "b"},
			[]string{"a", "b"}, []string{"--channel", "prod"}},
		{"none", []string{"x", "y"}, nil, []string{"x", "y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, rest := takeFileValues(tc.args)
			if strings.Join(files, ",") != strings.Join(tc.files, ",") {
				t.Errorf("files = %v, want %v", files, tc.files)
			}
			if strings.Join(rest, ",") != strings.Join(tc.rest, ",") {
				t.Errorf("rest = %v, want %v", rest, tc.rest)
			}
		})
	}
}
