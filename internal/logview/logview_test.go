package logview

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLabel(t *testing.T) {
	cases := map[string]string{
		"inst_created":     "input",
		"action_started":   "request",
		"action_succeeded": "result",
		"action_failed":    "error",
		"inst_completed":   "output",
		"child_spawned":    "data", // events without a payload label fall back to "data"
	}
	for event, want := range cases {
		if got := Label(event); got != want {
			t.Errorf("Label(%q) = %q, want %q", event, got, want)
		}
	}
}

func TestParseModeAndIncludesData(t *testing.T) {
	for _, s := range []string{"basic", "detail", "json"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q) errored: %v", s, err)
		}
	}
	if _, err := ParseMode("raw"); err == nil {
		t.Error("ParseMode(raw) should error")
	}
	if ModeBasic.IncludesData() {
		t.Error("basic should not include data")
	}
	if !ModeDetail.IncludesData() {
		t.Error("detail should include data")
	}
	if !ModeJSON.IncludesData() {
		t.Error("json should include data")
	}
}

func TestDetail(t *testing.T) {
	rec := Record{
		Event: "action_succeeded", ID: "i1", Task: "fetch",
		Data: `{"slept":5000}`, Meta: map[string]any{"status": float64(200)},
	}
	// id/task are columns, not detail; basic omits the data body, detail appends it
	// under the event's label.
	basic := rec.Detail(ModeBasic)
	if hasKey(basic, "result") || hasKey(basic, "id") || hasKey(basic, "task") {
		t.Errorf("basic detail should be just meta: %v", basic)
	}
	detail := rec.Detail(ModeDetail)
	if !hasKey(detail, "result") || !hasKey(detail, "status") {
		t.Errorf("detail missing expected keys: %v", detail)
	}
	if hasKey(detail, "id") || hasKey(detail, "task") {
		t.Errorf("id/task are columns, not detail: %v", detail)
	}
}

func TestRenderEvent(t *testing.T) {
	rec := Record{Event: "action_succeeded", Data: `{"slept":5000}`, Meta: map[string]any{"status": float64(200)}}
	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

	// With id (tree): fixed-width columns, JSON body rendered raw, status from meta.
	got := RenderEvent(TimeClock, ts, "info", "abc123", rec.Event, "fetch", rec.Detail(ModeDetail), true)
	want := "15:04:05  INFO   abc123  action_succeeded  fetch           status=200 result={\"slept\":5000}"
	if got != want {
		t.Errorf("RenderEvent(tree):\n got %q\nwant %q", got, want)
	}

	// basic mode drops the data body.
	got = RenderEvent(TimeClock, ts, "info", "abc123", rec.Event, "fetch", rec.Detail(ModeBasic), true)
	want = "15:04:05  INFO   abc123  action_succeeded  fetch           status=200"
	if got != want {
		t.Errorf("RenderEvent(basic):\n got %q\nwant %q", got, want)
	}

	// The zero value is the console's layout, so a caller with no opinion cannot
	// silently render a zero-width time column.
	if got, clock := RenderEvent("", ts, "info", "", rec.Event, "fetch", nil, false),
		RenderEvent(TimeClock, ts, "info", "", rec.Event, "fetch", nil, false); got != clock {
		t.Errorf("zero TimeStyle:\n got %q\nwant %q (TimeClock)", got, clock)
	}
}

func TestTimeStyle(t *testing.T) {
	for _, s := range []string{"clock", "full"} {
		if _, err := ParseTimeStyle(s); err != nil {
			t.Errorf("ParseTimeStyle(%q) errored: %v", s, err)
		}
	}
	if _, err := ParseTimeStyle("relative"); err == nil {
		t.Error("ParseTimeStyle(relative) should error")
	}
	// Only the dated style suppresses DateBreak; getting this backwards prints the date
	// twice, or not at all.
	if TimeClock.CarriesDate() || !TimeFull.CarriesDate() {
		t.Error("CarriesDate should be false for clock, true for full")
	}

	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	rec := Record{Event: "action_succeeded", Meta: map[string]any{"status": float64(200)}}
	// The full style widens the time column by exactly its extra layout, so every later
	// column stays aligned with its header rather than drifting right by a few spaces.
	// UTC must render as "+00:00", not "Z" — a one-character offset would shorten the
	// column for exactly the readers running under TZ=UTC.
	got := RenderEvent(TimeFull, ts, "info", "", rec.Event, "fetch", rec.Detail(ModeBasic), false)
	want := "2026-01-02 15:04:05 +00:00  INFO   action_succeeded  fetch           status=200"
	if got != want {
		t.Errorf("RenderEvent(full):\n got %q\nwant %q", got, want)
	}
	// Every zone yields the same width, so the columns never shift with the reader's TZ.
	for _, loc := range []string{"UTC", "Europe/Prague", "Asia/Kathmandu"} {
		zone, err := time.LoadLocation(loc)
		if err != nil {
			t.Skipf("no tzdata for %s: %v", loc, err)
		}
		if w := len(ts.In(zone).Format(TimeFull.layout())); w != TimeFull.width() {
			t.Errorf("%s renders %d chars, but the column is %d wide", loc, w, TimeFull.width())
		}
	}
	for _, style := range []TimeStyle{TimeClock, TimeFull} {
		header, row := Header(style, false), RenderEvent(style, ts, "info", "", rec.Event, "fetch", nil, false)
		if i, j := strings.Index(header, "EVENT"), strings.Index(row, "action_succeeded"); i != j {
			t.Errorf("%s: EVENT header at %d but its column at %d — the widths disagree", style, i, j)
		}
	}
}

func TestDateBreak(t *testing.T) {
	ts := time.Date(2026, 8, 4, 15, 4, 5, 0, time.UTC)
	// The zone rides along: under TimeClock this line is the only place it can appear,
	// and without it a trail read from another machine is silently shifted.
	if got := DateBreak(ts); got != "--- 2026-08-04 +00:00 ---" {
		t.Errorf("DateBreak = %q", got)
	}
	zone, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// 15:04 UTC is the 4th in Prague too, but the date is the local one, not the stamp's.
	if got := DateBreak(ts.In(zone)); got != "--- 2026-08-04 +02:00 ---" {
		t.Errorf("DateBreak(Prague) = %q", got)
	}
	// An offset, not an abbreviation: "CST" is Shanghai here and Chicago below, fourteen
	// hours apart, and Kathmandu has no abbreviation at all. Matching delayspec's `tz`.
	for _, tc := range []struct{ zone, want string }{
		{"Asia/Shanghai", "+08:00"},
		{"America/Chicago", "-05:00"},
		{"Asia/Kathmandu", "+05:45"},
	} {
		loc, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Skipf("no tzdata for %s: %v", tc.zone, err)
		}
		if got, want := DateBreak(ts.In(loc)), "--- 2026-08-04 "+tc.want+" ---"; got != want {
			t.Errorf("DateBreak(%s) = %q, want %q", tc.zone, got, want)
		}
	}
}

func TestRenderFree(t *testing.T) {
	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	got := RenderFree(ts, "info", "engine started", []Field{{"worker", "w1"}})
	want := "15:04:05  INFO   msg=\"engine started\" worker=w1"
	if got != want {
		t.Errorf("RenderFree:\n got %q\nwant %q", got, want)
	}
}

func TestRenderJSON(t *testing.T) {
	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

	// An audit record: event/id/task plus the detail fields become object keys.
	got := RenderJSON(ts, "INFO", "action_succeeded", true, "abc123", "fetch",
		[]Field{{"status", float64(200)}, {"result", `{"slept":5000}`}})
	want := `{"event":"action_succeeded","id":"abc123","level":"info","result":"{\"slept\":5000}","status":200,"task":"fetch","time":"2026-01-02T15:04:05Z"}`
	if got != want {
		t.Errorf("RenderJSON(audit):\n got %s\nwant %s", got, want)
	}

	// An operational record: the message is carried as msg, no event/task.
	got = RenderJSON(ts, "INFO", "engine started", false, "", "", []Field{{"worker", "w1"}})
	want = `{"level":"info","msg":"engine started","time":"2026-01-02T15:04:05Z","worker":"w1"}`
	if got != want {
		t.Errorf("RenderJSON(free):\n got %s\nwant %s", got, want)
	}
}

// The console must render UTC whatever zone the host runs in — otherwise a fleet's logs
// interleave by wall clock and stop collating, and nothing in the output would say why.
func TestHandlerRendersUTC(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Kathmandu") // +05:45: no whole-hour coincidence
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	stamped := time.Date(2026, 8, 4, 15, 4, 5, 0, time.UTC).In(zone)

	for _, tc := range []struct {
		mode Mode
		want string
	}{
		{ModeBasic, "15:04:05"},                     // not 20:49:05, the host's clock
		{ModeJSON, `"time":"2026-08-04T15:04:05Z"`}, // Z, matching the API's stamps
	} {
		var buf bytes.Buffer
		h := NewHandler(&buf, slog.LevelInfo, tc.mode)
		if err := h.Handle(context.Background(), slog.NewRecord(stamped, slog.LevelInfo, "engine started", 0)); err != nil {
			t.Fatalf("Handle(%s): %v", tc.mode, err)
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("%s rendered %q, want it to contain %q", tc.mode, buf.String(), tc.want)
		}
	}
}

func TestFormatVal(t *testing.T) {
	cases := map[any]string{
		`{"a":1}`:    `{"a":1}`,  // JSON body raw (not quoted despite quotes)
		"→ next":     `"→ next"`, // free text with a space → quoted
		"http.500":   "http.500", // plain token raw
		float64(200): "200",      // integer, no decimal
	}
	for in, want := range cases {
		if got := FormatVal(in); got != want {
			t.Errorf("FormatVal(%v) = %q, want %q", in, got, want)
		}
	}
}

func hasKey(fs []Field, key string) bool {
	for _, f := range fs {
		if f.Key == key {
			return true
		}
	}
	return false
}
