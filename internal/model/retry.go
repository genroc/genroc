package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"genroc/internal/delayspec"
)

// The curve a Retry falls back to slot by slot: 1s, doubling, ceiling 5m — what Temporal
// and Step Functions default to. The ceiling is deliberately absolute rather than relative
// to the base the way Temporal's is: theirs is bounded by a wall-clock activity timeout,
// and genroc has no such budget behind it. See specs/retry-policy.md.
const (
	DefaultRetryDelay    = 1 * time.Second
	DefaultRetryFactor   = 2.0
	DefaultRetryMaxDelay = 5 * time.Minute
)

// Retry is an on_error rule's retry policy: how many attempts to make, and the backoff
// curve between them. Two wire forms, the scalar desugaring to `attempts`:
//
//	retry: 3
//	retry: {attempts: 5, delay: "30s", factor: 2, max_delay: "1h"}
//
// Every timing slot is optional; read them through Base/Growth/Ceiling, never directly,
// or an unset slot is a zero-length wait instead of its default.
//
// Nothing may embed this type: an UnmarshalJSON on an embedded struct is promoted to the
// outer one and silently eats the whole object (see Timeout, and the DelaySpec note in
// CLAUDE.md).
type Retry struct {
	Attempts int
	Delay    RetryDuration
	Factor   float64
	MaxDelay RetryDuration
}

// RetryAttempts is the Go spelling of the scalar shorthand, for definitions built in code
// rather than decoded from JSON.
func RetryAttempts(n int) Retry { return Retry{Attempts: n} }

func (r Retry) IsZero() bool {
	return r.Attempts == 0 && r.Factor == 0 && r.Delay.IsZero() && r.MaxDelay.IsZero()
}

// Base is the wait before the first retry.
func (r Retry) Base() time.Duration {
	if d := r.Delay.Duration(); d > 0 {
		return d
	}
	return DefaultRetryDelay
}

// Growth is the multiplier applied to the wait after each further attempt.
func (r Retry) Growth() float64 {
	if r.Factor > 0 {
		return r.Factor
	}
	return DefaultRetryFactor
}

// Ceiling is the longest wait the curve may reach. The default one never truncates an
// authored base — a `delay: 1h` under the 5m default would otherwise be clamped back to
// 5m, undoing the only slot the author actually set.
func (r Retry) Ceiling() time.Duration {
	if d := r.MaxDelay.Duration(); d > 0 {
		return d
	}
	return max(DefaultRetryMaxDelay, r.Base())
}

var retryFields = map[string]bool{"attempts": true, "delay": true, "factor": true, "max_delay": true}

// retryWire is the object form, shared by Retry's MarshalJSON and UnmarshalJSON so the
// tags stay in lockstep.
type retryWire struct {
	Attempts int           `json:"attempts,omitempty"`
	Delay    RetryDuration `json:"delay,omitempty,omitzero"`
	Factor   float64       `json:"factor,omitempty"`
	MaxDelay RetryDuration `json:"max_delay,omitempty,omitzero"`
}

// MarshalJSON always writes the object form, including for a policy that arrived as the
// scalar shorthand: one shape downstream, and a stored definition that is canonical.
func (r Retry) MarshalJSON() ([]byte, error) {
	if r.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(retryWire{Attempts: r.Attempts, Delay: r.Delay, Factor: r.Factor, MaxDelay: r.MaxDelay})
}

func (r *Retry) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*r = Retry{}
		return nil
	}
	if data[0] != '{' {
		// json.Number accepts a quoted number, which would let `retry: "3"` through — but
		// the published schema types the shorthand as an integer, so an editor flags what
		// the server would take. The rest of the grammar refuses quoted numbers too.
		if data[0] == '"' {
			return fmt.Errorf("retry: %s is quoted; the attempt count is a bare number", data)
		}
		var n json.Number
		if err := json.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("retry: must be a number of attempts or an object, got %s", data)
		}
		attempts, err := n.Int64()
		if err != nil || attempts < 0 {
			return fmt.Errorf("retry: %s is not a whole number of attempts", n)
		}
		*r = Retry{Attempts: int(attempts)}
		return nil
	}
	// A typo'd key here is silent in the worst way: the rule keeps its `code` and its
	// `goto`, so it still matches and still routes — it just never retries.
	if err := rejectUnknownFields("retry", data, retryFields); err != nil {
		return err
	}
	var w retryWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Attempts < 0 {
		return fmt.Errorf("retry: attempts must not be negative")
	}
	if w.Factor != 0 && w.Factor < 1 {
		return fmt.Errorf("retry: factor %g would shrink the wait after every attempt; use 1 for a constant delay", w.Factor)
	}
	*r = Retry{Attempts: w.Attempts, Delay: w.Delay, Factor: w.Factor, MaxDelay: w.MaxDelay}
	return nil
}

// JSONSchemaBytes returns the JSON Schema for Retry so that OpenAPI reflection produces its
// two wire forms rather than the flattened struct.
func (Retry) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{
				"type": "integer",
				"minimum": 0,
				"description": "Shorthand for 'attempts': the number of retries before following goto or failing, on the default backoff curve (1s, doubling, capped at 5m)."
			},
			{
				"type": "object",
				"description": "The long form, naming the attempt count and any part of the backoff curve to override.",
				"properties": {
					"attempts":  {"type": "integer", "minimum": 0, "description": "Number of retries before following goto or failing. 0 = no retries."},
					"delay":     {"type": ["string", "number"], "description": "Wait before the first retry: a fixed duration such as \"30s\" or \"2h30m\" (units ms, s, m, h — calendar units d/w/mo/y are not accepted, since the curve scales this value), or a bare number of milliseconds. Defaults to 1s."},
					"factor":    {"type": "number", "minimum": 1, "description": "Multiplier applied to the wait after each further attempt. 1 keeps the delay constant. Defaults to 2."},
					"max_delay": {"type": ["string", "number"], "description": "Ceiling the growing wait is clamped to, in the same grammar as 'delay'. Defaults to 5m, or to 'delay' when that is longer. Must not be shorter than 'delay'."}
				},
				"additionalProperties": false
			}
		]
	}`), nil
}

// RetryDuration is a fixed duration in a retry policy: "30s", "2h30m", or a bare number of
// milliseconds. It keeps the literal it was written as so a stored definition round-trips
// as authored.
//
// Calendar units are refused rather than resolved: the backoff curve scales this value and
// compares it against a ceiling, and "1mo" is not a length until a timezone and a start
// instant say so.
type RetryDuration struct {
	src any
	d   time.Duration
}

// ParseRetryDuration accepts the wire forms of a retry duration — a duration string or a
// number of milliseconds — and is the only place either is turned into a length.
func ParseRetryDuration(v any) (RetryDuration, error) {
	switch x := v.(type) {
	case string:
		parsed, err := delayspec.ParseDuration(x)
		if err != nil {
			return RetryDuration{}, err
		}
		fixed, ok := parsed.Fixed()
		if !ok {
			return RetryDuration{}, fmt.Errorf("duration %q: calendar units (d, w, mo, y) are not accepted in a retry policy, because the curve scales the value; use fixed units instead — \"1d\" is spelled \"24h\"", x)
		}
		if fixed <= 0 {
			return RetryDuration{}, fmt.Errorf("duration %q must be positive", x)
		}
		return RetryDuration{src: x, d: fixed}, nil
	case json.Number:
		ms, err := x.Int64()
		if err != nil {
			return RetryDuration{}, fmt.Errorf("duration %s: expected a whole number of milliseconds", x)
		}
		if ms <= 0 {
			return RetryDuration{}, fmt.Errorf("duration %s must be positive", x)
		}
		return RetryDuration{src: ms, d: time.Duration(ms) * time.Millisecond}, nil
	case int:
		return ParseRetryDuration(json.Number(fmt.Sprint(x)))
	case float64:
		return ParseRetryDuration(json.Number(fmt.Sprint(x)))
	default:
		return RetryDuration{}, fmt.Errorf("expected a duration string or a number of milliseconds, got %T", v)
	}
}

func (d RetryDuration) IsZero() bool { return d.src == nil }

// Duration is 0 when the slot is absent, which is why Retry's Base/Ceiling read it through
// a default rather than using it directly.
func (d RetryDuration) Duration() time.Duration { return d.d }

func (d RetryDuration) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.src)
}

func (d *RetryDuration) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*d = RetryDuration{}
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	parsed, err := ParseRetryDuration(v)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// JSONSchemaBytes returns the JSON Schema for RetryDuration, which reflection would
// otherwise render as an empty object — the type's only fields are unexported.
func (RetryDuration) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"type": ["string", "number"],
		"description": "A fixed duration: \"30s\", \"2h30m\" (units ms, s, m, h), or a bare number of milliseconds."
	}`), nil
}
