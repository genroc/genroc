package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every write of an instance's context must set Objects.
//
// The column lists what the context references, and the claims in object_refs are written from
// the same set. Omitting it is not a compile error and not a test failure anywhere near the
// change: the field defaults to "", so the instance's declaration is ERASED while its claims
// stand, and its own values start looking like content nothing accounts for. The GC does not care
// -- claims are what it reads -- so the damage is invisible until something compares the two.
//
// It was missed twice within minutes of the column being added (RetryProcess passing raw columns
// through, and the parent park in SpawnChildrenAndWait), which is what this exists to stop. It is
// the price of the references living beside the values rather than inside them; this check is how
// that price is paid once. specs/object-store.md.
func TestInstanceWritesCarryObjects(t *testing.T) {
	const field = "Objects"
	writes := map[string]bool{
		"UpdateInstanceParams":         true,
		"UpdateInstanceProgressParams": true,
		"InsertInstanceParams":         true,
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, filepath.Join(repoRoot(t), "internal", "db"), nil, 0)
	if err != nil {
		t.Fatalf("parse internal/db: %v", err)
	}
	seen := 0
	for _, p := range pkgs {
		for path, f := range p.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || !writes[sel.Sel.Name] {
					return true
				}
				if len(lit.Elts) == 0 {
					return true // a zero value returned beside an error, not a write
				}
				seen++
				for _, e := range lit.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
						return true
					}
				}
				t.Errorf("%s: %s literal does not set %s — the instance's reference declaration would be erased while its claims stand",
					fset.Position(lit.Pos()), sel.Sel.Name, field)
				return true
			})
		}
	}
	if seen == 0 {
		t.Fatal("found no instance write literals; this check has stopped checking anything")
	}
}
