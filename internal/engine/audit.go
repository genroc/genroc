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
	"genroc/internal/schema"
)

// snippetResult redacts an action's raw result body against its result_schema, then
// returns the capped JSON snippet. The response body is not in the instance context, so
// audit's context-secret pass can't scrub it — it is schema-redacted here instead.
func (e *Engine) snippetResult(task *model.Task, body any) string {
	if e.logCfg.Payloads && task.Action != nil && task.Action.ResultSchema != nil {
		body = task.Action.ResultSchema.Redact(body)
	}
	return e.snippet(body)
}

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
	Data  string // body (request/response/input/output/…); shown under its event Label
	Meta  map[string]any
}

// audit records an instance event to the console (slog) and the durable per-instance DB
// trail. Best-effort on the DB write: a failure is logged and swallowed so audit logging
// can never abort an advance.
func (e *Engine) audit(inst *model.ProcessInstance, ev logEvent) {
	ev.ID = inst.ID
	// Scrub every secret value (config + input + output, identified by the taint
	// schemas) from the log before it is emitted or stored. A single sink here is
	// the robust choice: genroc expressions have no functions, so a secret always
	// appears verbatim (or as a substring) in any logged value — there is no way for
	// it to reach a log line in a form a string-replace would miss.
	if secrets := e.contextSecrets(inst); len(secrets) > 0 {
		ev.Data = redactSecrets(ev.Data, secrets)
		ev.Msg = redactSecrets(ev.Msg, secrets)
		ev.Meta = redactMeta(ev.Meta, secrets)
	}
	// Console shows a capped excerpt regardless of how the full payload is persisted.
	consoleEv := ev
	consoleEv.Data = truncateStr(ev.Data, e.payloadCap())
	e.emit(consoleEv)
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

// contextSecrets gathers every secret value currently in the instance's context —
// config secrets plus input/output values whose inferred schema is marked secret — so
// audit can scrub them from log text. (The response body is not in the context; it is
// schema-redacted at its log site via snippetResult.)
//
// It considers only already-materialized values: an unresolved *ObjectRef is skipped,
// because a value never loaded was never used, so it cannot appear in any log line. This
// relies on the invariant that anything logged is derived from a value resolved BEFORE the
// audit call that logs it (every eval path feeds inst.ResolvedObjects first) — preserve it.
func (e *Engine) contextSecrets(inst *model.ProcessInstance) []string {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil
	}
	out := def.SecretConfigValues(inst.Config)
	sf, ok := e.schemaFile(inst)
	if !ok {
		return out
	}
	collect := func(v any, sc schema.Schema) {
		if sc.IsZero() {
			return
		}
		if ref, isRef := v.(*model.ObjectRef); isRef {
			cached, ok := inst.ResolvedObjects[ref.Ref]
			if !ok {
				return // never materialized this advance → cannot be in any log line
			}
			v = cached
		}
		out = append(out, sc.WithDefs(sf.Defs).CollectSecrets(v)...)
	}
	if v, ok := inst.ContextData["input"]; ok {
		collect(v, sf.ProcessInput)
	}
	if outs, ok := inst.ContextData["outputs"].(map[string]any); ok {
		for tid, v := range outs {
			if ts, ok := sf.Tasks[tid]; ok {
				collect(v, ts.Output)
			}
		}
	}
	// Scrub the longest value first: when one secret is a prefix/substring of
	// another (e.g. an input array [5, 50, 500]), replacing the shorter one first
	// consumes the shared lead and leaves the longer one's tail exposed ("***0",
	// "***00"). Length-descending order makes each value redacted as a whole.
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
func (e *Engine) emit(ev logEvent) {
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
		Event: ev.Event, Msg: ev.Msg, Code: string(ev.Code), Data: ev.Data, Meta: ev.Meta,
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
// computeOutput) for the instance_completed event; "" when there is no output or payload
// logging is off.
func (e *Engine) outputData(inst *model.ProcessInstance) string {
	return e.snippet(inst.ContextData["output"])
}

// snippet renders v as JSON for an audit detail, returning the FULL payload (no
// truncation — audit caps it for the console and externalizes oversized values, so the
// capture is never lossy). Returns "" when payload capture is off or v is empty.
func (e *Engine) snippet(v any) string {
	if !e.logCfg.Payloads || v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// snippetRaw returns an already-string payload (e.g. a raw error response body) in full;
// audit caps/externalizes it like snippet. Returns "" when payload capture is off or s
// is empty.
func (e *Engine) snippetRaw(s string) string {
	if !e.logCfg.Payloads {
		return ""
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

// logPreviewBytes is the length of the inline excerpt kept on a log row whose full
// payload was externalized, so a listing can show a snippet without loading the object.
const logPreviewBytes = 512

func truncateStr(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// encodeLogData renders a (secret-scrubbed) log payload into the data column: a small
// payload is stored inline as an envelope, a large one is written to a log object and
// stored as a reference plus a short preview, so the high-churn process_logs table never
// holds a huge value. Best-effort: a failed object write falls back to a truncated inline
// preview.
func (e *Engine) encodeLogData(instanceID, full string) string {
	if full == "" {
		return ""
	}
	if len(full) <= e.payloadCap() {
		if b, err := json.Marshal(model.Envelope{Data: full}); err == nil {
			return string(b)
		}
		return ""
	}
	ref, err := e.db.WriteLogObject(instanceID, full)
	if err != nil {
		if b, mErr := json.Marshal(model.Envelope{Data: truncateStr(full, e.payloadCap())}); mErr == nil {
			return string(b)
		}
		return ""
	}
	if b, err := json.Marshal(model.Envelope{Refs: []*model.ObjectRef{ref}, Preview: truncateStr(full, logPreviewBytes)}); err == nil {
		return string(b)
	}
	return ""
}
