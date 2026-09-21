package sources

import "testing"

// How a directive picks its resolver, as the project-file reference states it: entries are tried
// in order and taken first-match on name AND suffix, an empty `ext` accepts anything, and a
// suffix is a WHOLE suffix rather than an extension. The two failures are distinguished because
// they are worded differently: no entry carries the name, or some do and none take the suffix.
// docs reference/project-file.md.
func TestMatchResolver_NameAndSuffix(t *testing.T) {
	cfg := projectConfig{Resolvers: []resolverConfig{
		{Name: "import", Ext: []string{".ts"}},
		{Name: "import", Ext: []string{".js", ".MJS"}},
		{Name: "import"}, // no ext: accepts anything
		{Name: "spread", Ext: []string{".genroc.yaml"}},
	}}
	for _, tc := range []struct {
		name, directive, argument string
		wantIdx                   int
		wantKnown                 bool
	}{
		{"first entry that takes the suffix", "import", "a.ts", 0, true},
		{"a later entry catches what the first does not", "import", "a.js", 1, true},
		{"suffix matching ignores case on both sides", "import", "a.mjs", 1, true},
		// Nothing above accepts ".py"; the entry with no ext does, and only because it is
		// reached in order — an empty ext is not a higher-priority catch-all.
		{"an empty ext accepts anything", "import", "a.py", 2, true},
		// filepath.Ext calls both of these ".yaml", which is why the match is on the suffix.
		{"a whole suffix, not an extension", "spread", "x.genroc.yaml", 3, true},
		{"a shorter suffix is not the same suffix", "spread", "x.yaml", -1, true},
		{"an unknown name matches nothing and is not known", "nope", "a.ts", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, known, ok := cfg.matchResolver(tc.directive, tc.argument)
			if idx != tc.wantIdx {
				t.Errorf("$%s: %q picked entry %d, want %d", tc.directive, tc.argument, idx, tc.wantIdx)
			}
			if known != tc.wantKnown {
				t.Errorf("$%s: nameKnown = %v, want %v — it is what separates an unregistered resolver from a suffix it declines", tc.directive, known, tc.wantKnown)
			}
			if ok != (tc.wantIdx >= 0) {
				t.Errorf("$%s: ok = %v with index %d; the two must agree", tc.directive, ok, idx)
			}
		})
	}
}

// The built-in is appended after everything `.genroc` registers, which is what makes first-match
// the whole override rule — there is no shadowing pass to get wrong.
func TestMatchResolver_ABuiltinIsReachedLast(t *testing.T) {
	local := resolverConfig{Name: builtinProcess, Phase: phaseStructural, Ext: []string{".genroc.yaml"}}
	cfg := projectConfig{Resolvers: append([]resolverConfig{local}, builtins()...)}

	if idx, _, ok := cfg.matchResolver(builtinProcess, "child.genroc.yaml"); !ok || idx != 0 {
		t.Errorf("a local %q entry picked entry %d (ok=%v); it must win over the appended built-in", builtinProcess, idx, ok)
	}
	// The override is per suffix: a local entry that does not take this one falls through to
	// the built-in rather than shadowing the whole name.
	if idx, _, ok := cfg.matchResolver(builtinProcess, "child.genroc.json"); !ok || idx == 0 {
		t.Errorf("a suffix the local entry declines picked entry %d (ok=%v); it must reach the built-in", idx, ok)
	}
}
