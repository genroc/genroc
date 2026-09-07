package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// requireFieldOnLiterals fails for every composite literal of one of types in internal/db that
// does not set field. The check is source-level because the miss is not: an omitted field takes
// its zero value, which is a legal row and a wrong one.
func requireFieldOnLiterals(t *testing.T, field string, types map[string]bool, consequence string) {
	t.Helper()
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
				if !ok || !types[sel.Sel.Name] {
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
				t.Errorf("%s: %s literal does not set %s — %s",
					fset.Position(lit.Pos()), sel.Sel.Name, field, consequence)
				return true
			})
		}
	}
	if seen == 0 {
		t.Fatalf("found no %v literals; this check has stopped checking anything", types)
	}
}
