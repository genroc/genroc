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

// Import boundaries inside the ROOT module. `ui` and `jwks` are separate modules and are fenced
// by go.mod rather than by this test -- they cannot reach `genroc/internal` at all. genctl
// shares a module with the server, so its boundary is a rule rather than a wall, and this is the
// wall. specs/ui-component.md.
//
// The rule: genctl is a CLIENT. It speaks HTTP to a genroc server and validates definitions
// before sending them, which is why it legitimately depends on the definition language --
// model, schema, expression, shape, template. It has no business linking the engine, the
// database, the API server or the outbound transport, and every one of those would arrive with
// dependencies (drivers, migrations, OpenAPI generation) that a CLI has no use for.
//
// This is what a `genctl` module would enforce if the split went further. It does not, because
// genctl shares its entire internal surface with the server, so a module boundary would relocate
// the dependency rather than remove it -- measured before deciding.
var forbiddenImports = map[string][]string{
	"cmd/genctl": {
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
