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

// timeoutDef builds a one-task definition carrying the given raw JSON timeout. The action
// is external, the one type that accepts both slots, so the grammar can be exercised
// without the action-type rules (tested in internal/model) getting in the way.
func timeoutDef(valueJSON string) string {
	return `{
		"name": "timeout-slots",
		"input_schema": {"type":"object","properties":{"n":{"type":"integer"},"tags":{"type":"array","items":{"type":"string"}}},"required":["n","tags"]},
		"tasks": [
			{"id": "park", "action": {"type": "external"}, "timeout": ` + valueJSON + `, "switch": "end"}
		]
	}`
}

// A timeout is the delay slots aimed at a deadline, so it inherits their whole
// classification — including the shorthand, which desugars to `for` before any of it runs.
func TestGenerate_TimeoutSlots_Accepted(t *testing.T) {
	for _, value := range []string{
		// Shorthand: a literal duration, a bare number of milliseconds, an expression.
		`"30s"`,
		`"2h30m"`,
		`5000`,
		`"$: input.n"`,
		// Long form, both slots plus tz.
		`{"for": "1d", "tz": "Europe/Prague"}`,
		`{"until": "fri 17:00", "tz": "Europe/Prague"}`,
		`{"until": "2026-09-01T08:00:00+02:00"}`,
		`{"until": "$: input.n"}`,
	} {
		if err := runGenerateErr(t, timeoutDef(value)); err != nil {
			t.Errorf("timeout %s should be accepted: %v", value, err)
		}
	}
}

func TestGenerate_TimeoutSlots_Rejected(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		// The shorthand is the `for` grammar, so it inherits the unitless rejection.
		{`"5000"`, "no unit"},
		{`"2x"`, "unknown unit"},
		{`{"for": "5000"}`, "no unit"},
		// An expression that cannot be a duration, and one that resolves to nothing.
		{`"$: input.tags"`, "number of milliseconds"},
		{`{"until": "$: input.tags"}`, "number of unix milliseconds"},
		{`"$: input.nope"`, ""},
		// An interpolation yields a string at runtime, here as everywhere.
		{`"${ input.n }s"`, "${ }"},
		// The arity rule the two slots share, and the tz rule.
		{`{"for": "1h", "until": "fri 17:00"}`, "mutually exclusive"},
		{`{"tz": "Europe/Prague"}`, "one of for or until is required"},
		{`{"for": "1d", "tz": "CET"}`, "abbreviations"},
		// An unknown key is rejected a layer earlier, at decode, so it never reaches
		// validation — see TestTimeout_DecodeForms in internal/model.
	} {
		err := runGenerateErr(t, timeoutDef(tc.value))
		if err == nil {
			t.Errorf("timeout %s should be rejected", tc.value)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("timeout %s error = %q; want it to mention %q", tc.value, err, tc.want)
		}
	}
}

// The label must say which construct the slot belongs to. A timeout error reading "delay
// for" would send the author to the wrong line — the two share a checker, not a name.
func TestGenerate_TimeoutSlots_ErrorNamesTheTimeout(t *testing.T) {
	err := runGenerateErr(t, timeoutDef(`"2x"`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q; it should name the timeout slot", err)
	}
	if strings.Contains(err.Error(), "delay") {
		t.Errorf("error = %q; it must not call a timeout a delay", err)
	}
}
