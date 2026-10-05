package api

import (
	"context"
	"encoding/json"
	"fmt"
	"genroc/internal/numeric"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

const defaultChannel = "latest"

// engineService returns primitives, not a shared struct: a struct either side owned would
// make the dependency an import.
type engineService interface {
	Tick(ctx context.Context) (int, error)
	ManualTick() bool
	AuditCreated(inst *model.ProcessInstance, actor string)
	NotifyWork()
	WorkerID() string
	LeaseAge() time.Duration
}

type Handlers struct {
	db     *db.DB
	engine engineService
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
	// principal is attached by the route wrapper; unexported so a client cannot decode its own
	// grants into it. specs/api-auth.md §3.
	principal *Principal
}

type Reply struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	// Code is the failure classification; writeReply renders the status from it.
	Code Code `json:"code,omitempty"`
	// Outcome is Code's success-side twin: the status a lifecycle assertion renders as.
	// specs/id-list-commands.md.
	Outcome model.Outcome `json:"outcome,omitempty"`
	// Fields carries per-field detail when a submitted definition failed validation,
	// so a client can point at the offending field instead of parsing the message.
	Fields []model.FieldError `json:"fields,omitempty"`
}

// Handle dispatches to the matching action in the registry (actions.go), authorizing first.
func (h *Handlers) Handle(env Envelope) Reply {
	for i := range registry {
		if registry[i].Name == env.Action {
			if err := authorize(registry[i], env.principal); err != nil {
				return err.reply()
			}
			return registry[i].handle(h, env)
		}
	}
	return notFound("unknown action %q", env.Action).reply()
}

// okReply reports a marshal failure rather than a 200 with an empty body, which a client
// would read as an empty result.
func okReply(v interface{}) Reply {
	data, err := json.Marshal(v)
	if err != nil {
		return errReply(fmt.Errorf("encode response: %w", err))
	}
	return Reply{OK: true, Data: data}
}

// outcomeReply is okReply plus the outcome statusOfOutcome reads. OutcomeUnchanged carries
// no body: 204 forbids one.
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

// errReply classifies through codeOf, so a forwarded db error gets the right status.
func errReply(err error) Reply {
	return Reply{OK: false, Error: err.Error(), Code: codeOf(err), Fields: fieldsOf(err)}
}

func (e *Error) reply() Reply { return errReply(e) }

// decodeBody classifies an empty, malformed or unrecognised body as invalid.
func decodeBody[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := numeric.DecodeStrict(raw, &v); err != nil {
		return v, invalid("decode: %w", err)
	}
	return v, nil
}

// decodeOptionalBody: an absent body yields the zero T; a present one decodes strictly, so a
// misspelled field errors instead of dropping.
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
