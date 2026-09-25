package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"genroc/internal/api"
)

// Every registered action must reach a page. The generator joins two reads of the registry —
// the OpenAPI document and api.Reference() — on method and path, and a join that silently
// matches nothing produces a shorter reference rather than an error.
func TestEveryActionReachesAPage(t *testing.T) {
	dir := t.TempDir()
	if err := writeHTTPReference(dir); err != nil {
		t.Fatalf("writeHTTPReference: %v", err)
	}
	var all strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	page := all.String()

	documented := strings.Count(page, "\n## ")
	if want := countOperations(t); documented != want {
		t.Errorf("the reference documents %d endpoints and the spec has %d", documented, want)
	}
	for _, ref := range api.Reference() {
		// The heading carries the base path, which the spec declares once in `servers` and a
		// root-mounted action overrides — so match the suffix rather than rebuilding the rule.
		if !strings.Contains(page, "\n## "+ref.Method+" ") || !strings.Contains(page, ref.Path+"<a ") {
			t.Errorf("%s %s reached no page", ref.Method, ref.Path)
		}
	}
}

// A permission is the one thing on these pages that no other document carries, so an endpoint
// whose line went missing would leave a reader guessing at the credential it needs.
func TestEveryEndpointStatesItsPermission(t *testing.T) {
	dir := t.TempDir()
	if err := writeHTTPReference(dir); err != nil {
		t.Fatalf("writeHTTPReference: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sections := strings.Split(string(b), "\n## ")
		for _, s := range sections[1:] {
			heading, _, _ := strings.Cut(s, "\n")
			if !strings.Contains(s, "Requires the ") && !strings.Contains(s, "Requires any of ") &&
				!strings.Contains(s, "Needs no credential.") {
				t.Errorf("%s: %q states no permission", e.Name(), heading)
			}
		}
	}
}

// An admin-only action declares NO permissions — the registry's fail-closed default — so the
// zero value and "undocumented" look identical here and only the wording tells them apart.
func TestAdminOnlyReadsAsARequirement(t *testing.T) {
	if got := permissionLine(api.ReferenceAction{}); got != "Requires the `admin` permission." {
		t.Errorf("an action with no Allow rendered %q, which reads as a missing line", got)
	}
	if got := permissionLine(api.ReferenceAction{Open: true}); got != "Needs no credential." {
		t.Errorf("an Open action rendered %q", got)
	}
}

func countOperations(t *testing.T) int {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(api.Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, item := range doc.Paths {
		for _, m := range httpMethods {
			if _, ok := item[m]; ok {
				n++
			}
		}
	}
	return n
}

func TestEveryEndpointLinksIntoSwagger(t *testing.T) {
	dir := t.TempDir()
	if err := writeHTTPReference(dir); err != nil {
		t.Fatalf("writeHTTPReference: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range strings.Split(string(b), "\n## ")[1:] {
			heading, _, _ := strings.Cut(s, "\n")
			if !strings.Contains(heading, `href="../../../swagger/index.html#/`) || !strings.Contains(heading, `target="_blank"`) || strings.Contains(heading, "#/Other/") ||
				strings.Contains(heading, `/" target`) {
				t.Errorf("%s: %q has no Swagger deep link naming its tag and operationId", e.Name(), heading)
			}
		}
	}
}
