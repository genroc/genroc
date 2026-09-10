package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Object content and its claim must be written by claimObjects, inside a transaction. The content
// upsert's ON CONFLICT DO UPDATE exists to take a row lock, which lasts exactly as long as its
// statement: content written and claimed in two transactions sits committed and held by nobody in
// between, where the sweep is entitled to take it. The mistake has been made twice, both times by
// copying the loop that now has one home. specs/object-store.md.
func TestObjectWritesGoThroughClaimObjects(t *testing.T) {
	const (
		file   = "internal/db/db_objects.go"
		helper = "claimObjects"
	)
	// The two release paths stamp a grace claim, which is a removal's second half rather than an
	// addition: an instance letting go of a value, and the sweep letting go on behalf of a log
	// row that no longer exists.
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
