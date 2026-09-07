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
	levelFlag := fs.String("level", string(model.LogInfo), "lowest level to show: this level and everything above it (warn keeps errors). `debug` is the bottom, so it is the whole trail -- the engine records a call's request and response bodies there")
	sinceFlag := fs.String("since", "", "read forward from this point: a duration back from now (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04); empty = the newest 200 entries")
	untilFlag := fs.String("until", "", "stop at this point (same forms as --since); on its own it keeps the cap, giving the newest rows before that instant")
	flatFlag := fs.Bool("flat", false, "this instance's own rows only; by default a ROOT id answers with every row in its tree")
	modeFlag := fs.String("mode", "detail", "output: basic (no data body), detail (+ data, cut to one line -- $COLUMNS sets the width), or json (one JSON object per line, untruncated)")
	timeFlag := fs.String("time", "clock", "time column: clock (15:04:05, with a day separator per date) or full (2006-01-02 15:04:05 +02:00); both render in the local zone ($TZ)")
	id := instanceIDAndFlags(fs, args)
	mode, err := logview.ParseMode(*modeFlag)
	if err != nil {
		fatal("%v", err)
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
	// A tree read can carry rows from several instances, so the ID column comes with it. It is
	// tied to the REQUEST rather than to what a page happens to hold: a column that appears
	// once the second page arrives would re-align a trail mid-scroll.
	tree := !*flatFlag
	u := *serverFlag + "/api/instances/" + url.PathEscape(id) + "/logs"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}

	// Buffered so a long trail costs one write per page rather than one per row; the
	// flush at each page boundary is what keeps the output streaming. fatal() exits
	// without unwinding, so every error path flushes first.
	out := bufio.NewWriter(os.Stdout)
	fatalFlushing := func(format string, args ...any) {
		out.Flush()
		fatal(format, args...)
	}
	noteIfCapped := func(capped bool) {
		noteCapped(capped, fmt.Sprintf("the newest %d entries", logTailDefault), "--since")
	}

	// json mode dumps each entry as the server's JSON, one per line (JSONL):
	// everything, untruncated, pipe-friendly (jq).
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
		Time     string          `json:"time"`
		Instance string          `json:"instance"`
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
	// Shared logview layout, so a row reads identically here and on the server console. The
	// header waits for the first row (an empty trail prints nothing); day carries the last
	// date rendered so each new day gets a DateBreak. Both fetchOrdered paths render here.
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
