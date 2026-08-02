package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

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
		{"non-scalar duration", `{"attempts":3,"delay":{}}`, "expected a duration string or a number"},
		{"boolean duration", `{"attempts":3,"delay":true}`, "expected a duration string or a number"},
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
			retry:   Retry{Attempts: 3, Delay: mustDur("10m"), MaxDelay: mustDur("30s")},
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
			retry:   Retry{Attempts: -1},
			wantErr: "must not be negative",
		},
		{
			name:    "shrinking factor",
			retry:   Retry{Attempts: 3, Factor: 0.5},
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
	ok := Retry{Attempts: 3, Delay: mustDur("1h")}
	if err := validateRetry(ok, "call", "on_error[0]"); err != nil {
		t.Fatalf("a delay longer than the default ceiling must be legal on its own: %v", err)
	}
	if ok.Ceiling() != time.Hour {
		t.Fatalf("ceiling = %v, want 1h", ok.Ceiling())
	}
}
