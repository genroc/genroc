package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// noEval is the evaluator an all-literal policy must never reach: doing so means a slot was
// misclassified as an expression.
func noEval(string) (any, error) { return nil, errors.New("no expression slot expected") }

// SaveDefinition stores json.Marshal of the decoded struct, so whatever these produce is
// what every later reader — and every later decode — sees.
func TestRetry_MarshalsCanonically(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{
			// The scalar is a spelling, not a second policy shape: it must not survive
			// into storage, or downstream readers need to handle both.
			name: "scalar desugars to the object form",
			json: `3`,
			want: `{"attempts":3}`,
		},
		{
			// Preserved as authored rather than normalised to 90000, so a definition reads
			// back the way it was written.
			name: "a duration keeps its literal",
			json: `{"attempts":2,"delay":"90s"}`,
			want: `{"attempts":2,"delay":"90s"}`,
		},
		{
			name: "milliseconds stay a number",
			json: `{"attempts":2,"delay":1500}`,
			want: `{"attempts":2,"delay":1500}`,
		},
		{
			name: "every slot round-trips",
			json: `{"attempts":4,"delay":"30s","factor":3,"max_delay":"10m"}`,
			want: `{"attempts":4,"delay":"30s","factor":3,"max_delay":"10m"}`,
		},
		{
			name: "absent marshals away entirely",
			json: `null`,
			want: `null`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Retry
			if err := r.UnmarshalJSON([]byte(tt.json)); err != nil {
				t.Fatalf("decode %s: %v", tt.json, err)
			}
			got, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("%s round-tripped to %s, want %s", tt.json, got, tt.want)
			}
		})
	}
}

func TestRetry_RejectsIncoherentPolicies(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{"unknown key", `{"attempts":3,"backoff":"30s"}`, `unknown field "backoff"`},
		{"shrinking factor", `{"attempts":3,"factor":0.5}`, "shrink the wait"},
		{"calendar unit", `{"attempts":3,"delay":"1d"}`, "calendar units"},
		{"zero delay", `{"attempts":3,"delay":0}`, "must be positive"},
		{"negative delay", `{"attempts":3,"delay":-5}`, "must be positive"},
		{"negative attempts", `{"attempts":-1}`, "must not be negative"},
		{"fractional attempts", `1.5`, "whole number"},
		{"unitless duration", `{"attempts":3,"delay":"30"}`, "has no unit"},
		// A duration that wraps int64 nanoseconds is refused by the shared grammar rather
		// than arriving here as a plausible small positive value.
		{"out-of-range duration", `{"attempts":3,"delay":"5124096h"}`, "out of range"},
		// The shorthand is typed `integer` in the published schema, so the decoder must not
		// be looser than the schema an editor validates against.
		{"quoted attempts", `"3"`, "quoted"},
		{"boolean", `true`, "must be a number of attempts or an object"},
		{"array", `[]`, "must be a number of attempts or an object"},
		{"non-scalar duration", `{"attempts":3,"delay":{}}`, "expected a duration string, a number of milliseconds, or a $: expression"},
		{"boolean duration", `{"attempts":3,"delay":true}`, "expected a duration string, a number of milliseconds, or a $: expression"},
		{"fractional milliseconds", `{"attempts":3,"delay":1.5}`, "whole number of milliseconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Retry
			err := (&r).UnmarshalJSON([]byte(tt.json))
			if err == nil {
				t.Fatalf("accepted %s", tt.json)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// ParseRetryDuration is exported for definitions built in Go, which hand it native types
// rather than the json.Number every decoded value arrives as.
func TestParseRetryDuration_AcceptsNativeGoNumbers(t *testing.T) {
	for _, v := range []any{1500, int(1500), float64(1500)} {
		d, err := ParseRetryDuration(v)
		if err != nil {
			t.Fatalf("ParseRetryDuration(%v): %v", v, err)
		}
		if d.Duration() != 1500*time.Millisecond {
			t.Fatalf("ParseRetryDuration(%v) = %v, want 1.5s", v, d.Duration())
		}
	}
	if _, err := ParseRetryDuration(1.5); err == nil {
		t.Fatal("a fractional millisecond count must be refused, not truncated")
	}
	if _, err := ParseRetryDuration(true); err == nil {
		t.Fatal("a non-numeric, non-string value must be refused")
	}
}

// An empty policy is the absent key, not a present-but-blank one. It is what keeps
// `retry: 0` — the long-standing spelling of "this rule caps retries at none" — legal, and
// it is why D7's child-task rejection can gate on IsZero without refusing that.
func TestRetry_EmptyPolicyIsAbsent(t *testing.T) {
	for _, s := range []string{`{}`, `0`, `null`} {
		var r Retry
		if err := r.UnmarshalJSON([]byte(s)); err != nil {
			t.Fatalf("decode %s: %v", s, err)
		}
		if !r.IsZero() {
			t.Fatalf("%s decoded to a non-empty policy %+v", s, r)
		}
		if err := validateRetry(r, "call", "on_error[0]"); err != nil {
			t.Fatalf("%s must validate as an absent policy: %v", s, err)
		}
	}
}

// The decoder cannot see these — one is only wrong relative to another slot, and one is a
// policy that decodes cleanly and then never runs.
func TestValidateRetry_RejectsIncoherentCombinations(t *testing.T) {
	mustDur := func(v any) RetryDuration {
		d, err := ParseRetryDuration(v)
		if err != nil {
			t.Fatalf("ParseRetryDuration(%v): %v", v, err)
		}
		return d
	}
	tests := []struct {
		name    string
		retry   Retry
		wantErr string
	}{
		{
			name:    "ceiling below the base",
			retry:   Retry{Attempts: RetryCount(3), Delay: mustDur("10m"), MaxDelay: mustDur("30s")},
			wantErr: "shorter than retry.delay",
		},
		{
			name:    "a curve with no attempts",
			retry:   Retry{Delay: mustDur("30s")},
			wantErr: "never retry",
		},
		// These two the decoder rejects first, so only a definition built in Go can carry
		// them here — which is the whole reason this check is duplicated at registration.
		{
			name:    "negative attempts",
			retry:   Retry{Attempts: RetryCount(-1)},
			wantErr: "must not be negative",
		},
		{
			name:    "shrinking factor",
			retry:   Retry{Attempts: RetryCount(3), Factor: RetryNumber{n: 0.5}},
			wantErr: "shrink the wait",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRetry(tt.retry, "call", "on_error[0]")
			if err == nil {
				t.Fatalf("accepted %+v", tt.retry)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}

	// The pairing is only rejected when both slots are authored: an explicit delay above
	// the *default* ceiling widens the ceiling instead (Retry.Ceiling), which is what keeps
	// a lone `delay` from being silently clamped back.
	ok := Retry{Attempts: RetryCount(3), Delay: mustDur("1h")}
	if err := validateRetry(ok, "call", "on_error[0]"); err != nil {
		t.Fatalf("a delay longer than the default ceiling must be legal on its own: %v", err)
	}
	resolved, err := ok.Resolve(noEval)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Ceiling != time.Hour {
		t.Fatalf("ceiling = %v, want 1h", resolved.Ceiling)
	}
}

// A "$:" slot has no value at registration, so the decoder must keep it as a source rather
// than coercing it to a number — a slot that decoded to 0 is a policy that never retries.
func TestRetry_DecodesExpressionSlots(t *testing.T) {
	var r Retry
	src := `{"attempts":"$: config.retry_attempts","delay":"$: config.retry_delay_ms","factor":"$: config.retry_factor","max_delay":"$: config.retry_max_delay_ms"}`
	if err := json.Unmarshal([]byte(src), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, slot := range []struct {
		name   string
		isExpr bool
		expr   string
	}{
		{"attempts", r.Attempts.IsExpr(), r.Attempts.Expr()},
		{"delay", r.Delay.IsExpr(), r.Delay.Expr()},
		{"factor", r.Factor.IsExpr(), r.Factor.Expr()},
		{"max_delay", r.MaxDelay.IsExpr(), r.MaxDelay.Expr()},
	} {
		if !slot.isExpr {
			t.Fatalf("%s decoded as a literal; an unresolved slot reads as 0, which is a policy that never retries", slot.name)
		}
		if !strings.HasPrefix(slot.expr, "$:") {
			t.Fatalf("%s kept %q, want the $: source", slot.name, slot.expr)
		}
	}
	if r.IsZero() {
		t.Fatal("an all-expression policy reads as absent, so validateOnError would skip every check on it")
	}

	// SaveDefinition stores the marshalled struct: an expression that did not round-trip
	// would be re-read as a literal or lost outright.
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Retry
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-decode %s: %v", out, err)
	}
	if back.Attempts.Expr() != "$: config.retry_attempts" || back.Delay.Expr() != "$: config.retry_delay_ms" {
		t.Fatalf("round-trip lost the sources: %s", out)
	}
}

// "${ }" produces a string at runtime, so it is refused by name in every numeric slot —
// the same failure the delay grammar removes.
func TestRetry_RejectsInterpolationAndLiteralStrings(t *testing.T) {
	for _, tt := range []struct{ name, src, want string }{
		{"interpolated attempts", `{"attempts":"${ config.n }"}`, "not a number"},
		{"interpolated factor", `{"attempts":2,"factor":"${ config.f }"}`, "not a number"},
		{"quoted number", `{"attempts":"3"}`, "not a number"},
		{"scalar shorthand cannot be an expression", `"$: config.n"`, "the attempt count is a bare number"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var r Retry
			err := json.Unmarshal([]byte(tt.src), &r)
			if err == nil {
				t.Fatalf("accepted %s as %+v", tt.src, r)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// Resolve is where an expression slot is finally judged, so it must repeat every bound
// validateRetry applies to a literal — otherwise config supplies what the grammar refuses.
func TestRetry_ResolveAppliesBoundsAndDefaults(t *testing.T) {
	expr := func(src string) Retry {
		var r Retry
		if err := json.Unmarshal([]byte(src), &r); err != nil {
			t.Fatalf("decode %s: %v", src, err)
		}
		return r
	}
	eval := func(v any) func(string) (any, error) {
		return func(string) (any, error) { return v, nil }
	}

	t.Run("an expression supplies the curve", func(t *testing.T) {
		r := expr(`{"attempts":"$: config.n","delay":"$: config.d"}`)
		got, err := r.Resolve(eval(json.Number("2500")))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Attempts != 2500 || got.Base != 2500*time.Millisecond {
			t.Fatalf("resolved to %+v, want attempts 2500 and a 2.5s base", got)
		}
		// An unset slot still defaults, exactly as it does for a literal policy.
		if got.Factor != DefaultRetryFactor {
			t.Fatalf("factor = %v, want the %v default", got.Factor, DefaultRetryFactor)
		}
	})

	for _, tt := range []struct {
		name, src string
		val       any
		want      string
	}{
		{"a fractional attempt count", `{"attempts":"$: config.n"}`, 2.5, "not a whole number of attempts"},
		{"a negative attempt count", `{"attempts":"$: config.n"}`, -1, "must not be negative"},
		{"a shrinking factor", `{"attempts":2,"factor":"$: config.f"}`, 0.5, "shrink the wait"},
		{"a non-number", `{"attempts":"$: config.n"}`, "lots", "must evaluate to a number"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := expr(tt.src).Resolve(eval(tt.val))
			if err == nil {
				t.Fatalf("resolve accepted %v", tt.val)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}

	// The pairing check has to survive both slots being expressions, since that is the one
	// arrangement registration cannot judge at all.
	both := expr(`{"attempts":2,"delay":"$: config.d","max_delay":"$: config.m"}`)
	n := 0
	_, err := both.Resolve(func(string) (any, error) {
		n++
		if n == 1 {
			return 600000, nil // delay: 10m
		}
		return 30000, nil // max_delay: 30s
	})
	if err == nil || !strings.Contains(err.Error(), "shorter than retry.delay") {
		t.Fatalf("a ceiling under the base resolved without complaint: %v", err)
	}
}

// An expression cannot be read as an attempt count at registration, so the only_once tiers
// must treat it as one — reading it as 0 would wave a catch-all rule straight through.
func TestValidateOnError_ExpressionAttemptsKeepsOnlyOnceTiers(t *testing.T) {
	var r Retry
	if err := json.Unmarshal([]byte(`{"attempts":"$: config.n"}`), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	yes := true
	task := &Task{
		ID:       "call",
		Action:   &Action{Type: ActionTypeFetch, URL: "http://x"},
		OnlyOnce: &yes,
		OnError:  []ErrorCase{{Retry: r}},
		Switch:   SwitchMap{{Goto: GotoEnd}},
	}
	err := validateOnError(task, map[string]struct{}{"call": {}})
	if err == nil {
		t.Fatal("a catch-all with expression-valued attempts was accepted on an only_once task")
	}
	if !strings.Contains(err.Error(), "catch-all rule cannot have retries") {
		t.Fatalf("error %q is not the catch-all tier message", err)
	}
}
