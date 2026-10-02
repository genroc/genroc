package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"genroc/internal/errcode"
	"genroc/internal/logview"
	"genroc/internal/model"
)

// logEvent is shared by audit (console + durable trail) and logOnly (console only), so both
// render identically.
type logEvent struct {
	Level model.LogLevel
	Event string
	ID    string // instance id; audit fills this from the instance
	Task  string
	Msg   string // human note (rendered as note=…, since slog uses msg for the event)
	Code  errcode.Code
	// Data is a VALUE, not pre-rendered text: it is cut on the way to storage, so a repeated
	// payload shares the instance's object. specs/object-store.md.
	Data  any
	Meta  map[string]any
	Actor string // left unset for the engine's own work; audit fills in model.ActorEngine
}

// audit's DB write is best-effort: a failure is logged and swallowed, never aborting an advance.
func (e *Engine) audit(inst *model.ProcessInstance, ev logEvent) {
	ev.ID = inst.ID
	// The one place engine attribution is applied, so nothing is written unattributed. Never
	// the operator who started the run. specs/api-auth.md section 7.
	if ev.Actor == "" {
		ev.Actor = model.ActorEngine
	}
	// Redaction has one sink, stdout; the trail and the API carry what happened. Value scrubbing
	// works because expressions have no functions. specs/object-store.md §Redaction.
	consoleEv := ev
	text := dataText(ev.Data)
	if secrets := e.contextSecrets(inst); len(secrets) > 0 {
		text = redactSecrets(text, secrets)
		consoleEv.Msg = redactSecrets(consoleEv.Msg, secrets)
		consoleEv.Meta = redactMeta(consoleEv.Meta, secrets)
	}
	// Console shows a capped excerpt regardless of how the full payload is persisted.
	e.emitWithData(consoleEv, truncateStr(text, e.payloadCap()))
	if err := e.db.AppendLogValue(&model.LogEntry{
		InstanceID: ev.ID,
		Level:      ev.Level,
		Event:      ev.Event,
		TaskID:     ev.Task,
		Message:    ev.Msg,
		Code:       string(ev.Code),
		Actor:      ev.Actor,
		Meta:       ev.Meta,
	}, ev.Data, int64(e.payloadCap())); err != nil {
		e.logOnly(logEvent{Level: model.LogError, ID: ev.ID, Msg: "append audit log: " + err.Error()})
	}
}

// contextSecrets is the config and nothing more: `secret: true` is valid only in config_schema.
// specs/object-store.md §secret: true is CONFIG-ONLY.
func (e *Engine) contextSecrets(inst *model.ProcessInstance) []string {
	def, err := e.definition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil
	}
	out := def.SecretConfigValues(inst.Config)
	// Longest first: scrubbing a substring secret first exposes the longer one's tail ("***0").
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func redactSecrets(s string, secrets []string) string {
	for _, sv := range secrets {
		if sv != "" {
			s = strings.ReplaceAll(s, sv, "***")
		}
	}
	return s
}

// redactMeta returns a copy of meta with secret values scrubbed from its string values;
// the original map is left unchanged.
func redactMeta(meta map[string]any, secrets []string) map[string]any {
	if len(meta) == 0 || len(secrets) == 0 {
		return meta
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if s, ok := v.(string); ok {
			out[k] = redactSecrets(s, secrets)
		} else {
			out[k] = v
		}
	}
	return out
}

// logOnly is for lines in no instance's trail; it carries no Event, so it renders free-form.
func (e *Engine) logOnly(ev logEvent) {
	ev.Event = "" // operational: no structured event
	e.emit(ev)
}

// emit renders through logview.Record so console and CLI match.
func (e *Engine) emit(ev logEvent) { e.emitWithData(ev, dataText(ev.Data)) }

// dataText renders a log value for the console. Storage keeps the value; only the operator's
// line needs text.
func dataText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func (e *Engine) emitWithData(ev logEvent, data string) {
	lvl := slogLevel(ev.Level)
	if !e.log.Enabled(context.Background(), lvl) {
		return
	}
	if ev.Event == "" {
		// operational: message + any id/meta as free-form fields.
		attrs := make([]any, 0, 2+2*len(ev.Meta))
		if ev.ID != "" {
			attrs = append(attrs, "id", ev.ID)
		}
		for _, k := range sortedMetaKeys(ev.Meta) {
			attrs = append(attrs, k, ev.Meta[k])
		}
		e.log.Log(context.Background(), lvl, ev.Msg, attrs...)
		return
	}
	// audit: the event is the slog message; id/task become columns; the rest detail.
	detail := logview.Record{
		Event: ev.Event, Msg: ev.Msg, Code: string(ev.Code), Actor: ev.Actor, Data: data, Meta: ev.Meta,
	}.Detail(e.logCfg.Mode)
	attrs := make([]any, 0, 6+2*len(detail))
	attrs = append(attrs, logview.AuditKey, true, "id", ev.ID, "task", ev.Task)
	for _, f := range detail {
		attrs = append(attrs, f.Key, f.Val)
	}
	e.log.Log(context.Background(), lvl, ev.Event, attrs...)
}

func sortedMetaKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func slogLevel(l model.LogLevel) slog.Level {
	switch l {
	case model.LogDebug:
		return slog.LevelDebug
	case model.LogWarn:
		return slog.LevelWarn
	case model.LogError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// statusMeta wraps an HTTP status as event metadata, or nil for a non-HTTP (status 0)
// transport so the meta field stays absent.
func statusMeta(status int) map[string]any {
	if status == 0 {
		return nil
	}
	return map[string]any{"status": status}
}

// AuditCreated records instance_created with the process input. actor is who asked for this
// run; a child passes "", crediting the engine rather than the root's operator.
func (e *Engine) AuditCreated(inst *model.ProcessInstance, actor string) {
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceCreated,
		Actor: actor, Data: e.snippet(inst.State["input"])})
}

func (e *Engine) outputData(inst *model.ProcessInstance) any {
	return e.snippet(inst.State["output"])
}

// snippet keeps the FULL payload: audit caps it for the console and cuts oversized values,
// so capture is never lossy. nil when payload capture is off.
func (e *Engine) snippet(v any) any {
	if !e.logCfg.Payloads {
		return nil
	}
	return v
}

// snippetRaw is snippet for a string payload; nil when capture is off or s is empty.
func (e *Engine) snippetRaw(s string) any {
	if !e.logCfg.Payloads || s == "" {
		return nil
	}
	return s
}

// payloadCap is the configured per-payload size — both the console truncation point and
// the inline-vs-externalize threshold for log data.
func (e *Engine) payloadCap() int {
	if e.logCfg.PayloadBytes > 0 {
		return e.logCfg.PayloadBytes
	}
	return defaultPayloadBytes
}

func truncateStr(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}
