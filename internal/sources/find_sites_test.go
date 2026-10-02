package sources

import "testing"

// An editor analyses whatever is open, so a non-mapping root reaches findSites; it must not panic.
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
