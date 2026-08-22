package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"genroc/internal/delayspec"
	"genroc/internal/template"
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
// curve between them. Three wire forms, the scalar desugaring to `attempts`:
//
//	retry: 3
//	retry: {attempts: 5, delay: "30s", factor: 2, max_delay: "1h"}
//	retry: {attempts: "$: config.retry_attempts", delay: "$: config.retry_delay_ms"}
//
// Every slot is optional and every slot also accepts a "$:" expression, which has no value
// until the rule fires. Nothing may read a slot for its number: call Resolve once per error
// and read the ResolvedRetry it returns, which carries the defaults already applied.
//
// Nothing may embed this type: an UnmarshalJSON on an embedded struct is promoted to the
// outer one and silently eats the whole object (see Timeout, and the DelaySpec note in
// CLAUDE.md).
type Retry struct {
	Attempts RetryNumber
	Delay    RetryDuration
	Factor   RetryNumber
	MaxDelay RetryDuration
}

// RetryAttempts is the Go spelling of the scalar shorthand, for definitions built in code
// rather than decoded from JSON.
func RetryAttempts(n int) Retry { return Retry{Attempts: RetryCount(n)} }

func (r Retry) IsZero() bool {
	return r.Attempts.IsZero() && r.Factor.IsZero() && r.Delay.IsZero() && r.MaxDelay.IsZero()
}

// ResolvedRetry is a Retry with every slot reduced to a number and every default already
// applied — the only form the attempt comparison and the backoff curve read.
type ResolvedRetry struct {
	Attempts int
	Base     time.Duration
	Factor   float64
	Ceiling  time.Duration
}

// Resolve reduces every slot to a number, evaluating the "$:" ones through eval. The bounds
// validateRetry checks at registration are re-checked here with the same wording, because a
// slot that is an expression has no value to check until now.
func (r Retry) Resolve(eval func(expr string) (any, error)) (ResolvedRetry, error) {
	attempts, _, err := r.Attempts.resolve(eval)
	if err != nil {
		return ResolvedRetry{}, fmt.Errorf("retry.attempts: %w", err)
	}
	if attempts != float64(int(attempts)) {
		return ResolvedRetry{}, fmt.Errorf("retry.attempts: %v is not a whole number of attempts", attempts)
	}
	if attempts < 0 {
		return ResolvedRetry{}, fmt.Errorf("retry.attempts must not be negative, got %v", attempts)
	}

	factor, factorSet, err := r.Factor.resolve(eval)
	if err != nil {
		return ResolvedRetry{}, fmt.Errorf("retry.factor: %w", err)
	}
	if !factorSet {
		factor = DefaultRetryFactor
	} else if factor < 1 {
		return ResolvedRetry{}, fmt.Errorf("retry.factor %g would shrink the wait after every attempt; use 1 for a constant delay", factor)
	}

	base, baseSet, err := r.Delay.resolve(eval)
	if err != nil {
		return ResolvedRetry{}, fmt.Errorf("retry.delay: %w", err)
	}
	if !baseSet {
		base = DefaultRetryDelay
	} else if base <= 0 {
		return ResolvedRetry{}, fmt.Errorf("retry.delay must be positive, got %s", base)
	}

	ceiling, ceilingSet, err := r.MaxDelay.resolve(eval)
	if err != nil {
		return ResolvedRetry{}, fmt.Errorf("retry.max_delay: %w", err)
	}
	switch {
	case !ceilingSet:
		// The default ceiling never truncates an authored base — a `delay: 1h` under the 5m
		// default would otherwise be clamped back to 5m, undoing the only slot set.
		ceiling = max(DefaultRetryMaxDelay, base)
	case ceiling <= 0:
		return ResolvedRetry{}, fmt.Errorf("retry.max_delay must be positive, got %s", ceiling)
	case ceiling < base:
		return ResolvedRetry{}, fmt.Errorf("retry.max_delay (%s) is shorter than retry.delay (%s), so the first wait would already be clamped and the delay never applied", ceiling, base)
	}

	return ResolvedRetry{Attempts: int(attempts), Base: base, Factor: factor, Ceiling: ceiling}, nil
}

var retryFields = map[string]bool{"attempts": true, "delay": true, "factor": true, "max_delay": true}

// retryWire is the object form, shared by Retry's MarshalJSON and UnmarshalJSON so the
// tags stay in lockstep.
type retryWire struct {
	Attempts RetryNumber   `json:"attempts,omitempty,omitzero"`
	Delay    RetryDuration `json:"delay,omitempty,omitzero"`
	Factor   RetryNumber   `json:"factor,omitempty,omitzero"`
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
			return fmt.Errorf("retry: %s is quoted; the attempt count is a bare number (write the long form for an expression: {attempts: %s})", data, data)
		}
		var n json.Number
		if err := json.Unmarshal(data, &n); err != nil {
			return fmt.Errorf("retry: must be a number of attempts or an object, got %s", data)
		}
		attempts, err := n.Int64()
		if err != nil || attempts < 0 {
			return fmt.Errorf("retry: %s is not a whole number of attempts", n)
		}
		*r = Retry{Attempts: RetryCount(int(attempts))}
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
	if !w.Attempts.IsExpr() && w.Attempts.n < 0 {
		return fmt.Errorf("retry: attempts must not be negative")
	}
	if !w.Factor.IsExpr() && w.Factor.n != 0 && w.Factor.n < 1 {
		return fmt.Errorf("retry: factor %g would shrink the wait after every attempt; use 1 for a constant delay", w.Factor.n)
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
				"description": "The long form, naming the attempt count and any part of the backoff curve to override. Every slot also accepts a $: expression, evaluated when the rule fires — so a policy can be driven from config.",
				"properties": {
					"attempts":  {"type": ["integer", "string"], "minimum": 0, "description": "Number of retries before following goto or failing. 0 = no retries. A $: expression must evaluate to a whole number."},
					"delay":     {"type": ["string", "number"], "description": "Wait before the first retry: a fixed duration such as \"30s\" or \"2h30m\" (units ms, s, m, h — calendar units d/w/mo/y are not accepted, since the curve scales this value), a bare number of milliseconds, or a $: expression evaluating to milliseconds. Defaults to 1s."},
					"factor":    {"type": ["number", "string"], "minimum": 1, "description": "Multiplier applied to the wait after each further attempt. 1 keeps the delay constant. A $: expression must evaluate to a number of at least 1. Defaults to 2."},
					"max_delay": {"type": ["string", "number"], "description": "Ceiling the growing wait is clamped to, in the same grammar as 'delay'. Defaults to 5m, or to 'delay' when that is longer. Must not be shorter than 'delay'."}
				},
				"additionalProperties": false
			}
		]
	}`), nil
}

// RetryNumber is a retry policy's attempt count or growth factor: a literal number, or a
// "$:" expression evaluated when the rule fires. A literal zero and an absent slot are the
// same thing, which is what makes `retry: 0` the absent key.
type RetryNumber struct {
	n    float64
	expr string // non-empty when the slot is an expression; n is then meaningless
}

// RetryCount is the Go spelling of a literal attempt count, for definitions built in code.
func RetryCount(n int) RetryNumber { return RetryNumber{n: float64(n)} }

func (v RetryNumber) IsZero() bool { return v.expr == "" && v.n == 0 }
func (v RetryNumber) IsExpr() bool { return v.expr != "" }

// Expr is the "$:" source, empty when the slot is a literal. Validation type-checks it; the
// engine hands it to the evaluator.
func (v RetryNumber) Expr() string { return v.expr }

// Literal is the authored number, meaningful only when the slot is not an expression —
// every static check on it must be guarded by IsExpr.
func (v RetryNumber) Literal() float64 { return v.n }

// resolve returns the slot's value and whether it was set at all, so the caller can tell an
// authored 0 from an absent slot when applying a default.
func (v RetryNumber) resolve(eval func(string) (any, error)) (float64, bool, error) {
	if v.expr == "" {
		return v.n, v.n != 0, nil
	}
	out, err := eval(v.expr)
	if err != nil {
		return 0, false, err
	}
	n, err := retryFloat(out)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

func (v RetryNumber) MarshalJSON() ([]byte, error) {
	if v.expr != "" {
		return json.Marshal(v.expr)
	}
	if v.n == 0 {
		return []byte("null"), nil
	}
	return json.Marshal(v.n)
}

func (v *RetryNumber) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*v = RetryNumber{}
		return nil
	}
	if data[0] == '"' {
		var src string
		if err := json.Unmarshal(data, &src); err != nil {
			return err
		}
		expr, err := retryExpr(src)
		if err != nil {
			return err
		}
		*v = RetryNumber{expr: expr}
		return nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("expected a number or a $: expression, got %s", data)
	}
	f, err := n.Float64()
	if err != nil {
		return fmt.Errorf("%s is not a number", n)
	}
	*v = RetryNumber{n: f}
	return nil
}

// JSONSchemaBytes returns the JSON Schema for RetryNumber, which reflection would otherwise
// render as an empty object — the type's only fields are unexported.
func (RetryNumber) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"type": ["number", "string"],
		"description": "A number, or a $: expression evaluating to one."
	}`), nil
}

// RetryDuration is a fixed duration in a retry policy: "30s", "2h30m", a bare number of
// milliseconds, or a "$:" expression evaluating to milliseconds. It keeps the literal it
// was written as so a stored definition round-trips as authored.
//
// Calendar units are refused rather than resolved: the backoff curve scales this value and
// compares it against a ceiling, and "1mo" is not a length until a timezone and a start
// instant say so.
type RetryDuration struct {
	src  any
	d    time.Duration
	expr string // non-empty when the slot is an expression; d is then meaningless
}

// ParseRetryDuration accepts the wire forms of a retry duration — a duration string, a
// number of milliseconds, or a "$:" expression — and is the only place any of them is
// turned into a length.
func ParseRetryDuration(v any) (RetryDuration, error) {
	switch x := v.(type) {
	case string:
		tmpl, err := template.Parse(x)
		if err != nil {
			return RetryDuration{}, err
		}
		lit, isLit := tmpl.Static()
		if !isLit {
			expr, err := retryExpr(x)
			if err != nil {
				return RetryDuration{}, err
			}
			return RetryDuration{src: x, expr: expr}, nil
		}
		parsed, err := delayspec.ParseDuration(lit)
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
		return RetryDuration{}, fmt.Errorf("expected a duration string, a number of milliseconds, or a $: expression, got %T", v)
	}
}

func (d RetryDuration) IsZero() bool { return d.src == nil }
func (d RetryDuration) IsExpr() bool { return d.expr != "" }

// Expr is the "$:" source, empty when the slot is a literal.
func (d RetryDuration) Expr() string { return d.expr }

// Duration is 0 when the slot is absent or an expression, which is why every static check
// on it must be guarded by IsExpr and why Resolve applies the defaults instead.
func (d RetryDuration) Duration() time.Duration { return d.d }

// resolve returns the slot's length and whether it was set at all. An expression yields a
// number of MILLISECONDS, the same unit a delay's `for` takes.
func (d RetryDuration) resolve(eval func(string) (any, error)) (time.Duration, bool, error) {
	if d.expr == "" {
		return d.d, d.src != nil, nil
	}
	out, err := eval(d.expr)
	if err != nil {
		return 0, false, err
	}
	ms, err := retryFloat(out)
	if err != nil {
		return 0, false, err
	}
	if ms != float64(int64(ms)) {
		return 0, false, fmt.Errorf("%v is not a whole number of milliseconds", ms)
	}
	return time.Duration(int64(ms)) * time.Millisecond, true, nil
}

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
		"description": "A fixed duration: \"30s\", \"2h30m\" (units ms, s, m, h), a bare number of milliseconds, or a $: expression evaluating to milliseconds."
	}`), nil
}

// retryExpr accepts only a whole-value "$:" leaf. A "${ }" interpolation is rejected BY
// NAME because it produces a string at runtime — the same failure checkDelaySlot removes
// from the delay grammar.
func retryExpr(src string) (string, error) {
	tmpl, err := template.Parse(src)
	if err != nil {
		return "", err
	}
	if !tmpl.IsExpr() {
		return "", fmt.Errorf("%q is not a number; write a bare number, or a whole-value $: expression such as %q", src, "$: config.retry_attempts")
	}
	return src, nil
}

// retryFloat narrows what an expression evaluated to. Mirrors delayMillis: the engine hands
// back whichever numeric form the expression produced.
func retryFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, fmt.Errorf("%v is not a number", n)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("must evaluate to a number, got %T", v)
	}
}
