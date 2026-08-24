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

// logEvent is the structured payload of one log line. Level and Event are
// required; the rest are optional. It is shared by audit (console + durable DB
// trail) and logOnly (console only), so both render identically — only persistence
// differs.
type logEvent struct {
	Level model.LogLevel
	Event string
	ID    string // instance id; audit fills this from the instance
	Task  string
	Msg   string // human note (rendered as note=…, since slog uses msg for the event)
	Code  errcode.Code
	// Data is the body (request/response/input/output/…) as a VALUE, not pre-rendered text. It
	// is cut like any other value on the way to storage -- so a payload that repeats something
	// the instance already externalized shares that object instead of copying it -- and rendered
	// once for the console. specs/object-store.md.
	Data any
	Meta map[string]any
}

// audit records an instance event to the console (slog) and the durable per-instance DB
// trail. Best-effort on the DB write: a failure is logged and swallowed so audit logging
// can never abort an advance.
func (e *Engine) audit(inst *model.ProcessInstance, ev logEvent) {
	ev.ID = inst.ID
	// Redaction is a RECORDING concern and its one sink is stdout, where a value is read by an
	// operator who did not ask for it. The durable trail and every API response carry what
	// actually happened: protecting a value at rest is encryption's job, and redacting on read
	// was never that. specs/object-store.md §Redaction.
	//
	// Scrubbing works by replacing the secret VALUES: expressions have no functions, so a secret
	// always appears verbatim in anything logged, and nothing reaches a line in a form a
	// string-replace would miss.
	consoleEv := ev
	text := dataText(ev.Data)
	if secrets := e.contextSecrets(inst); len(secrets) > 0 {
		text = redactSecrets(text, secrets)
		consoleEv.Msg = redactSecrets(consoleEv.Msg, secrets)
		consoleEv.Meta = redactMeta(consoleEv.Meta, secrets)
	}
	// Console shows a capped excerpt regardless of how the full payload is persisted.
	e.emitWithData(consoleEv, truncateStr(text, e.payloadCap()))
	if err := e.db.AppendLog(&model.LogEntry{
		InstanceID: ev.ID,
		Level:      ev.Level,
		Event:      ev.Event,
		TaskID:     ev.Task,
		Message:    ev.Msg,
		Code:       string(ev.Code),
		Data:       e.encodeLogData(ev.ID, ev.Data),
		Meta:       ev.Meta,
	}); err != nil {
		e.logOnly(logEvent{Level: model.LogError, ID: ev.ID, Msg: "append audit log: " + err.Error()})
	}
}

// contextSecrets gathers the secret values to keep out of stdout. `secret: true` is valid only
// in config_schema (model.validate refuses it anywhere else), so this is the config and nothing
// more -- no schema walk over the context, no taint to follow.
//
// That narrowing is what removed the whole second redaction mechanism. A fetch RESPONSE BODY
// never enters the context, so a value-based scrub could not see it and a schema-driven pass had
// to blank it structurally while building the log snippet -- before audit was called, leaving
// nothing unredacted to store. With no `secret: true` outside config there is nothing for it to
// blank. specs/object-store.md §secret: true is CONFIG-ONLY.
func (e *Engine) contextSecrets(inst *model.ProcessInstance) []string {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil
	}
	out := def.SecretConfigValues(inst.Config)
	// Scrub the longest value first: when one secret is a prefix/substring of another (e.g. an
	// input array [5, 50, 500]), replacing the shorter one first consumes the shared lead and
	// leaves the longer one's tail exposed ("***0", "***00"). Length-descending order makes each
	// value redacted as a whole.
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

// logOnly records a console-only line (server lifecycle / operational events not in any
// instance's durable trail). It carries no Event, so it renders free-form rather than as
// a columnar audit row.
func (e *Engine) logOnly(ev logEvent) {
	ev.Event = "" // operational: no structured event
	e.emit(ev)
}

// emit renders one record to the console via slog. It builds the attrs only when the
// level is enabled, keeping audit's hot path — the DB write — cheap. A record with an
// Event is a structured audit event (rendered in aligned columns); one without is
// operational (free-form). Fields come from logview.Record so console and CLI match.
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
		Event: ev.Event, Msg: ev.Msg, Code: string(ev.Code), Data: data, Meta: ev.Meta,
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

// AuditCreated records the instance_created milestone, capturing the instance's process
// input (subject to payload-logging config). Called by the API for a root instance and by
// the engine for each spawned child; it bookends the trail with instance_completed.
func (e *Engine) AuditCreated(inst *model.ProcessInstance) {
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceCreated, Data: e.snippet(inst.ContextData["input"])})
}

// outputData is the snippet of the process's final output (context_data["output"], set by
// computeOutput) for the instance_completed event; nil when there is no output or payload
// logging is off.
func (e *Engine) outputData(inst *model.ProcessInstance) any {
	return e.snippet(inst.ContextData["output"])
}

// snippet passes v through as an audit detail, keeping the FULL payload (no truncation --
// audit caps it for the console and cuts oversized values, so the capture is never lossy).
// Returns nil when payload capture is off.
func (e *Engine) snippet(v any) any {
	if !e.logCfg.Payloads {
		return nil
	}
	return v
}

// snippetRaw returns an already-string payload (e.g. a raw error response body) in full;
// audit caps/externalizes it like snippet. Returns "" when payload capture is off or s
// is empty.
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

// encodeLogData renders a scrubbed payload into the data column: small inline, large as a
// log object + short preview, so high-churn process_logs never holds a huge value.
// Best-effort: a failed object write falls back to a truncated inline preview.
func (e *Engine) encodeLogData(instanceID string, v any) string {
	if v == nil {
		return ""
	}
	// The same cut as any other value: the fewest, largest leaves move out and the rest stays
	// inline. A payload repeating something the instance already externalized -- a child's input
	// carrying a script bundle -- produces the identical leaf, hashes the same, and SHARES that
	// object rather than storing a second copy of it.
	env, err := e.db.CutLogValue(instanceID, v, int64(e.payloadCap()))
	if err != nil {
		if b, mErr := json.Marshal(model.Envelope{Data: truncateStr(dataText(v), e.payloadCap())}); mErr == nil {
			return string(b)
		}
		return ""
	}
	if b, err := json.Marshal(env); err == nil {
		return string(b)
	}
	return ""
}
