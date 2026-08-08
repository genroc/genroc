// Package archtest holds checks about the shape of the codebase rather than its
// behaviour — rules a reviewer would otherwise have to remember.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Package-level mutable state is reachable from every goroutine, and in Go a goroutine can
// be created anywhere — so it must be synchronised whether or not anything is actually
// shared, the synchronisation is invisible at the call site, and it is a GC root, which
// turns "forgot to clean up" from a self-correcting bug into a permanent leak. It also
// creates dependencies no signature declares, and cannot be reset by dropping an owner.
//
// The rule is narrower than "avoid globals": package-level is fine for VALUES (lookup
// tables, compiled regexes, error sentinels, embedded files) and wrong for STATE. If it
// changes after init, it wants an owner.
//
// Each exception below is a decision someone made on purpose. Adding one means writing
// down which owner it could have had and why it does not — not appending a line.
var allowed = map[string]string{
	"template.cache": "memo of a pure function on the eval hot path; an owner would mean threading a " +
		"cache through shape.Eval/Roots/infer and every recursive call. template_bench_test.go is the justification.",
	"db.clockOffset": "the test/sim clock shift. Slated to move onto an owner — per-worker clocks are " +
		"what a frozen-worker simulation needs, and a process-global offset cannot express worker-vs-DB skew.",
	// The two Once/bytes pairs are one decision each: a lazily built artifact of the
	// binary's own types, written inside Do and never reset. No owner exists to hang them
	// on — Spec() is called before any server is constructed, to emit the spec file.
	"api.processSchemaOnce":  "write-once memo of the generated process schema; see the note above.",
	"api.processSchemaBytes": "the payload processSchemaOnce fills in.",
	"api.specOnce":           "write-once memo of the generated OpenAPI spec; see the note above.",
	"api.specBytes":          "the payload specOnce fills in.",
}

func TestNoPackageLevelMutableState(t *testing.T) {
	root := repoRoot(t)
	violations := map[string]token.Position{}

	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// sqlc owns internal/db/gen; archtest is this file.
			if d.Name() == "gen" || d.Name() == "archtest" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		for name, pos := range mutableGlobals(fset, file) {
			violations[file.Name.Name+"."+name] = pos
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}

	var unexpected []string
	for key, pos := range violations {
		if _, ok := allowed[key]; !ok {
			unexpected = append(unexpected, key+" at "+pos.String())
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("package-level mutable state, which every goroutine can reach:\n  %s\n\n"+
			"Give it an owner (a struct field on the thing whose lifetime it shares), or add it to `allowed` "+
			"with the reason an owner was not possible. See the comment above `allowed`.",
			strings.Join(unexpected, "\n  "))
	}

	for key := range allowed {
		if _, ok := violations[key]; !ok {
			t.Errorf("%s is allow-listed but is no longer package-level mutable state; drop the entry", key)
		}
	}
}

// mutableGlobals reports a file's package-level vars that hold state rather than a value:
// a sync/atomic type is state by construction, and anything assigned or index-assigned
// after its declaration is state by evidence. A read-only lookup table matches neither.
func mutableGlobals(fset *token.FileSet, file *ast.File) map[string]token.Position {
	declared := map[string]token.Position{}
	stateful := map[string]bool{}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == "_" {
					continue
				}
				declared[name.Name] = fset.Position(name.Pos())
				if vs.Type != nil && mentionsSyncPrimitive(vs.Type) {
					stateful[name.Name] = true
				}
			}
		}
	}

	// Written after declaration — reassigned, compound-assigned, incremented, or having an
	// element replaced. Locals that shadow a package name are counted too; over-reporting
	// is the safe direction for a rule whose escape hatch is one allow-list line.
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				return true
			}
			for _, lhs := range s.Lhs {
				if name, ok := rootIdent(lhs); ok {
					stateful[name] = true
				}
			}
		case *ast.IncDecStmt:
			if name, ok := rootIdent(s.X); ok {
				stateful[name] = true
			}
		}
		return true
	})

	out := map[string]token.Position{}
	for name, pos := range declared {
		if stateful[name] {
			out[name] = pos
		}
	}
	return out
}

// rootIdent unwraps x, x[k] and x[k][j] to x — the name an assignment ultimately writes
// through. A selector (x.f) is deliberately not unwrapped: that is a field on some value,
// not the package-level name itself.
func rootIdent(e ast.Expr) (string, bool) {
	for {
		switch v := e.(type) {
		case *ast.Ident:
			return v.Name, true
		case *ast.IndexExpr:
			e = v.X
		default:
			return "", false
		}
	}
}

func mentionsSyncPrimitive(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "sync" || pkg.Name == "atomic") {
			found = true
		}
		return true
	})
	return found
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
