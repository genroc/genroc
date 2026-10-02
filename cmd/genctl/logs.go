package main

// genctl logs: an instance's audit trail. Three views over one stream, rendered through
// internal/logview so a row reads identically here and on the server console.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"genroc/internal/logview"
	"genroc/internal/model"
)

func runLogsCmd(server string, args []string) {
	fs := newFlagSet("logs", args)
	serverFlag := addServerFlag(fs, server)
	levelFlag := fs.String("level", string(model.LogInfo), "lowest level to show; debug includes request and response bodies")
	sinceFlag := fs.String("since", "", "show rows from this point on: a duration ago (2h) or a timestamp")
	untilFlag := fs.String("until", "", "show rows before this point; same forms as --since")
	flatFlag := fs.Bool("flat", false, "only this instance's rows, not its tree's")
	modeFlag := fs.String("mode", "detail", "basic, or detail to add each entry's data on one line")
	jsonFlag := fs.Bool("json", false, "print the raw JSON entries, one per line (JSONL), untruncated")
	timeFlag := fs.String("time", "clock", "clock (time, with a line per day) or full (date and time on every row)")
	id := instanceIDAndFlags(fs, args)
	// Not logview.ParseMode: it accepts json (the server's --log-mode uses it), which here is
	// spelled --json like every other list.
	mode := logview.Mode(*modeFlag)
	if mode != logview.ModeBasic && mode != logview.ModeDetail {
		fatal("invalid --mode %q (want basic or detail)", *modeFlag)
	}
	if *jsonFlag {
		mode = logview.ModeJSON
	}
	style, err := logview.ParseTimeStyle(*timeFlag)
	if err != nil {
		fatal("%v", err)
	}

	q := url.Values{}
	if *levelFlag != "" {
		if model.LogLevelsAtLeast(model.LogLevel(*levelFlag)) == nil {
			fatal("invalid --level %q (want debug, info, warn, or error)", *levelFlag)
		}
		q.Set("level", *levelFlag)
	}
	// created_at is a trail's only order, so --since needs no column to pair with.
	limit := applyWindow(q, *sinceFlag, *untilFlag, "created_at", logTailDefault)
	if *flatFlag {
		q.Set("flat", "true")
	}
	// The ID column follows the REQUEST, not the page: one appearing with the second page would
	// re-align a trail mid-scroll.
	tree := !*flatFlag
	u := *serverFlag + "/api/instances/" + url.PathEscape(id) + "/logs"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}

	// Flushed at each page boundary to keep streaming. fatal() exits without unwinding, so every
	// error path flushes first.
	out := bufio.NewWriter(os.Stdout)
	fatalFlushing := func(format string, args ...any) {
		out.Flush()
		fatal(format, args...)
	}
	noteIfCapped := func(capped bool) {
		noteCapped(capped, fmt.Sprintf("the newest %d entries", logTailDefault), "--since")
	}

	if mode == logview.ModeJSON {
		capped, err := fetchOrdered(u, limit, newestFirst, func(items []json.RawMessage) error {
			for _, it := range items {
				out.Write(it)
				out.WriteByte('\n')
			}
			return out.Flush()
		})
		if err != nil {
			fatalFlushing("%v", err)
		}
		out.Flush()
		noteIfCapped(capped)
		return
	}

	type logRow struct {
		Time     string          `json:"created_at"`
		Instance string          `json:"instance_id"`
		Level    string          `json:"level"`
		Event    string          `json:"event"`
		Task     string          `json:"task"`
		Message  string          `json:"message"`
		Code     string          `json:"code"`
		Actor    string          `json:"actor"`
		Data     json.RawMessage `json:"data"`
		Meta     map[string]any  `json:"meta"`
		Objects  []objectEntry   `json:"objects"`
	}
	// The header waits for the first row, so an empty trail prints nothing.
	header, day, width := false, "", logLineWidth()
	capped, err := fetchOrdered(u, limit, newestFirst, func(rows []logRow) error {
		for _, l := range rows {
			if !header {
				fmt.Fprintln(out, logview.Header(style, tree))
				header = true
			}
			t, ok := parseTime(l.Time)
			if d := t.Format("2006-01-02"); ok && !style.CarriesDate() && d != day {
				fmt.Fprintln(out, logview.DateBreak(t))
				day = d
			}
			rec := logview.Record{Event: l.Event, Task: l.Task, Msg: l.Message, Code: l.Code, Actor: l.Actor, Data: logData(l.Data, l.Objects), Meta: l.Meta}
			idTag := ""
			if tree {
				idTag = l.Instance
			}
			fmt.Fprintln(out, logview.Clamp(logview.RenderEvent(style, t, l.Level, idTag, l.Event, l.Task, rec.Detail(mode), tree), width))
		}
		return out.Flush()
	})
	if err != nil {
		fatalFlushing("%v", err)
	}
	out.Flush()
	noteIfCapped(capped)
}
