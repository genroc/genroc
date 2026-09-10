package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Import boundaries inside the ROOT module. `ui` and `jwks` are fenced by go.mod instead -- NOT
// by the internal rule, which is path-prefix and not module-scoped.
//
// The rule here: genctl is a CLIENT. It speaks HTTP and infers the types a source resolver
// typechecks against, so it legitimately depends on the definition language, and has no business
// linking the engine, the database, the API server or the outbound transport -- each of which
// arrives with dependencies a CLI has no use for. A `genctl` module would relocate the dependency
// rather than remove it. specs/ui-component.md, specs/language-server.md §4.
var forbiddenImports = map[string][]string{
	"cmd/genctl": {
		"genroc/internal/db",
		"genroc/internal/api",
		"genroc/internal/engine",
		"genroc/internal/transport",
	},
	// The language server behind `genctl lsp`. Same rule for the same reason: it reads the
	// definition language and speaks to no server at all. A module of its own was measured
	// and rejected -- it would have fenced nothing it does not already inherit, and could
	// not reach the project config in cmd/genctl. specs/language-server.md §4.
	"internal/lsp": {
		"genroc/internal/db",
		"genroc/internal/api",
		"genroc/internal/engine",
		"genroc/internal/transport",
	},
}

func TestBinariesKeepTheirImportBoundaries(t *testing.T) {
	root := repoRoot(t)
	for dir, forbidden := range forbiddenImports {
		t.Run(dir, func(t *testing.T) {
			for _, bad := range violatingImports(t, filepath.Join(root, dir), forbidden) {
				t.Errorf("%s imports %s\n  %s is a client: it speaks HTTP to a server and "+
					"validates definitions locally. Linking this pulls in dependencies a CLI has "+
					"no use for, and makes it a second implementation of the server.",
					dir, bad, dir)
			}
		})
	}
}

// violatingImports walks dir and returns every import matching a forbidden prefix, sorted so a
// failure reads the same on every run.
func violatingImports(t *testing.T, dir string, forbidden []string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if p == bad || strings.HasPrefix(p, bad+"/") {
					seen[p] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
