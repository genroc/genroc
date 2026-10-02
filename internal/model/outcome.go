package model

import (
	"encoding/json"
	"fmt"

	"genroc/internal/numeric"
)

// ExternalOutcome answers a parked external task with a result or a failure, as one value
// because it is one event. Failure nil means a result, which may itself be null, so a decoder
// must discriminate on key PRESENCE.
type ExternalOutcome struct {
	Failure *ExternalFailure
	Result  any
}

// ExternalFailure is a submitted failure. Data is absent, not null, for a code declared to carry
// none, so the runtime context is no richer than inference derives.
type ExternalFailure struct {
	Code    string
	Message string
	Data    any
	HasData bool
}

// signalEnvelope is the stored form of a buffered signal (process_signals.outcome) and mirrors
// the request body, so the column and the wire never drift. Result is held raw so a large
// integer survives the round trip; see numeric.
type signalEnvelope struct {
	Error  *ExternalFailureJSON `json:"error,omitempty"`
	Result json.RawMessage      `json:"result,omitempty"`
}

// ExternalFailureJSON is ExternalFailure on the wire and in the buffer. Data is a pointer so
// its absence survives: a code declared to carry nothing must not come back as null.
type ExternalFailureJSON struct {
	Code    string           `json:"code"`
	Message string           `json:"message"`
	Data    *json.RawMessage `json:"data,omitempty"`
}

// MarshalOutcome encodes an outcome for the signal buffer.
func MarshalOutcome(o ExternalOutcome) (string, error) {
	var env signalEnvelope
	if o.Failure != nil {
		f := &ExternalFailureJSON{Code: o.Failure.Code, Message: o.Failure.Message}
		if o.Failure.HasData {
			raw, err := json.Marshal(o.Failure.Data)
			if err != nil {
				return "", fmt.Errorf("marshal failure data: %w", err)
			}
			msg := json.RawMessage(raw)
			f.Data = &msg
		}
		env.Error = f
	} else {
		raw, err := json.Marshal(o.Result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		env.Result = raw
	}
	b, err := json.Marshal(env)
	return string(b), err
}

// UnmarshalOutcome decodes through numeric: a plain unmarshal into `any` collapses every number
// to float64.
func UnmarshalOutcome(s string) (ExternalOutcome, error) {
	var env signalEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return ExternalOutcome{}, fmt.Errorf("decode buffered signal: %w", err)
	}
	if env.Error != nil {
		f := &ExternalFailure{Code: env.Error.Code, Message: env.Error.Message}
		if env.Error.Data != nil {
			if err := numeric.Decode(*env.Error.Data, &f.Data); err != nil {
				return ExternalOutcome{}, fmt.Errorf("decode buffered failure data: %w", err)
			}
			f.HasData = true
		}
		return ExternalOutcome{Failure: f}, nil
	}
	var v any
	if len(env.Result) > 0 {
		if err := numeric.Decode(env.Result, &v); err != nil {
			return ExternalOutcome{}, fmt.Errorf("decode buffered result: %w", err)
		}
	}
	return ExternalOutcome{Result: v}, nil
}
