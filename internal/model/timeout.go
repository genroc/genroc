package model

import (
	"encoding/json"
	"fmt"
)

// Timeout is a task's execution deadline, written in either of two forms:
//
//	timeout: "30s"                                  // the `for` grammar, resolved in UTC
//	timeout: {until: "fri 17:00", tz: "Europe/Prague"}
//
// The scalar desugars to `for` at decode, the way SwitchMap's string shorthand desugars to
// a single case, so everything downstream sees one shape and the stored definition is
// canonical.
//
// Absent means "no deadline of its own": a fetch falls back to the engine's default, an
// external waits indefinitely. 0 no longer spells "forever" the way the old timeout_ms did,
// because a slot whose zero means the opposite of its smallest value cannot also carry a
// duration grammar — a zero or past deadline is refused on a fetch and clamps to "due now"
// on an external.
type Timeout struct {
	DelaySpec
}

// TimeoutFor is the Go spelling of the scalar shorthand, for definitions built in code
// rather than decoded from JSON.
func TimeoutFor(v any) Timeout { return Timeout{DelaySpec{For: v}} }

func (t Timeout) IsZero() bool { return t.For == nil && t.Until == nil && t.TZ == "" }

func (t Timeout) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.DelaySpec)
}

var timeoutFields = map[string]bool{"for": true, "until": true, "tz": true}

func (t *Timeout) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || len(data) == 0 {
		*t = Timeout{}
		return nil
	}
	if data[0] != '{' {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
		*t = Timeout{DelaySpec{For: v}}
		return nil
	}
	// A typo'd key is the failure this rejection exists for: `untill` or the removed
	// `timeout_ms` would decode to an empty object, and an empty timeout is silently no
	// timeout at all — the deadline the author wrote would simply never apply.
	if err := rejectUnknownFields("timeout", data, timeoutFields); err != nil {
		return err
	}
	var spec DelaySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("timeout: %w", err)
	}
	*t = Timeout{spec}
	return nil
}

// JSONSchemaBytes returns the JSON Schema for Timeout so that OpenAPI reflection produces
// its two wire forms rather than the flattened embedded struct.
func (Timeout) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{
				"type": ["string", "number"],
				"description": "Shorthand for 'for', resolved in UTC: a duration such as \"30s\" or \"2h30m\" (units ms, s, m, h, d, w, mo, y), a bare number of milliseconds, or a $: expression evaluating to milliseconds. A quoted number without a unit is rejected as ambiguous."
			},
			{
				"type": "object",
				"description": "The long form, naming exactly one of 'for' or 'until' plus an optional 'tz'. Use it for a timeout with a timezone, or for an absolute deadline on an external task.",
				"properties": {
					"for":   {"type": ["string", "number"], "description": "A duration measured from the moment the task is reached, e.g. \"30s\" or \"2h30m\", a bare number of milliseconds, or a $: expression evaluating to milliseconds."},
					"until": {"type": ["string", "number"], "description": "An absolute deadline — external tasks only, since a fetch deadline already in the past would report a timeout for a request that was never sent. Accepts the same instants as a delay's 'until': \"2026-09-01T08:00:00+02:00\", \"fri 17:00\", \"*-*-01 08:00\", a bare number of unix milliseconds, or a $: expression evaluating to unix milliseconds."},
					"tz":    {"type": "string", "description": "IANA name (\"Europe/Prague\") or fixed offset (\"+02:00\") that for's calendar units and until's wall clocks resolve in; defaults to UTC. Abbreviations such as \"CET\" are rejected — they are ambiguous across DST."}
				},
				"oneOf": [
					{"required": ["for"]},
					{"required": ["until"]}
				],
				"additionalProperties": false
			}
		]
	}`), nil
}
