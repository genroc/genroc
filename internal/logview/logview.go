// Package logview renders an instance's audit trail for both the server console (streaming) and
// genctl logs (batch), in the same fields and columns. The zone differs by design: the console is
// pinned to TimeClock in UTC, the CLI follows the reader's zone and states it on every DateBreak.
package logview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"genroc/internal/model"
)

// Mode is how a record is rendered: basic shows the bounded columns/fields, detail
// adds the (variable-width) data body, and json emits one JSON object per line.
type Mode string

const (
	ModeBasic  Mode = "basic"
	ModeDetail Mode = "detail"
	ModeJSON   Mode = "json"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeBasic, ModeDetail, ModeJSON:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("invalid log mode %q (want basic, detail, or json)", s)
	}
}

func (m Mode) IncludesData() bool { return m == ModeDetail || m == ModeJSON }

// AuditKey is the slog attr marking a DB-persisted audit event, which the console renders in
// columns; operational logs lack it. The handler strips it from output.
const AuditKey = "_audit"

// Column widths are fixed because the server streams one record at a time; a wider value just
// pushes its row right. The time column's width comes from the TimeStyle instead.
const (
	colLevel = 5  // DEBUG
	colID    = 8  // a minted instance id, in full: what a listing prints is what a command takes
	colEvent = 16 // longest event (action_succeeded)
	colTask  = 14 // user-defined task id; the last column before the detail fields
)

// TimeStyle selects the time column's layout: the console is fixed at TimeClock, and the CLI
// offers TimeFull for a trail read days later. The zero value is TimeClock.
type TimeStyle string

const (
	TimeClock TimeStyle = "clock" // 15:04:05
	TimeFull  TimeStyle = "full"  // 2006-01-02 15:04:05 +02:00
)

func ParseTimeStyle(s string) (TimeStyle, error) {
	switch TimeStyle(s) {
	case TimeClock, TimeFull:
		return TimeStyle(s), nil
	default:
		return "", fmt.Errorf("invalid time style %q (want clock or full)", s)
	}
}

// CarriesDate reports whether the column shows the date itself. A caller printing
// DateBreak must skip it when this is true — the date belongs in exactly one place.
func (s TimeStyle) CarriesDate() bool { return s == TimeFull }

// layout renders in the caller's zone. Use "-07:00", not "Z07:00": the latter collapses UTC to
// "Z" and the column stops being fixed-width.
func (s TimeStyle) layout() string {
	if s == TimeFull {
		return "2006-01-02 15:04:05 -07:00"
	}
	return "15:04:05"
}

// width is derived from the layout, so the column can never drift from what fills it.
func (s TimeStyle) width() int { return len(s.layout()) }

// Label is the display name for an event's data body (e.g. "result", "input"); events
// without a payload fall back to "data".
func Label(event string) string {
	switch event {
	case "inst_created":
		return "input"
	case "action_started":
		return "request"
	case "action_succeeded":
		return "result"
	case "action_failed":
		return "error"
	case "inst_completed":
		return "output"
	default:
		return "data"
	}
}

// Field is one rendered key/value of a log line.
type Field struct {
	Key string
	Val any
}

// Record is the layout-independent content of one audit event.
type Record struct {
	Event string
	ID    string
	Task  string
	Msg   string // human note
	Code  string
	Actor string // who asked for this, on operator-initiated events only
	Data  string // body (request/response/input/output/…)
	Meta  map[string]any
}

// Detail returns the trailing key=value fields (not the fixed columns): msg, code, sorted
// meta, and the data body under its Label when the mode includes it. Shared by both surfaces.
func (r Record) Detail(mode Mode) []Field {
	fs := make([]Field, 0, 3+len(r.Meta))
	if r.Msg != "" {
		fs = append(fs, Field{"msg", r.Msg})
	}
	if r.Code != "" {
		fs = append(fs, Field{"code", r.Code})
	}
	// model.ActorEngine is on nearly every row, so it is not rendered; storage and the API
	// keep it either way.
	if r.Actor != "" && r.Actor != model.ActorEngine {
		fs = append(fs, Field{"by", r.Actor})
	}
	for _, k := range sortedKeys(r.Meta) {
		fs = append(fs, Field{k, r.Meta[k]})
	}
	if r.Data != "" && mode.IncludesData() {
		fs = append(fs, Field{Label(r.Event), r.Data})
	}
	return fs
}

// RenderEvent renders an audit event as one fixed-width column line (id column only if withID):
//
//	15:04:05  INFO   2559a9  action_started    first         msg=fetch url=… request={…}
func RenderEvent(style TimeStyle, t time.Time, level, id, event, task string, detail []Field, withID bool) string {
	line := columnPrefix(style, t.Format(style.layout()), strings.ToUpper(level), id, event, task, withID)
	if d := renderFields(detail); d != "" {
		line += "  " + d
	}
	return strings.TrimRight(line, " ")
}

// Clamp cuts line to width characters, marking the cut with an ellipsis; width <= 0 leaves it
// whole. A wrapping payload would misalign every row after it.
func Clamp(line string, width int) string {
	if width <= 0 {
		return line
	}
	r := []rune(line)
	if len(r) <= width {
		return line
	}
	return string(r[:width-1]) + "…"
}

// RenderFree renders an operational record free-form, deliberately not column-fitted:
//
//	15:04:05  INFO   msg="engine started" max_concurrent=200 worker=…
func RenderFree(t time.Time, level, message string, fields []Field) string {
	if message != "" {
		fields = append([]Field{{"msg", message}}, fields...)
	}
	line := fmt.Sprintf("%-*s  %-*s", TimeClock.width(), t.Format(TimeClock.layout()), colLevel, strings.ToUpper(level))
	if d := renderFields(fields); d != "" {
		line += "  " + d
	}
	return strings.TrimRight(line, " ")
}

// RenderJSON renders a record as one compact JSON object (JSONL), untruncated: columns
// and detail fields become keys — audit records carry event (+task), operational ones msg.
func RenderJSON(t time.Time, level, message string, isAudit bool, id, task string, fields []Field) string {
	obj := make(map[string]any, 5+len(fields))
	obj["time"] = t.Format(time.RFC3339Nano)
	obj["level"] = strings.ToLower(level)
	if id != "" {
		obj["id"] = id
	}
	if isAudit {
		obj["event"] = message
		if task != "" {
			obj["task"] = task
		}
	} else if message != "" {
		obj["msg"] = message
	}
	for _, f := range fields {
		obj[f.Key] = f.Val
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(b)
}

// Header is the column header line for the CLI (the streaming server has none).
func Header(style TimeStyle, withID bool) string {
	return strings.TrimRight(columnPrefix(style, "TIME", "LEVEL", "ID", "EVENT", "TASK", withID), " ")
}

// DateBreak is the CLI's marker above each calendar day's first row under TimeClock; callers emit
// the first unconditionally and none under a style that CarriesDate. The zone is t's offset, never
// an abbreviation ("CST" is Shanghai and Chicago), so each day across a DST change has its own.
func DateBreak(t time.Time) string {
	return "--- " + t.Format("2006-01-02 -07:00") + " ---"
}

func columnPrefix(style TimeStyle, t, level, id, event, task string, withID bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  ", style.width(), t, colLevel, level)
	if withID {
		fmt.Fprintf(&b, "%-*s  ", colID, id)
	}
	fmt.Fprintf(&b, "%-*s  %-*s", colEvent, event, colTask, task)
	return b.String()
}

func renderFields(fs []Field) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, f.Key+"="+FormatVal(f.Val))
	}
	return strings.Join(parts, " ")
}

// FormatVal renders a field value compactly: JSON bodies ({…}/[…]) and plain tokens
// raw, free text with spaces quoted, integers without a trailing decimal.
func FormatVal(v any) string {
	s := valToString(v)
	switch {
	case s == "":
		return `""`
	case strings.HasPrefix(s, "{"), strings.HasPrefix(s, "["): // JSON body — keep raw/readable
		return s
	case strings.ContainsAny(s, " \t"):
		return strconv.Quote(s)
	default:
		return s
	}
}

func valToString(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case json.Number:
		return n.String()
	case float64: // JSON numbers; integers print without a decimal point
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'g', -1, 64)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case bool:
		return strconv.FormatBool(n)
	case fmt.Stringer:
		return n.String()
	default:
		return fmt.Sprint(v)
	}
}

// ShortID is the compact id tag for the ID column — the id's random tail, not its
// timestamp-prefixed head, so a parent and same-millisecond child differ.
func ShortID(id string) string {
	if len(id) > colID {
		return id[len(id)-colID:]
	}
	return id
}

func sortedKeys(m map[string]any) []string {
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

// NewHandler builds the server console slog handler: AuditKey records render as aligned columns
// and the rest free-form, or one JSON object per line in json mode. Times are UTC.
func NewHandler(w io.Writer, level slog.Level, mode Mode) slog.Handler {
	return &consoleHandler{w: w, level: level, mode: mode, mu: &sync.Mutex{}}
}

type consoleHandler struct {
	w     io.Writer
	level slog.Level
	mode  Mode
	attrs []slog.Attr
	mu    *sync.Mutex
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

// Handle stamps UTC, not the host's zone: a fleet's logs must collate into one timeline, and no
// console line names its zone.
func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	t := r.Time.UTC()
	isAudit := false
	var id, task string
	detail := make([]Field, 0, 8)
	collect := func(a slog.Attr) {
		switch a.Key {
		case AuditKey:
			isAudit = true
		case "id":
			id = a.Value.String()
		case "task":
			task = a.Value.String()
		default:
			detail = append(detail, Field{a.Key, a.Value.Any()})
		}
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool { collect(a); return true })

	var line string
	switch {
	case h.mode == ModeJSON:
		line = RenderJSON(t, r.Level.String(), r.Message, isAudit, id, task, detail)
	case isAudit:
		line = RenderEvent(TimeClock, t, r.Level.String(), ShortID(id), r.Message, task, detail, true)
	default:
		if id != "" { // an operational log about an instance keeps its id as a field
			detail = append([]Field{{"id", id}}, detail...)
		}
		line = RenderFree(t, r.Level.String(), r.Message, detail)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

func (h *consoleHandler) WithAttrs(as []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &nh
}

func (h *consoleHandler) WithGroup(string) slog.Handler { return h } // groups unused
