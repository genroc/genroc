package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// declaredStatuses reads every `X Status = "..."` constant out of this package's own source, so
// the check is against the constants rather than against a second list someone has to remember.
func declaredStatuses(t *testing.T) map[string]string {
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
			if id, ok := spec.Type.(*ast.Ident); !ok || id.Name != "Status" {
				return true
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				out[name.Name] = value
			}
			return true
		})
	}
	return out
}

// A status with no line reaches the reference as a blank cell, which reads as an omission in
// the docs rather than as the missing entry it is. Nothing else fails when one is added.
func TestEveryStatusIsDocumented(t *testing.T) {
	declared := declaredStatuses(t)
	if len(declared) < 7 {
		t.Fatalf("read %d Status constants out of the source; the scan is broken, not the package", len(declared))
	}

	listed := map[Status]bool{}
	for _, info := range Statuses() {
		listed[info.Status] = true
		if info.Means == "" {
			t.Errorf("%q carries no prose", info.Status)
		}
	}
	for name, value := range declared {
		if !listed[Status(value)] {
			t.Errorf("%s (%q) is a status but reaches no reference row; add it to statusMeanings and statusOrder", name, value)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("Statuses() lists %d statuses and the package declares %d", len(listed), len(declared))
	}
}

// The predicates are what separate the rows, and they must be the engine's own: a page that
// computed "terminal" from a list beside the code is a page that can disagree with it.
func TestStatusInfoTakesItsPredicatesFromTheModel(t *testing.T) {
	for _, info := range Statuses() {
		if info.Terminal != info.Status.Terminal() {
			t.Errorf("%q: Terminal is %v, the model says %v", info.Status, info.Terminal, info.Status.Terminal())
		}
		if info.Accepts != info.Status.AcceptsExternalOutcome() {
			t.Errorf("%q: Accepts is %v, the model says %v", info.Status, info.Accepts, info.Status.AcceptsExternalOutcome())
		}
	}
}
