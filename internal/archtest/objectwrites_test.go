package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The upsert's row lock lasts one statement: content claimed in a second transaction sits
// held by nobody in between, where the sweep may take it. specs/object-store.md.
func TestObjectWritesGoThroughClaimObjects(t *testing.T) {
	const (
		file   = "internal/db/db_objects.go"
		helper = "claimObjects"
	)
	// The two release paths stamp a grace claim: a removal's second half, not an addition.
	allowed := map[string]bool{
		helper:                   true,
		"applyContextObjectDiff": true,
		"retireOrphanedLogRefs":  true,
	}

	fset := token.NewFileSet()
	root := repoRoot(t)
	pkg, err := parser.ParseDir(fset, filepath.Join(root, "internal", "db"), nil, 0)
	if err != nil {
		t.Fatalf("parse internal/db: %v", err)
	}

	for _, p := range pkg {
		for path, f := range p.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					name := sel.Sel.Name
					if name != "PutObject" && name != "PutObjectRef" {
						return true
					}
					if allowed[fn.Name.Name] {
						return true
					}
					t.Errorf("%s: %s calls %s directly. Object content and its claim must be written together by %s, inside a transaction — a lock held for one statement does not protect a claim made in the next. See %s.",
						fset.Position(call.Pos()), fn.Name.Name, name, helper, file)
					return true
				})
			}
		}
	}
}
