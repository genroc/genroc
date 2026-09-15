package validationtest

import "testing"

// A `switch` case's proof travels the edge it selects, so the task it routes to can read what
// was proved. The cost of getting this wrong is asymmetric — a refinement that does not follow
// turns a registration error into an uncatchable engine.expression — so every accepting row
// here is paired with the shape that must still be refused.
// specs/guard-narrowing.md.

// twoTask builds `a` (guarded switch) routing to `b`, which uses what `a` proved.
func twoTask(cases, use string) string {
	return `{"name":"p",
	 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]},"m":{"type":["integer","null"]}},"required":["n","m"]},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "output":{"v":"$: input.n","w":"$: input.m"},
	   "switch":` + cases + `},
	  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
	   "output":{"r":"$: ` + use + `"},"switch":"end"},
	  {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},"switch":"end"}]}`
}

func TestGuardNarrowing_AcrossAnEdge(t *testing.T) {
	for _, tc := range []struct {
		name, cases, use string
		wantOK           bool
	}{
		{name: "the case that routed here proved it", wantOK: true,
			cases: `[{"case":"self.output.v != null","goto":"$b"},{"goto":"$c"}]`,
			use:   `outputs.a.v + 1`},
		{name: "a guard on the process input travels too", wantOK: true,
			cases: `[{"case":"input.n != null","goto":"$b"},{"goto":"$c"}]`,
			use:   `input.n + 1`},
		{name: "a conjunction proves both", wantOK: true,
			cases: `[{"case":"self.output.v != null && self.output.w != null","goto":"$b"},{"goto":"$c"}]`,
			use:   `outputs.a.v + outputs.a.w`},

		// Ordered-case negation: reaching case 1 means case 0 was FALSE. This is the
		// guard-clause shape — handle the bad case, fall through — and it gets all of its
		// narrowing from the negation.
		{name: "falling past a null check narrows the fall-through", wantOK: true,
			cases: `[{"case":"self.output.v == null","goto":"$c"},{"goto":"$b"}]`,
			use:   `outputs.a.v + 1`},

		{name: "the opposite proof does not narrow",
			cases: `[{"case":"self.output.v == null","goto":"$b"},{"goto":"$c"}]`,
			use:   `outputs.a.v + 1`},
		{name: "proving v says nothing about w",
			cases: `[{"case":"self.output.v != null","goto":"$b"},{"goto":"$c"}]`,
			use:   `outputs.a.w + 1`},
		{name: "an unguarded edge proves nothing",
			cases: `[{"goto":"$b"}]`,
			use:   `outputs.a.v + 1`},
		{name: "falling past an unguarded case proves nothing",
			cases: `[{"case":"input.n == 1","goto":"$c"},{"goto":"$b"}]`,
			use:   `outputs.a.v + 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runGenerateErr(t, twoTask(tc.cases, tc.use))
			if tc.wantOK && err != nil {
				t.Fatalf("the routing proved this: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("accepted a read nothing on this edge proved")
			}
		})
	}
}

// A refinement survives only if EVERY edge into the task establishes it. One route that
// proves nothing is enough to make the value nullable again — the task cannot know which
// edge it arrived on.
func TestGuardNarrowing_MergeNeedsEveryEdge(t *testing.T) {
	// `input.n == 1` proves non-null when TRUE and nothing when false, so the fall-through to
	// b carries no fact about n. That is what isolates the meet: a union would let a's proof
	// reach c through an edge that never established it.
	def := func(secondCase string) string {
		return `{"name":"p",
		 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
		 "tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
		   "switch":[{"case":"input.n == 1","goto":"$c"},{"goto":"$b"}]},
		  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
		   "switch":[{"case":"` + secondCase + `","goto":"$c"},{"goto":"end"}]},
		  {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},
		   "output":{"r":"$: input.n + 1"},"switch":"end"}]}`
	}
	t.Run("both edges prove it", func(t *testing.T) {
		if err := runGenerateErr(t, def("input.n != null")); err != nil {
			t.Fatalf("every edge into c proved it: %v", err)
		}
	})
	// `!= 1` proves nothing when true: not being one particular non-null value says nothing
	// about the type. So this edge reaches c having established no fact about n.
	t.Run("one edge that does not is enough to lose it", func(t *testing.T) {
		if err := runGenerateErr(t, def("input.n != 1")); err == nil {
			t.Fatal("c cannot know which edge it arrived on, and one route proved nothing")
		}
	})
}

// A loop re-enters the task that produced the output, overwriting it — so a refinement about
// `outputs.<self>` cannot survive the trip back. Without the kill a loop would keep asserting
// what only the first iteration proved.
func TestGuardNarrowing_LoopKillsItsOwnOutput(t *testing.T) {
	src := `{"name":"p",
	 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "output":{"v":"$: input.n"},
	   "switch":[{"case":"self.output.v != null","goto":"$a"},{"goto":"end"}]}]}`
	if err := runGenerateErr(t, src); err != nil {
		t.Fatalf("the loop itself must still register: %v", err)
	}

	// Reading the proof after the back edge must NOT be allowed: the task overwrote it.
	loop := `{"name":"p",
	 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "output":{"v":"$: input.n"},
	   "switch":[{"case":"self.output.v != null","goto":"$b"},{"goto":"end"}]},
	  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
	   "output":{"r":"$: outputs.a.v + 1"},
	   "switch":[{"goto":"$a"}]}]}`
	if err := runGenerateErr(t, loop); err != nil {
		t.Fatalf("b is only ever reached by the proving edge: %v", err)
	}
}

// An error edge means the task FAILED, so it produced no output and proved nothing.
func TestGuardNarrowing_ErrorEdgeCarriesNothing(t *testing.T) {
	src := `{"name":"p",
	 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "output":{"v":"$: input.n"},
	   "on_error":[{"code":["http.500"],"goto":"$b"}],
	   "switch":[{"case":"self.output.v != null","goto":"$b"},{"goto":"end"}]},
	  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
	   "output":{"r":"$: outputs.a.v + 1"},"switch":"end"}]}`
	if err := runGenerateErr(t, src); err == nil {
		t.Fatal("b is also reachable by an error edge, where a produced no output at all")
	}
}

// config is frame-invariant in NAME and re-resolved from the environment every tick, so a
// proof about it downstream is a proof about a value that may already have changed.
func TestGuardNarrowing_ConfigNeverTravels(t *testing.T) {
	src := `{"name":"p",
	 "config_schema":{"type":"object","properties":{"N":{"type":"integer"}}},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "switch":[{"case":"config.N != null","goto":"$b"},{"goto":"end"}]},
	  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
	   "output":{"r":"$: config.N + 1"},"switch":"end"}]}`
	if err := runGenerateErr(t, src); err == nil {
		t.Fatal("a config guard must not travel: the value is re-resolved every tick")
	}
}

// The negation of a conjunction is not a fact about either reference: falling past
// `a != null && b != null` tells you one of them failed, not which. The catalogue enforces it,
// and this is the edge-level pairing — the shape an author actually writes.
func TestGuardNarrowing_NegatedConjunctionProvesNothing(t *testing.T) {
	src := func(use string) string {
		return `{"name":"p",
		 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]},"m":{"type":["integer","null"]}},"required":["n","m"]},
		 "tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
		   "switch":[{"case":"input.n != null && input.m != null","goto":"$c"},{"goto":"$b"}]},
		  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
		   "output":{"r":"$: ` + use + `"},"switch":"end"},
		  {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},"switch":"end"}]}`
	}
	for _, use := range []string{"input.n + 1", "input.m + 1"} {
		t.Run("fall-through cannot read "+use, func(t *testing.T) {
			if err := runGenerateErr(t, src(use)); err == nil {
				t.Fatal("one of the two failed, and nothing says which")
			}
		})
	}
	// The TAKEN edge still proves both — the restriction is on the negation only.
	t.Run("the taken edge proves both", func(t *testing.T) {
		taken := `{"name":"p",
		 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]},"m":{"type":["integer","null"]}},"required":["n","m"]},
		 "tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
		   "switch":[{"case":"input.n != null && input.m != null","goto":"$c"},{"goto":"end"}]},
		  {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},
		   "output":{"r":"$: input.n + input.m"},"switch":"end"}]}`
		if err := runGenerateErr(t, taken); err != nil {
			t.Fatalf("both were proved on the edge that was taken: %v", err)
		}
	})
}

// A guard belongs to the switch it was written in. One task's case ordering must not supply
// negations to an edge leaving a DIFFERENT task, however similar the two look.
func TestGuardNarrowing_NoCrossEdgeAccumulation(t *testing.T) {
	// `!= 1` proves nothing when true and non-null when false, so a's two edges differ: the
	// fall-through to c carries the proof, the edge to b carries nothing. b then reaches c
	// having established nothing of its own — and a's case ordering is not b's to borrow.
	src := `{"name":"p",
	 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "switch":[{"case":"input.n != 1","goto":"$b"},{"goto":"$c"}]},
	  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
	   "switch":[{"goto":"$c"}]},
	  {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},
	   "output":{"r":"$: input.n + 1"},"switch":"end"}]}`
	if err := runGenerateErr(t, src); err == nil {
		t.Fatal("b's unguarded edge proved nothing; a's negation is not b's to lend")
	}
}

// `self.result` is the GUARDING task's, and the target has its own under that name — so a
// proof about it does not travel even when the exported output is derived from it. The two
// reads below differ only in which frame the guard was written in.
func TestGuardNarrowing_SelfResultDoesNotTravel(t *testing.T) {
	src := func(guard string) string {
		return `{"name":"p","tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
		    "responses":{"200":{"type":"object","properties":{"v":{"type":["integer","null"]}},"required":["v"]}}},
		   "output":{"v":"$: self.result.v"},
		   "switch":[{"case":"` + guard + `","goto":"$b"},{"goto":"end"}]},
		  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
		   "output":{"r":"$: outputs.a.v + 1"},"switch":"end"}]}`
	}
	t.Run("a guard on the exported output travels", func(t *testing.T) {
		if err := runGenerateErr(t, src("self.output.v != null")); err != nil {
			t.Fatalf("self.output is exactly what outputs.a names downstream: %v", err)
		}
	})
	t.Run("the same proof written on self.result does not", func(t *testing.T) {
		if err := runGenerateErr(t, src("self.result.v != null")); err == nil {
			t.Fatal("self.result names a different value in b's frame; deriving equivalence would be guessing")
		}
	})
}

// The process output is built from the terminals rather than from a task's entry context, so
// refinements do not reach it. Pinned as a LIMIT, not a claim it is right: if it is lifted,
// this is the test that says so.
func TestGuardNarrowing_ProcessOutputIsNotNarrowed(t *testing.T) {
	src := `{"name":"p",
	 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
	 "tasks":[
	  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
	   "switch":[{"case":"input.n != null","goto":"$b"},{"goto":"end"}]},
	  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},"switch":"end"}],
	 "output":{"r":"$: input.n + 1"}}`
	if err := runGenerateErr(t, src); err == nil {
		t.Fatal("if the process output now narrows, this limit was lifted — update the spec")
	}
}

// Switch cases are evaluated in order and the first match wins (`evalSwitch`), so case k runs
// only when every earlier case was false. A definition that guards a value in one case and
// reads it in the next is the shape authors write first, and refusing it sends them to a
// `?? default` that provably never evaluates.
func TestGuardNarrowing_LaterCaseSeesEarlierOnesFailing(t *testing.T) {
	// The whole output is an indexed element, so it is genuinely nullable — an empty array
	// gives null, which is what the first case is guarding.
	src := func(cases string) string {
		return `{"name":"p","tasks":[{"id":"a",
		 "action":{"type":"fetch","method":"get","url":"http://x",
		  "responses":{"200":{"type":"array","items":{"type":"object","properties":{"activated":{"type":"boolean"}},"required":["activated"]}}}},
		 "output":"$: self.result[0]",
		 "switch":` + cases + `}]}`
	}
	t.Run("the null case above narrows the one below", func(t *testing.T) {
		if err := runGenerateErr(t, src(`[
		  {"case":"self.output == null","panic":{"code":"no_results","message":"m"}},
		  {"case":"self.output.activated","goto":"end"},
		  {"goto":"end"}]`)); err != nil {
			t.Fatalf("reaching case 1 means case 0 was false: %v", err)
		}
	})
	t.Run("without the guard above it stays refused", func(t *testing.T) {
		if err := runGenerateErr(t, src(`[
		  {"case":"self.output.activated","goto":"end"},
		  {"goto":"end"}]`)); err == nil {
			t.Fatal("nothing proved the output is there")
		}
	})
	t.Run("a guard that proves the opposite does not help", func(t *testing.T) {
		if err := runGenerateErr(t, src(`[
		  {"case":"self.output != null","goto":"end"},
		  {"case":"self.output.activated","goto":"end"},
		  {"goto":"end"}]`)); err == nil {
			t.Fatal("reaching case 1 means the output IS null")
		}
	})
}

// A guard on the WHOLE output, rather than a property of it, travels the same way — the
// output of a task whose `output` is a bare expression is the value itself.
// (The `$ref` that such an output is carried as is covered by the case above, where
// `self.output` resolves through one; here `outputs.a` is inline.)
func TestGuardNarrowing_GuardOnAWholeOutput(t *testing.T) {
	src := `{"name":"p","tasks":[
	 {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
	   "responses":{"200":{"type":"array","items":{"type":"object","properties":{"activated":{"type":"boolean"}},"required":["activated"]}}}},
	  "output":"$: self.result[0]",
	  "switch":[{"case":"self.output != null","goto":"$b"},{"goto":"end"}]},
	 {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
	  "output":{"r":"$: outputs.a.activated"},"switch":"end"}]}`
	if err := runGenerateErr(t, src); err != nil {
		t.Fatalf("the edge proved the whole output is there: %v", err)
	}
}

// An `on_error` rule's predicate is `(code == a || code == b) && case`, so falling past rule j
// proves only the NEGATION of that conjunction — which is not a fact about either half, since
// the rule may have been skipped on the code before its `case` was ever evaluated. Exactly one
// shape survives: a rule with no `code` is a pure `case`, and falling past it proves it false.
func TestGuardNarrowing_OnErrorRulesNegateOnlyPureCases(t *testing.T) {
	def := func(rules string) string {
		return `{"name":"p","tasks":[
		 {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
		   "responses":{"200":{"type":"object"},"404":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}}},
		  "on_error":` + rules + `,"switch":"end"},
		 {"id":"h","switch":"end"}]}`
	}
	t.Run("a pure case above narrows the rule below", func(t *testing.T) {
		if err := runGenerateErr(t, def(`[
		  {"case":"error.data.n == null","goto":"$h"},
		  {"case":"error.data.n > 2","goto":"$h"},
		  {"goto":"$h"}]`)); err != nil {
			t.Fatalf("rule 0 has no code, so falling past it proves its case false: %v", err)
		}
	})
	t.Run("a coded rule above proves nothing", func(t *testing.T) {
		if err := runGenerateErr(t, def(`[
		  {"code":["http.404"],"case":"error.data.n == null","goto":"$h"},
		  {"case":"error.data.n > 2","goto":"$h"},
		  {"goto":"$h"}]`)); err == nil {
			t.Fatal("rule 0 may have been skipped on its CODE, before its case ran")
		}
	})
	t.Run("with no rule above it stays refused", func(t *testing.T) {
		if err := runGenerateErr(t, def(`[
		  {"case":"error.data.n > 2","goto":"$h"},
		  {"goto":"$h"}]`)); err == nil {
			t.Fatal("nothing proved the payload is there")
		}
	})
}

// A `panic` or `raise` beside a case renders only when that case MATCHED, so it reads a scope
// the case has narrowed — the expression beside it cannot, being what establishes the fact.
// Refusing this splits a guard from the message it was written to make safe.
func TestGuardNarrowing_SwitchClausesAssumeTheirCase(t *testing.T) {
	// `self.result[0]` is genuinely nullable — an empty array indexes to null — so every row
	// below turns on whether the clause may assume the guard.
	src := func(cases string) string {
		return `{"name":"p","tasks":[{"id":"a",
		 "action":{"type":"fetch","method":"get","url":"http://x",
		  "responses":{"200":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}},
		 "output":"$: self.result[0]",
		 "switch":` + cases + `}]}`
	}
	for _, tc := range []struct {
		name, cases string
		wantOK      bool
	}{
		{name: "its own guard narrows its panic data", wantOK: true,
			cases: `[{"case":"self.output != null","panic":{"code":"c","message":"m","data":{"a":"$: self.output.n + 1"}}},
			         {"goto":"end"}]`},
		{name: "and its raise message template", wantOK: true,
			cases: `[{"case":"self.output != null","raise":{"code":"c","message":"${self.output.n + 1}"}},
			         {"goto":"end"}]`},
		{name: "an earlier case's negation reaches it too", wantOK: true,
			cases: `[{"case":"self.output == null","goto":"end"},
			         {"panic":{"code":"c","message":"m","data":{"a":"$: self.output.n + 1"}}}]`},

		{name: "with nothing guarding it, it stays refused",
			cases: `[{"panic":{"code":"c","message":"m","data":{"a":"$: self.output.n + 1"}}}]`},
		{name: "a guard proving the opposite does not help",
			cases: `[{"case":"self.output == null","panic":{"code":"c","message":"m","data":{"a":"$: self.output.n + 1"}}},
			         {"goto":"end"}]`},
		// The case is what PROVES the fact, so giving it the fact would be circular: the
		// clause slot is one level down precisely so this one keeps the unnarrowed scope.
		{name: "the case expression is not narrowed by itself",
			cases: `[{"case":"self.output.n + 1 > 2","goto":"end"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runGenerateErr(t, src(tc.cases))
			if tc.wantOK && err != nil {
				t.Fatalf("the clause runs only when the case held: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("accepted a read nothing proved")
			}
		})
	}
}

// The same for `on_error`, and it is the direction priorRuleRefs cannot use: a rule's predicate
// is `(code…) && case`, whose NEGATION is a fact about neither half — but whose holding is a
// fact about both. So a rule that CAUGHT proves its case, however it is coded.
func TestGuardNarrowing_OnErrorClausesAssumeTheirCase(t *testing.T) {
	src := func(rules string) string {
		return `{"name":"p","tasks":[
		 {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
		   "responses":{"200":{"type":"object"},
		                "404":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}}},
		  "on_error":` + rules + `,"switch":"end"},
		 {"id":"h","switch":"end"}]}`
	}
	for _, tc := range []struct {
		name, rules string
		wantOK      bool
	}{
		{name: "the case narrows the retry delay beside it", wantOK: true,
			rules: `[{"code":["http.404"],"case":"error.data.n != null","retry":{"retries":2,"delay":"$: error.data.n"},"goto":"$h"}]`},
		{name: "and the panic data beside it", wantOK: true,
			rules: `[{"code":["http.404"],"case":"error.data.n != null","panic":{"code":"c","message":"m","data":{"a":"$: error.data.n + 1"}}},
			         {"code":["http.500"],"goto":"$h"}]`},

		{name: "with no case the delay stays nullable",
			rules: `[{"code":["http.404"],"retry":{"retries":2,"delay":"$: error.data.n"},"goto":"$h"}]`},
		{name: "a case proving the opposite does not help",
			rules: `[{"code":["http.404"],"case":"error.data.n == null","retry":{"retries":2,"delay":"$: error.data.n"},"goto":"$h"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runGenerateErr(t, src(tc.rules))
			if tc.wantOK && err != nil {
				t.Fatalf("the rule caught, so its case held: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("accepted a read nothing proved")
			}
		})
	}
}

// An `on_error` rule's `goto` is an edge like a switch case's, and it carries what the rule
// proved for the same reason: the rule fired, so its whole predicate held. What cannot travel
// is everything that belongs to the task that FAILED — it produced no output, and the `error`
// it caught is the target's own `last_error`, a different value under a different name.
func TestGuardNarrowing_OnErrorGotoCarriesItsCase(t *testing.T) {
	src := func(rule, use string) string {
		return `{"name":"p",
		 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
		 "tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
		    "responses":{"200":{"type":"object"},
		                 "404":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}}},
		   "output":{"v":"$: input.n"},
		   "on_error":` + rule + `,"switch":"end"},
		  {"id":"h","action":{"type":"fetch","method":"get","url":"http://z"},
		   "output":{"r":"$: ` + use + `"},"switch":"end"}]}`
	}
	for _, tc := range []struct {
		name, rule, use string
		wantOK          bool
	}{
		{name: "a guard on the process input travels", wantOK: true,
			rule: `[{"code":["http.500"],"case":"input.n != null","goto":"$h"}]`,
			use:  `input.n + 1`},

		{name: "the error it caught does not: the target reads its own last_error",
			rule: `[{"code":["http.404"],"case":"error.data.n != null","goto":"$h"}]`,
			use:  `last_error.data.n + 1`},
		{name: "nothing about the failing task's own output travels",
			rule: `[{"code":["http.500"],"case":"self.previous.v != null","goto":"$h"}]`,
			use:  `outputs.a.v + 1`},
		{name: "a rule with no case carries nothing",
			rule: `[{"code":["http.500"],"goto":"$h"}]`,
			use:  `input.n + 1`},
		{name: "the opposite proof does not narrow",
			rule: `[{"code":["http.500"],"case":"input.n == null","goto":"$h"}]`,
			use:  `input.n + 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runGenerateErr(t, src(tc.rule, tc.use))
			if tc.wantOK && err != nil {
				t.Fatalf("the rule caught, so its case held on this edge: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("accepted a read this error edge never established")
			}
		})
	}
}

// The error edge meets with every other edge into the handler, exactly as a switch edge does.
// A handler reached BOTH by a guarded rule and by an unguarded route cannot know which it
// arrived on — and what held before the task failed still holds, since failing proves nothing
// about the process input.
func TestGuardNarrowing_ErrorEdgeMeetsAndInherits(t *testing.T) {
	t.Run("a second unguarded edge loses the proof", func(t *testing.T) {
		src := `{"name":"p",
		 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
		 "tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
		   "on_error":[{"code":["http.500"],"case":"input.n != null","goto":"$h"}],
		   "switch":[{"goto":"$h"}]},
		  {"id":"h","action":{"type":"fetch","method":"get","url":"http://z"},
		   "output":{"r":"$: input.n + 1"},"switch":"end"}]}`
		if err := runGenerateErr(t, src); err == nil {
			t.Fatal("the success edge proved nothing, and h cannot know which one it took")
		}
	})
	// What was proved BEFORE the task ran is not undone by the task failing.
	t.Run("a proof from upstream survives the failure", func(t *testing.T) {
		src := `{"name":"p",
		 "input_schema":{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]},
		 "tasks":[
		  {"id":"a","action":{"type":"fetch","method":"get","url":"http://x"},
		   "switch":[{"case":"input.n != null","goto":"$b"},{"goto":"end"}]},
		  {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
		   "on_error":[{"code":["http.500"],"goto":"$h"}],"switch":"end"},
		  {"id":"h","action":{"type":"fetch","method":"get","url":"http://z"},
		   "output":{"r":"$: input.n + 1"},"switch":"end"}]}`
		if err := runGenerateErr(t, src); err != nil {
			t.Fatalf("b failing says nothing about the process input: %v", err)
		}
	})
}

// A `$ref` CHAIN, built by a definition rather than by hand: task b's `output` is task a's, so
// `b_output` is a ref to `a_output` and the null is two links away from the guard. A guard
// materializes the reference it names, and `deref` follows the whole chain in one step.
func TestGuardNarrowing_ThroughARefChain(t *testing.T) {
	src := func(cases string) string {
		return `{"name":"p","tasks":[
		 {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
		   "responses":{"200":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}},
		  "output":"$: self.result[0]","switch":[{"goto":"$b"}]},
		 {"id":"b","action":{"type":"fetch","method":"get","url":"http://y"},
		  "output":"$: outputs.a",
		  "switch":` + cases + `},
		 {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},
		  "output":{"r":"$: outputs.b.n + 1"},"switch":"end"}]}`
	}
	t.Run("the edge proves it through both refs", func(t *testing.T) {
		if err := runGenerateErr(t, src(`[{"case":"self.output != null","goto":"$c"},{"goto":"end"}]`)); err != nil {
			t.Fatalf("a re-exported output is a ref to a ref, and the guard names it: %v", err)
		}
	})
	t.Run("without the guard it stays refused", func(t *testing.T) {
		if err := runGenerateErr(t, src(`[{"goto":"$c"},{"goto":"end"}]`)); err == nil {
			t.Fatal("nothing proved the re-exported output is there")
		}
	})
	// The chain itself, so a solver change that inlines `b_output` is visible here rather than
	// only in whatever it breaks downstream.
	out := runGenerate(t, src(`[{"case":"self.output != null","goto":"$c"},{"goto":"end"}]`))
	if got := mustMarshal(defOf(out, "b_output")); got != `{"$ref":"#/$defs/a_output"}` {
		t.Errorf("b_output = %s, want a bare ref to a_output", got)
	}
}

// `outputs.a ?? outputs.b` over two nullable outputs puts the null inside a `$ref` that is an
// ARM of a union — the shape the unit test in schematest pins, here shown to be something a
// definition produces. What a reader is told about it is the whole point: the type is right,
// and the summary beside it has to agree.
func TestGuardNarrowing_CoalesceOfTwoNullableOutputs(t *testing.T) {
	src := `{"name":"p","tasks":[
	 {"id":"a","action":{"type":"fetch","method":"get","url":"http://x",
	   "responses":{"200":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}},
	  "output":"$: self.result[0]","switch":[{"goto":"$b"}]},
	 {"id":"b","action":{"type":"fetch","method":"get","url":"http://y",
	   "responses":{"200":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}}}},
	  "output":"$: self.result[0]","switch":[{"goto":"$c"}]},
	 {"id":"c","action":{"type":"fetch","method":"get","url":"http://z"},
	  "output":{"seen":"$: 1"},"switch":[{"goto":"end"}]}],
	 "output":{"r":"$: outputs.a ?? outputs.b"}}`
	out := runGenerate(t, src)
	at, err := out.ProcessOutput.WithDefs(out.Defs).At("r")
	if err != nil {
		t.Fatalf("At(r): %v", err)
	}
	if !at.HasNull() {
		t.Error("the recovery is still nullable — its right arm is a nullable output")
	}
	if got := at.Summary(); got != "object{n}|null" {
		t.Errorf("Summary = %q, want %q — the published contract reads this", got, "object{n}|null")
	}

	// The hover's own path, which does NOT navigate: it infers the expression in the slot's
	// context and summarises what comes back. Resolution reads the pool off the ROOT node, and
	// a union built by inference has to carry it up from its arms — `At` above attaches one on
	// the way down and would hide a union that lost it.
	ctx := slotContext(t, src, "tasks.c.output")
	inferred, err := ctx.Infer("outputs.a ?? outputs.b")
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	if !inferred.HasNull() {
		t.Error("the union came back non-null: its arms hold the pool and its root does not")
	}
	if got := inferred.Summary(); got != "object{n}|null" {
		t.Errorf("hover would print %q, want %q", got, "object{n}|null")
	}
}
