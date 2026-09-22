package sources

import "testing"

// Anything open in an editor is analysed as it is typed, so a document whose root is not a
// mapping -- a task list pasted on its own -- reaches findSites like any other. Reading its
// `name` used to assert the root WAS a mapping, and the language server died on the file
// instead of answering nothing for it.
func TestFindSites_RootIsNotAMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		root any
	}{
		{"a sequence", []any{map[string]any{"id": "load_user"}}},
		{"a scalar", "name: hello"},
		{"nothing at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sites, err := findSites([]sourceDoc{{Value: tc.root, File: "snippet.genroc.yaml"}}, projectConfig{})
			if err != nil {
				t.Fatalf("findSites: %v", err)
			}
			if len(sites) != 0 {
				t.Fatalf("no directive in it, so no site: got %d", len(sites))
			}
		})
	}
}
