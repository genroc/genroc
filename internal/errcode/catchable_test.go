package errcode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// A code is either catchable — where `catchable` describes it and an editor offers it — or
// terminal. One in neither is invisible in both directions: never offered while it is written,
// never explained once it is reported. Reading the declarations rather than a second list is
// the point; a list would be the drift.
func TestEveryCodeIsClassified(t *testing.T) {
	declared := declaredCodes(t)
	if len(declared) < 10 {
		t.Fatalf("read %d Code constants out of the package source; the scan is broken, not the package", len(declared))
	}

	listed := map[string]Info{}
	for _, info := range catchable {
		listed[string(info.Code)] = info
	}

	for name, code := range declared {
		terminal := strings.HasPrefix(code, "engine.")
		if _, ok := listed[code]; ok == terminal {
			if terminal {
				t.Errorf("%s (%q) is terminal — it goes straight to failInstance, so no on_error rule can name it; remove it from catchable", name, code)
				continue
			}
			t.Errorf("%s (%q) is catchable and missing from catchable: add it with the Kind of task that reports it and what it means, or name it engine.* if nothing can catch it", name, code)
		}
	}

	for _, info := range catchable {
		code := string(info.Code)
		switch {
		case info.Kinds == 0:
			t.Errorf("%q is offered to no task at all; give it the Kind that reports it", code)
		case info.Means == "":
			t.Errorf("%q carries no prose, so an editor offers a bare name", code)
		case strings.ContainsRune(code, '%'):
			t.Errorf("%q is a pattern, not a code: this table is what the engine stores, and a wildcard spelling belongs to whoever offers it", code)
		case !hasValue(declared, code):
			t.Errorf("catchable names %q, which this package does not declare — a code is the constant or it is a typo", code)
		}
	}
}

func hasValue(declared map[string]string, code string) bool {
	for _, v := range declared {
		if v == code {
			return true
		}
	}
	return false
}

// declaredCodes reads every `X Code = "..."` constant out of the package's own source.
func declaredCodes(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(f fs.FileInfo) bool {
		return !strings.HasSuffix(f.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	out := map[string]string{}
	for _, pkg := range pkgs {
		ast.Inspect(pkg, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if id, ok := spec.Type.(*ast.Ident); !ok || id.Name != "Code" {
				return true
			}
			for i, name := range spec.Names {
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				out[name.Name] = value
			}
			return true
		})
	}
	return out
}

// The mirror of TestEveryCodeIsClassified for the other half. The prefix check there says a
// terminal code must stay OUT of catchable; nothing said it must be IN terminal, so a new
// engine.* code was classified by being absent from one list rather than present in any.
func TestEveryTerminalCodeIsListed(t *testing.T) {
	listed := map[string]Info{}
	for _, info := range terminal {
		listed[string(info.Code)] = info
	}
	found := 0
	for name, code := range declaredCodes(t) {
		if !strings.HasPrefix(code, "engine.") {
			continue
		}
		found++
		info, ok := listed[code]
		if !ok {
			t.Errorf("%s (%q) is terminal but missing from terminal: add it with what it means, or nothing can name it in the reference", name, code)
			continue
		}
		if info.Means == "" {
			t.Errorf("%q carries no prose, so it surfaces as a bare name", code)
		}
		if info.Kinds != 0 {
			t.Errorf("%q carries Kinds %d; no task reports a terminal code, the engine does", code, info.Kinds)
		}
	}
	if found < 5 {
		t.Fatalf("read %d engine.* codes out of the source; the scan is broken, not the package", found)
	}
	for code := range listed {
		if !strings.HasPrefix(code, "engine.") {
			t.Errorf("%q is in terminal but is not an engine.* code", code)
		}
	}
}
