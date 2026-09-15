package validation

import "testing"

// A guard is written in one task's frame and read in another's. Every row that translates is
// a proof that survives the hop; every row that does not is one the target would otherwise
// read as a fact about a DIFFERENT value under the same name — which is the whole cost of
// getting this wrong, since a wrong refinement turns a registration error into an
// uncatchable engine.expression. specs/guard-narrowing.md.
func TestTranslateGuard(t *testing.T) {
	for _, tc := range []struct {
		name          string
		path          string
		guardTask     string
		exportsOutput bool
		want          string // "" = must not travel
	}{
		{name: "self.output becomes the task's own outputs entry",
			path: "self.output.v", guardTask: "a", exportsOutput: true, want: "outputs.a.v"},
		{name: "deeper paths keep their tail",
			path: "self.output.v.w", guardTask: "a", exportsOutput: true, want: "outputs.a.v.w"},
		{name: "the whole output",
			path: "self.output", guardTask: "a", exportsOutput: true, want: "outputs.a"},
		{name: "an index survives",
			path: "self.output.items[0]", guardTask: "a", exportsOutput: true, want: "outputs.a.items[0]"},
		{name: "a non-identifier key keeps its bracket form",
			path: `self.output["x.y"]`, guardTask: "a", exportsOutput: true, want: `outputs.a["x.y"]`},
		{name: "a non-identifier TASK id takes bracket form too",
			path: "self.output.v", guardTask: "user-protected", exportsOutput: true, want: `outputs["user-protected"].v`},

		{name: "input names the same value everywhere",
			path: "input.user_id", guardTask: "a", exportsOutput: true, want: "input.user_id"},
		{name: "another task's output names the same value everywhere",
			path: "outputs.b.v", guardTask: "a", exportsOutput: true, want: "outputs.b.v"},

		// Everything below must NOT travel.
		{name: "a task that exports nothing has no downstream name",
			path: "self.output.v", guardTask: "a", exportsOutput: false},
		{name: "self.result is the guarding task's own",
			path: "self.result.v", guardTask: "a", exportsOutput: true},
		{name: "self.previous is the guarding task's own",
			path: "self.previous.v", guardTask: "a", exportsOutput: true},
		{name: "last_error is refilled on every transition",
			path: "last_error.code", guardTask: "a", exportsOutput: true},
		// The trap: frame-invariant in NAME, re-resolved from the environment every tick.
		{name: "config proves nothing downstream",
			path: "config.SERVER_URL", guardTask: "a", exportsOutput: true},
		{name: "a computed key means something else in the target frame",
			path: "m[k]", guardTask: "a", exportsOutput: true},
		{name: "an unknown root is rejected rather than guessed",
			path: "whatever.v", guardTask: "a", exportsOutput: true},
		{name: "empty", path: "", guardTask: "a", exportsOutput: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translateGuard(tc.path, tc.guardTask, tc.exportsOutput)
			if tc.want == "" {
				if ok {
					t.Fatalf("translateGuard(%q) = %q, but this proof must not travel", tc.path, got)
				}
				return
			}
			if !ok {
				t.Fatalf("translateGuard(%q) refused a proof that survives the hop", tc.path)
			}
			if got != tc.want {
				t.Errorf("translateGuard(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
