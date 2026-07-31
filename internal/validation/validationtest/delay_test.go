package validationtest

import (
	"strings"
	"testing"
)

// delayDef builds a one-task definition whose delay carries the given slot and raw JSON
// value, so both the string and bare-number forms can be exercised.
func delayDef(slot, valueJSON string) string {
	return `{
		"name": "delay-slots",
		"input_schema": {"type":"object","properties":{"n":{"type":"integer"},"tags":{"type":"array","items":{"type":"string"}}},"required":["n","tags"]},
		"tasks": [
			{"id": "wait", "action": {"type": "delay", "` + slot + `": ` + valueJSON + `}, "switch": "end"}
		]
	}`
}

// A delay slot's accepted type depends on how it is written, and that split is decided
// syntactically before inference runs: a pure literal parses against the delayspec
// grammar, a $: leaf must infer to a number, and a ${ } interpolation is rejected.
func TestGenerate_DelaySlots_Accepted(t *testing.T) {
	for _, tc := range []struct{ slot, value string }{
		// Literals, parsed at registration.
		{"for", `"2h30m"`},
		{"for", `"1d 12h"`},
		{"for", `"3mo"`},
		{"for", `"500ms"`},
		{"until", `"+2d 08:00"`},
		{"until", `"*-*-01 08:00"`},
		{"until", `"mon 09:00"`},
		{"until", `"2026-09-01T08:00:00+02:00"`},
		{"until", `"2026-09-01 08:00"`},
		// Bare numbers: milliseconds (for) and unix milliseconds (until).
		{"for", `5000`},
		{"until", `1789000000000`},
		// $: expressions inferring to a number.
		{"for", `"$: input.n * 2"`},
		{"until", `"$: input.n"`},
	} {
		if err := runGenerateErr(t, delayDef(tc.slot, tc.value)); err != nil {
			t.Errorf("%s: %s should be accepted: %v", tc.slot, tc.value, err)
		}
	}
}

func TestGenerate_DelaySlots_Rejected(t *testing.T) {
	for _, tc := range []struct{ slot, value, want string }{
		// The unitless string is the ambiguity the syntax exists to remove.
		{"for", `"5000"`, "no unit"},
		{"for", `"2x"`, "unknown unit"},
		{"for", `""`, "empty"},
		// An expression that cannot be a duration.
		{"for", `"$: input.tags"`, "number of milliseconds"},
		{"until", `"$: input.tags"`, "number of unix milliseconds"},
		// An undefined reference must not slip to runtime.
		{"for", `"$: input.nope"`, ""},
		// Instant grammar: no natural language, and no impossible calendar date.
		{"until", `"in two days"`, ""},
		{"until", `"*-02-30 08:00"`, "no day 30"},
		{"until", `"*-*-01"`, ""},
	} {
		err := runGenerateErr(t, delayDef(tc.slot, tc.value))
		if err == nil {
			t.Errorf("%s: %s should be rejected", tc.slot, tc.value)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %s error = %q; want it to mention %q", tc.slot, tc.value, err, tc.want)
		}
	}
}

// A ${ } interpolation produces a string at runtime — precisely the failure this syntax
// removes — so it is rejected by name rather than left to fall through.
func TestGenerate_DelaySlots_RejectsInterpolationByName(t *testing.T) {
	for _, tc := range []struct{ slot, value string }{
		{"for", `"${ input.n }h"`},
		{"for", `"${ input.n }"`},
		{"until", `"${ input.n }"`},
	} {
		err := runGenerateErr(t, delayDef(tc.slot, tc.value))
		if err == nil {
			t.Fatalf("%s: %s should be rejected", tc.slot, tc.value)
		}
		msg := err.Error()
		if !strings.Contains(msg, "${ }") {
			t.Errorf("%s: %s error = %q; it should name the ${ } form", tc.slot, tc.value, msg)
		}
		// The message must point at both forms that do work.
		if !strings.Contains(msg, "$:") {
			t.Errorf("%s: %s error = %q; it should point at the $: form", tc.slot, tc.value, msg)
		}
	}
}
