package model

import (
	"encoding/json"
	"fmt"
)

// Timeout is a task's deadline: a scalar ("30s", desugared to `for` at decode) or an object
// ({until, tz}). Absent means no deadline of its own (a fetch takes the engine default, an
// external waits); 0 does NOT mean forever — internal/model/CLAUDE.md.
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
	// A typo'd key (`untill`) would decode to an empty timeout, which is silently no
	// timeout at all.
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
				"description": "Shorthand for 'for', resolved in UTC: \"2h30m\", a number of milliseconds, or a $: expression."
			},
			{
				"type": "object",
				"description": "The long form, naming exactly one of 'for' or 'until' plus an optional 'tz'.",
				"properties": {
					"for":   {"type": ["string", "number"], "description": "A duration from when the task is reached: \"2h30m\", a number of milliseconds, or a $: expression."},
					"until": {"type": ["string", "number"], "description": "An absolute deadline, external tasks only. The same instants as a delay's 'until'."},
					"tz":    {"type": "string", "description": "IANA name (\"Europe/Prague\") or fixed offset (\"+02:00\"); defaults to UTC. Abbreviations are rejected."}
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
