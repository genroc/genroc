package api

import (
	"context"
	"encoding/json"
	"fmt"
	"genroc/internal/numeric"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
	"genroc/internal/validation"
)

const defaultChannel = "latest"

// engineService is the slice of the engine the API depends on. Primitive returns, not a
// shared struct: the dependency points api → engine only through this interface, and a
// struct either side owned would make that an import.
type engineService interface {
	Tick(ctx context.Context) (int, error)
	ManualTick() bool
	AuditCreated(inst *model.ProcessInstance)
	NotifyWork()
	WorkerID() string
	LeaseAge() time.Duration
}

type Handlers struct {
	db     *db.DB
	engine engineService
	// schemas memoises inference for redacting an instance's context on read. Only
	// stored definitions go in — a definition being registered is inferred fresh,
	// since it is the submitted document being checked, not a cached one.
	schemas validation.SchemaCache
}

func NewHandlers(database *db.DB, eng engineService) *Handlers {
	return &Handlers{db: database, engine: eng}
}

// --- Envelope ---

type Envelope struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
	// For GET-style actions that only need an ID.
	ID string `json:"id,omitempty"`
}

type Reply struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	// Code classifies the failure for machines; see the Code constants. It is on
	// Reply, not on the HTTP response alone, because TCP and UDS clients encode
	// Reply directly and have no status line to read.
	Code Code `json:"code,omitempty"`
	// Outcome is Code's success-side twin, and lives here for the same reason: what a
	// lifecycle assertion did (applied / accepted / unchanged) has to reach the
	// transports that have no status line. Empty on every action that is not an
	// assertion. specs/id-list-commands.md.
	Outcome model.Outcome `json:"outcome,omitempty"`
	// Fields carries per-field detail when a submitted definition failed validation,
	// so a client can point at the offending field instead of parsing the message.
	Fields []model.FieldError `json:"fields,omitempty"`
}

// Handle is the single entry-point shared by all transports (HTTP, TCP, UDS); it
// dispatches to the matching action in the registry (actions.go).
func (h *Handlers) Handle(env Envelope) Reply {
	for i := range registry {
		if registry[i].Name == env.Action {
			return registry[i].handle(h, env)
		}
	}
	return notFound("unknown action %q", env.Action).reply()
}

// okReply encodes a successful payload. A marshal failure is reported rather than
// swallowed: returning OK with an empty Data would hand the client a 200 and an empty
// body, so it would believe it had received an empty result rather than nothing.
func okReply(v interface{}) Reply {
	data, err := json.Marshal(v)
	if err != nil {
		return errReply(fmt.Errorf("encode response: %w", err))
	}
	return Reply{OK: true, Data: data}
}

// outcomeReply is okReply for a lifecycle assertion: same body, plus the outcome the
// HTTP status is derived from (statusOfOutcome). An OutcomeUnchanged reply carries no
// body — 204 forbids one — so the fields describing what changed are omitted where
// nothing did.
func outcomeReply(res db.LifecycleResult) Reply {
	if res.Outcome == model.OutcomeUnchanged {
		return Reply{OK: true, Outcome: res.Outcome}
	}
	r := okReply(map[string]any{
		"outcome":   res.Outcome,
		"status":    res.Status,
		"instances": res.Instances,
	})
	r.Outcome = res.Outcome
	return r
}

// errReply renders any error as a failed Reply, classifying it through codeOf — so a
// handler that simply forwards a db error still produces the right code and status,
// and only paths nobody has classified come out as internal.
func errReply(err error) Reply {
	return Reply{OK: false, Error: err.Error(), Code: codeOf(err), Fields: fieldsOf(err)}
}

func (e *Error) reply() Reply { return errReply(e) }

// decodeBody unmarshals a required JSON body into T. An empty, malformed or
// unrecognised body is an error wrapped with the "decode:" prefix and classified
// invalid — the request is wrong, and no retry of it will do better.
func decodeBody[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := numeric.DecodeStrict(raw, &v); err != nil {
		return v, invalid("decode: %w", err)
	}
	return v, nil
}

// decodeOptionalBody: an absent body yields the zero T, a present one MUST decode —
// strictly, so a misspelled field errors instead of dropping. Optional is about presence
// only; {"advance_ms": "12000"} once left the clock unmoved and answered 200.
func decodeOptionalBody[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := numeric.DecodeStrict(raw, &v); err != nil {
		return v, invalid("decode: %w", err)
	}
	return v, nil
}
