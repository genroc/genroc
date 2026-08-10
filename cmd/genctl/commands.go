package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"genroc/internal/numeric"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"genroc/internal/logview"

	"gopkg.in/yaml.v3"
)

func runApplyCmd(server string, args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "f", "definition file (YAML or JSON); repeat for multiple files")
	serverFlag := addServerFlag(fs, server)
	channelFlag := fs.String("channel", "latest", "channel to apply definitions to")
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: -f is required")
		os.Exit(1)
	}

	defs, err := loadDefs(files)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{
		"channel":     *channelFlag,
		"definitions": defs,
	}

	var resp []struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Saved   bool   `json:"saved"`
	}
	if err := call(*serverFlag+"/definitions/batch", http.MethodPut, body, &resp); err != nil {
		fatal("%v", err)
	}
	for _, r := range resp {
		status := "saved"
		if !r.Saved {
			status = "unchanged"
		}
		fmt.Printf("%s: %s@v%d\n", status, r.Name, r.Version)
	}
}

func runValidateCmd(server string, args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "f", "definition file (YAML or JSON); repeat for multiple files")
	serverFlag := addServerFlag(fs, server)
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: -f is required")
		os.Exit(1)
	}

	defs, err := loadDefs(files)
	if err != nil {
		fatal("%v", err)
	}

	var raw json.RawMessage
	if err := call(*serverFlag+"/definitions/validate", http.MethodPost, defs, &raw); err != nil {
		fatal("%v", err)
	}
	printIndented(raw)
}

func runChannelCmd(server string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: genctl channel <list|set|delete> ...")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("channel", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	fs.Parse(args[1:])
	rest := fs.Args()

	sub := args[0]
	switch sub {
	case "list":
		if len(rest) < 1 {
			fatal("usage: genctl channel list <process>")
		}
		type channelRow struct {
			Channel string `json:"channel"`
			Version int    `json:"version"`
		}
		listURL := *serverFlag + "/channels?name=" + url.QueryEscape(rest[0])
		resp, err := listAll[channelRow](listURL)
		if err != nil {
			fatal("%v", err)
		}
		for _, e := range resp {
			fmt.Printf("%s -> v%d\n", e.Channel, e.Version)
		}

	case "set":
		if len(rest) < 3 {
			fatal("usage: genctl channel set <process> <channel> <version>")
		}
		v, err := strconv.Atoi(rest[2])
		if err != nil || v < 1 {
			fatal("version must be a positive integer")
		}
		if err := call(*serverFlag+"/channels", http.MethodPut,
			map[string]any{"name": rest[0], "channel": rest[1], "version": v}, nil); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set: %s@%s -> v%d\n", rest[0], rest[1], v)

	case "delete":
		if len(rest) < 2 {
			fatal("usage: genctl channel delete <process> <channel>")
		}
		if err := call(*serverFlag+"/channels", http.MethodDelete,
			map[string]any{"name": rest[0], "channel": rest[1]}, nil); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted: %s@%s\n", rest[0], rest[1])

	default:
		fatal("unknown channel subcommand %q", sub)
	}
}

func runPromoteCmd(server string, args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	fromFlag := fs.String("from", "", "source channel")
	toFlag := fs.String("to", "", "target channel")
	processFlag := fs.String("process", "", "limit to this process and its dependency subtree (optional)")
	fs.Parse(args)

	if *fromFlag == "" || *toFlag == "" {
		fatal("--from and --to are required")
	}

	body := map[string]any{"from": *fromFlag, "to": *toFlag}
	if *processFlag != "" {
		body["process"] = *processFlag
	}

	var resp struct {
		From     string           `json:"from"`
		To       string           `json:"to"`
		Promoted []map[string]any `json:"promoted"`
	}
	if err := call(*serverFlag+"/channels/promote", http.MethodPost, body, &resp); err != nil {
		fatal("%v", err)
	}
	for _, p := range resp.Promoted {
		fmt.Printf("promoted: %v@v%v -> %s\n", p["name"], p["version"], resp.To)
	}
}

func runStatusCmd(server string, args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	channelFlag := fs.String("channel", "latest", "channel to inspect")
	fs.Parse(args)

	var resp []struct {
		Name      string `json:"name"`
		Version   int    `json:"version"`
		StaleRefs []struct {
			TaskID         string `json:"task_id"`
			ChildName      string `json:"child_name"`
			BakedVersion   int    `json:"baked_version"`
			ChannelVersion int    `json:"channel_version"`
		} `json:"stale_refs"`
	}
	if err := call(*serverFlag+"/channels/status", http.MethodPost,
		map[string]any{"channel": *channelFlag}, &resp); err != nil {
		fatal("%v", err)
	}

	allClean := true
	for _, item := range resp {
		if len(item.StaleRefs) == 0 {
			continue
		}
		allClean = false
		fmt.Printf("STALE  %s@v%d\n", item.Name, item.Version)
		for _, ref := range item.StaleRefs {
			fmt.Printf("  task %q: %s baked@v%d, channel@v%d\n",
				ref.TaskID, ref.ChildName, ref.BakedVersion, ref.ChannelVersion)
		}
	}
	if allClean {
		fmt.Printf("channel %q is coherent\n", *channelFlag)
	}
}

func runRunCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl run <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]")
	}
	process := args[0]

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	channelFlag := fs.String("channel", "", "resolve the version via this channel")
	versionFlag := fs.Int("version", 0, "pin an explicit process version")
	inputFlag := fs.String("input", "", "input as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read input from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set an input field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "print only the new instance id, e.g. id=$(genctl run NAME -q)")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(args[1:])

	input, hasInput, err := buildInput(*inputFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{"process": process}
	switch {
	case *versionFlag > 0:
		body["version"] = *versionFlag
	case *channelFlag != "":
		body["channel"] = *channelFlag
	}
	if hasInput {
		body["input"] = input
	}

	var resp struct {
		ID      string `json:"id"`
		Process string `json:"process"`
		Version int    `json:"version"`
		Status  string `json:"status"`
	}
	if err := call(*serverFlag+"/instances", http.MethodPost, body, &resp); err != nil {
		// Surface an input-schema mismatch as a clear, dedicated message instead of
		// the generic "server: ..." wrapper.
		if detail, ok := inputValidationError(err); ok {
			fatal("input is not valid for %s:\n  %s", process, detail)
		}
		fatal("%v", err)
	}
	// Record the id so a follow-up command can resolve @last (or a bare-id default)
	// without copy-pasting. Best-effort: an unwritable state dir must not fail run.
	if err := saveLastInstance(resp.ID); err != nil {
		fmt.Fprintf(os.Stderr, "genctl: warning: could not record last instance id: %v\n", err)
	}
	// -q prints just the id so it composes: id=$(genctl run NAME -q).
	if *quietFlag {
		fmt.Println(resp.ID)
		return
	}
	fmt.Printf("started: %s  %s@v%d  (%s)\n", resp.ID, resp.Process, resp.Version, resp.Status)
}

func runResolveCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl resolve <token> [--result <json|-> | -f file] [--set k=v ...] [-q]")
	}
	token := args[0]

	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(args[1:])

	// A missing --result/-f/--set means an empty result: valid for a task with no
	// result_schema, and rejected by the server otherwise (surfaced below).
	result, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{"token": token, "result": result}

	var resp struct {
		Resolved bool `json:"resolved"`
	}
	if err := call(*serverFlag+"/external-tasks/resolve", http.MethodPost, body, &resp); err != nil {
		// Surface a result-schema mismatch as a clear, dedicated message instead of the
		// generic "server: ..." wrapper (mirrors run's input-validation handling).
		if detail, ok := resultValidationError(err); ok {
			fatal("result is not valid for this task:\n  %s", detail)
		}
		fatal("%v", err)
	}
	if *quietFlag {
		return
	}
	fmt.Printf("resolved: %s\n", token)
}

// runSignalCmd delivers a result to an instance's external task by id + --task (not a
// queue token like resolve): resolved now if the task is armed, else buffered FIFO until armed.
func runSignalCmd(server string, args []string) {
	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	taskFlag := fs.String("task", "", "the external task id to signal")
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	// The instance id is the sole positional (before or after flags); resolves @last.
	id := instanceIDAndFlags(fs, args)

	if *taskFlag == "" {
		fatal("usage: genctl signal <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]")
	}

	result, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{"task_id": *taskFlag, "result": result}

	var resp struct {
		Delivered bool `json:"delivered"`
		Buffered  bool `json:"buffered"`
	}
	if err := call(*serverFlag+"/instances/"+url.PathEscape(id)+"/signal", http.MethodPost, body, &resp); err != nil {
		// Surface a result-schema mismatch as a dedicated message (mirrors resolve/run).
		if detail, ok := resultValidationError(err); ok {
			fatal("result is not valid for task %q:\n  %s", *taskFlag, detail)
		}
		fatal("%v", err)
	}
	if *quietFlag {
		return
	}
	state := "delivered"
	if resp.Buffered {
		state = "buffered"
	}
	fmt.Printf("signaled: %s  task=%s  (%s)\n", id, *taskFlag, state)
}

func runGetCmd(server string, args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	jsonFlag := fs.Bool("json", false, "print the raw JSON response")
	resolveFlag := fs.Bool("resolve", false, "resolve externalized context values inline instead of {ref, size} references")
	id := instanceIDAndFlags(fs, args)

	u := *serverFlag + "/instances/" + url.PathEscape(id)
	if *resolveFlag {
		u += "?resolve=true"
	}
	if *jsonFlag {
		var raw json.RawMessage
		if err := callGet(u, &raw); err != nil {
			fatal("%v", err)
		}
		printIndented(raw)
		return
	}

	var inst struct {
		ID         string         `json:"id"`
		Process    string         `json:"process"`
		Version    int            `json:"version"`
		Status     string         `json:"status"`
		WaitState  string         `json:"wait_state"`
		Task       string         `json:"task"`
		RetryCount int            `json:"retry_count"`
		Error      string         `json:"error"`
		ErrorCode  string         `json:"error_code"`
		CreatedAt  string         `json:"created_at"`
		UpdatedAt  string         `json:"updated_at"`
		Context    map[string]any `json:"context"`
	}
	if err := callGet(u, &inst); err != nil {
		fatal("%v", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", inst.ID)
	fmt.Fprintf(w, "Process:\t%s@v%d\n", inst.Process, inst.Version)
	fmt.Fprintf(w, "Status:\t%s\n", inst.Status)
	// Where the process is, printed right under what is happening to it — and on a
	// settled instance, where it stopped.
	if inst.Task != "" {
		fmt.Fprintf(w, "Task:\t%s\n", inst.Task)
	}
	if inst.WaitState != "" {
		fmt.Fprintf(w, "Wait:\t%s\n", inst.WaitState)
	}
	if inst.RetryCount > 0 {
		fmt.Fprintf(w, "Retries:\t%d\n", inst.RetryCount)
	}
	fmt.Fprintf(w, "Created:\t%s\n", longTime(inst.CreatedAt))
	fmt.Fprintf(w, "Updated:\t%s\n", longTime(inst.UpdatedAt))
	if inst.Error != "" {
		fmt.Fprintf(w, "Error:\t%s\n", inst.Error)
	}
	if inst.ErrorCode != "" {
		fmt.Fprintf(w, "Code:\t%s\n", inst.ErrorCode)
	}
	w.Flush()

	if len(inst.Context) > 0 {
		fmt.Println("\nContext:")
		b, _ := json.MarshalIndent(inst.Context, "", "  ")
		os.Stdout.Write(b)
		os.Stdout.Write([]byte("\n"))
	}
}

func runInstancesCmd(server string, args []string) {
	fs := flag.NewFlagSet("instances", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	statusFlag := fs.String("status", "", "filter by status (running, completed, failing, failed, raised, pausing, paused)")
	codeFlag := fs.String("error-code", "", "filter by exact error code (e.g. card_declined, http.500)")
	sortFlag := fs.String("sort", "created", "sort key: created or updated (most recently active)")
	sinceFlag := fs.String("since", "", "read forward from this point: a duration back from now (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04); bounds whichever column --sort selects")
	untilFlag := fs.String("until", "", "stop at this point (same forms as --since); on its own it keeps the cap, giving the newest rows before that instant")
	jsonFlag := fs.Bool("json", false, "print the raw items as a JSON array")
	fs.Parse(args)

	q := url.Values{}
	if *codeFlag != "" {
		q.Set("error_code", *codeFlag)
	}
	if *statusFlag != "" {
		q.Set("status", *statusFlag)
	}
	q.Set("sort", *sortFlag)
	// The only list with a choice of sort, so the only one where --since has a column to
	// pair with: "updated" bounds updated_at, anything else the default created_at.
	sinceCol := "created_at"
	if *sortFlag == "updated" {
		sinceCol = "updated_at"
	}
	limit := applyWindow(q, *sinceFlag, *untilFlag, sinceCol, listCap)
	u := *serverFlag + "/instances?" + q.Encode()
	note := func(capped bool) {
		noteCapped(capped, fmt.Sprintf("the newest %d instances", listCap), "--since")
	}

	if *jsonFlag {
		var items []json.RawMessage
		capped, err := fetchOrdered(u, limit, newestFirst, func(page []json.RawMessage) error {
			items = append(items, page...)
			return nil
		})
		if err != nil {
			fatal("%v", err)
		}
		printJSONItems(items)
		note(capped)
		return
	}

	type instanceRow struct {
		ID        string `json:"id"`
		Process   string `json:"process"`
		Version   int    `json:"version"`
		Status    string `json:"status"`
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}

	// A tabwriter sizes its columns from everything written before Flush, so this one
	// cannot stream: it buffers whichever way the rows arrive, and the header is written
	// lazily so an empty list says so instead of printing a bare header.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows := 0
	capped, err := fetchOrdered(u, limit, newestFirst, func(page []instanceRow) error {
		for _, r := range page {
			if rows == 0 {
				fmt.Fprintln(w, "ID\tSTATUS\tPROCESS\tUPDATED\tCREATED\tCODE\tERROR")
			}
			rows++
			errMsg := r.Error
			if len(errMsg) > 50 {
				errMsg = errMsg[:47] + "..."
			}
			fmt.Fprintf(w, "%s\t%s\t%s@v%d\t%s\t%s\t%s\t%s\n",
				r.ID, r.Status, r.Process, r.Version,
				shortTime(r.UpdatedAt), shortTime(r.CreatedAt), r.ErrorCode, errMsg)
		}
		return nil
	})
	if err != nil {
		fatal("%v", err)
	}
	if rows == 0 {
		fmt.Println("no instances")
		return
	}
	w.Flush()
	note(capped)
}

// runDefinitionsCmd lists the registry, newest-registered first like every other list.
// --sort name gives alphabetical order instead, under which --since is a filter over
// created_at rather than the point the walk starts from, so it does not lift the cap.
func runDefinitionsCmd(server string, args []string) {
	fs := flag.NewFlagSet("definitions", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	sortFlag := fs.String("sort", "created", "sort key: created (newest registered first) or name")
	sinceFlag := fs.String("since", "", "read forward from this point: a duration back from now (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04)")
	untilFlag := fs.String("until", "", "stop at this point (same forms as --since); on its own it keeps the cap, giving the newest rows before that instant")
	jsonFlag := fs.Bool("json", false, "print the raw items as a JSON array")
	fs.Parse(args)

	q := url.Values{}
	q.Set("sort", *sortFlag)
	limit := applyWindow(q, *sinceFlag, *untilFlag, "created_at", listCap)
	// Under --sort name the cap keeps the *first* N alphabetically, not the last, and
	// --since still lifts it — created_at is then a filter over the window rather than
	// the point the walk starts from, but the walk (A→Z) is finite either way.
	dir := newestFirst
	shown := "the newest %d definitions"
	if *sortFlag == "name" {
		dir, shown = firstFirst, "the first %d definitions"
	}
	u := *serverFlag + "/definitions?" + q.Encode()
	note := func(capped bool) {
		noteCapped(capped, fmt.Sprintf(shown, listCap), "--since")
	}

	if *jsonFlag {
		var items []json.RawMessage
		capped, err := fetchOrdered(u, limit, dir, func(page []json.RawMessage) error {
			items = append(items, page...)
			return nil
		})
		if err != nil {
			fatal("%v", err)
		}
		printJSONItems(items)
		note(capped)
		return
	}

	type defRow struct {
		Name      string   `json:"name"`
		Version   int      `json:"version"`
		CreatedAt string   `json:"created_at"`
		Raises    []string `json:"raises"`
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows := 0
	capped, err := fetchOrdered(u, limit, dir, func(page []defRow) error {
		for _, r := range page {
			if rows == 0 {
				fmt.Fprintln(w, "NAME\tVERSION\tREGISTERED\tRAISES")
			}
			rows++
			fmt.Fprintf(w, "%s\tv%d\t%s\t%s\n",
				r.Name, r.Version, shortTime(r.CreatedAt), strings.Join(r.Raises, ", "))
		}
		return nil
	})
	if err != nil {
		fatal("%v", err)
	}
	if rows == 0 {
		fmt.Println("no definitions")
		return
	}
	w.Flush()
	note(capped)
}

func runExternalTasksCmd(server string, args []string) {
	fs := flag.NewFlagSet("external-tasks", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	processFlag := fs.String("process", "", "filter by process name")
	versionFlag := fs.Int("version", 0, "filter by process version (0 = any)")
	taskFlag := fs.String("task", "", "filter by task id")
	sinceFlag := fs.String("since", "", "read forward from this point: a duration back from now (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04)")
	untilFlag := fs.String("until", "", "stop at this point (same forms as --since); on its own it keeps the cap, giving the newest rows before that instant")
	jsonFlag := fs.Bool("json", false, "print the raw items as a JSON array (includes each task's input and result_schema)")
	fs.Parse(args)

	q := url.Values{}
	if *processFlag != "" {
		q.Set("process", *processFlag)
	}
	if *versionFlag != 0 {
		q.Set("version", strconv.Itoa(*versionFlag))
	}
	if *taskFlag != "" {
		q.Set("task", *taskFlag)
	}
	// updated_at is the park time — the WAITING column and this queue's only sort.
	limit := applyWindow(q, *sinceFlag, *untilFlag, "updated_at", listCap)
	u := *serverFlag + "/external-tasks"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	note := func(capped bool) {
		noteCapped(capped, fmt.Sprintf("the newest %d waiting tasks", listCap), "--since")
	}

	if *jsonFlag {
		var items []json.RawMessage
		capped, err := fetchOrdered(u, limit, newestFirst, func(page []json.RawMessage) error {
			items = append(items, page...)
			return nil
		})
		if err != nil {
			fatal("%v", err)
		}
		printJSONItems(items)
		note(capped)
		return
	}

	// The queue never exposes process context, so these fields mirror ExternalTaskResp.
	// The table shows only the addressable columns; --json carries input + result_schema.
	type taskRow struct {
		Token        string `json:"token"`
		Process      string `json:"process"`
		Version      int    `json:"version"`
		TaskID       string `json:"task_id"`
		WaitingSince string `json:"waiting_since"`
	}

	// TOKEN goes last (it is long) and is what you pass to `genctl resolve`.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows := 0
	capped, err := fetchOrdered(u, limit, newestFirst, func(page []taskRow) error {
		for _, r := range page {
			if rows == 0 {
				fmt.Fprintln(w, "WAITING\tPROCESS\tTASK\tTOKEN")
			}
			rows++
			fmt.Fprintf(w, "%s\t%s@v%d\t%s\t%s\n",
				shortTime(r.WaitingSince), r.Process, r.Version, r.TaskID, r.Token)
		}
		return nil
	})
	if err != nil {
		fatal("%v", err)
	}
	if rows == 0 {
		fmt.Println("no external tasks waiting")
		return
	}
	w.Flush()
	note(capped)
}

// Caps for a list command that names no start point (--since/--from lifts the cap by
// saying where to begin — one control per list). logs is larger because a trail is
// read as a trail, not scanned as a table.
const (
	logTailDefault = 200
	listCap        = 20
)

// noteCapped tells the reader on stderr that the cap dropped rows, naming the flag that
// reaches past it. stderr so it never lands in a pipe beside the rows; silent when nothing
// was dropped, so it can never send anyone chasing rows that do not exist.
func noteCapped(capped bool, shown, lift string) {
	if capped {
		fmt.Fprintf(os.Stderr, "genctl: showing %s — pass %s to read further\n", shown, lift)
	}
}

// applyWindow turns --since/--until into the endpoint's bounds on col and returns the
// fetch limit: listCap until --since names a start (only --since lifts it; --until alone
// stays capped — the newest N before that instant). col must match the active sort's key.
func applyWindow(q url.Values, since, until, col string, cap int) int {
	prefix := strings.TrimSuffix(col, "_at")
	set := func(flag, value, suffix string) {
		ms, err := parseWhen(flag, value)
		if err != nil {
			fatal("%v", err)
		}
		q.Set(prefix+suffix, strconv.FormatInt(ms, 10))
	}
	if until != "" {
		set("--until", until, "_before")
	}
	if since == "" {
		return cap
	}
	set("--since", since, "_after")
	return 0
}

func runLogsCmd(server string, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	levelFlag := fs.String("level", "", "filter by level (debug, info, warn, error); empty = all")
	sinceFlag := fs.String("since", "", "read forward from this point: a duration back from now (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04); empty = the newest 200 entries")
	untilFlag := fs.String("until", "", "stop at this point (same forms as --since); on its own it keeps the cap, giving the newest rows before that instant")
	recursiveFlag := fs.Bool("recursive", false, "include the whole process subtree (root instance id)")
	resolveFlag := fs.Bool("resolve", false, "inline full externalized payloads instead of a preview + reference")
	modeFlag := fs.String("mode", "detail", "output: basic (no data body), detail (+ data), or json (one JSON object per line, untruncated)")
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
		q.Set("level", *levelFlag)
	}
	// created_at is a trail's only order, so --since needs no column to pair with.
	limit := applyWindow(q, *sinceFlag, *untilFlag, "created_at", logTailDefault)
	if *recursiveFlag {
		q.Set("recursive", "true")
	}
	if *resolveFlag {
		q.Set("resolve", "true")
	}
	u := *serverFlag + "/instances/" + url.PathEscape(id) + "/logs"
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

	type logDataRef struct {
		Ref  string `json:"ref"`
		Size int64  `json:"size"`
	}
	type logRow struct {
		Time     string         `json:"time"`
		Instance string         `json:"instance"`
		Level    string         `json:"level"`
		Event    string         `json:"event"`
		Task     string         `json:"task"`
		Message  string         `json:"message"`
		Code     string         `json:"code"`
		Data     string         `json:"data"`
		DataRef  *logDataRef    `json:"data_ref"`
		Meta     map[string]any `json:"meta"`
	}
	// Shared logview layout, so a row reads identically here and on the server console. The
	// header waits for the first row (an empty trail prints nothing); day carries the last
	// date rendered so each new day gets a DateBreak. Both fetchOrdered paths render here.
	header, day := false, ""
	capped, err := fetchOrdered(u, limit, newestFirst, func(rows []logRow) error {
		for _, l := range rows {
			if !header {
				fmt.Fprintln(out, logview.Header(style, *recursiveFlag))
				header = true
			}
			t, ok := parseTime(l.Time)
			if d := t.Format("2006-01-02"); ok && !style.CarriesDate() && d != day {
				fmt.Fprintln(out, logview.DateBreak(t))
				day = d
			}
			// An externalized payload comes back as a bare {ref, size} reference with no
			// inline body — show the reference itself in the body's place (rendered raw via
			// the leading "{"). Pass --resolve to fetch and inline the full value instead.
			data := l.Data
			if data == "" && l.DataRef != nil {
				if b, err := json.Marshal(l.DataRef); err == nil {
					data = string(b)
				}
			}
			rec := logview.Record{Event: l.Event, Task: l.Task, Msg: l.Message, Code: l.Code, Data: data, Meta: l.Meta}
			idTag := ""
			if *recursiveFlag {
				idTag = shortID(l.Instance)
			}
			fmt.Fprintln(out, logview.RenderEvent(style, t, l.Level, idTag, l.Event, l.Task, rec.Detail(mode), *recursiveFlag))
		}
		return out.Flush()
	})
	if err != nil {
		fatalFlushing("%v", err)
	}
	out.Flush()
	noteIfCapped(capped)
}

func inputValidationError(err error) (string, bool) {
	return serverErrorDetail(err, "input validation: ")
}

func resultValidationError(err error) (string, bool) {
	return serverErrorDetail(err, "result validation: ")
}

// serverErrorDetail returns the part of err's message after marker, if present.
func serverErrorDetail(err error, marker string) (string, bool) {
	s := err.Error()
	if i := strings.Index(s, marker); i >= 0 {
		return s[i+len(marker):], true
	}
	return "", false
}

func runPauseCmd(server string, args []string) {
	fs := flag.NewFlagSet("pause", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	id := instanceIDAndFlags(fs, args)

	if err := call(*serverFlag+"/instances/"+url.PathEscape(id)+"/pause", http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("paused: %s\n", id)
}

func runResumeCmd(server string, args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	id := instanceIDAndFlags(fs, args)

	if err := call(*serverFlag+"/instances/"+url.PathEscape(id)+"/resume", http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("resumed: %s\n", id)
}

func runRetryCmd(server string, args []string) {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	forceFlag := fs.Bool("force", false, "override only_once retry protection")
	id := instanceIDAndFlags(fs, args)

	u := *serverFlag + "/instances/" + url.PathEscape(id) + "/retry"
	if *forceFlag {
		u += "?force=true"
	}
	if err := call(u, http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("retried: %s\n", id)
}

func runLastCmd(args []string) {
	fmt.Println(resolveInstanceID("@last"))
}

func loadDefs(files []string) ([]any, error) {
	var all []any
	for _, path := range files {
		docs, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, docs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no process definitions found in provided files")
	}
	return all, nil
}

func readFile(path string) ([]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		var doc any
		if err := numeric.Decode(data, &doc); err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		if arr, ok := doc.([]any); ok {
			return arr, nil
		}
		return []any{doc}, nil
	}

	var docs []any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		// Decode into a node rather than an `any`: yaml collapses a number too
		// large for int64 into a float64, which would corrupt a long id in a
		// definition before it was ever uploaded. See yamlToAny.
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
		doc, err := yamlToAny(&node)
		if err != nil {
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
		if doc == nil {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func runConfigCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: genctl config get <key>")
		fmt.Fprintln(os.Stderr, "       genctl config set <key> <value>")
		os.Exit(1)
	}
	sub, key := args[0], args[1]
	switch sub {
	case "get":
		cfg := loadConfig()
		switch key {
		case "server":
			if cfg.Server == "" {
				fmt.Println("(not set)")
			} else {
				fmt.Println(cfg.Server)
			}
		default:
			fatal("unknown config key %q", key)
		}
	case "set":
		if len(args) < 3 {
			fatal("usage: genctl config set <key> <value>")
		}
		val := args[2]
		cfg := loadConfig()
		switch key {
		case "server":
			cfg.Server = val
		default:
			fatal("unknown config key %q", key)
		}
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		path, _ := configFilePath()
		fmt.Printf("set server = %s  (%s)\n", val, path)
	default:
		fatal("unknown config subcommand %q", sub)
	}
}

// ── version compatibility ─────────────────────────────────────────────────────

// leadingArgs pulls the positional arguments preceding the first flag, then parses the
// rest and appends any trailing ones — so `compat order_pipeline 2 3 --json` and
// `compat --json order_pipeline 2 3` are the same command.
func leadingArgs(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	fs.Parse(args)
	return append(pos, fs.Args()...)
}

// parseSelector turns one --from/--to's repeated values into the API selector: EITHER one
// channel OR name@<version|channel> entries. Mixing is refused, not merged — a channel
// already names a version for every process, so an extra entry is ambiguous.
func parseSelector(side string, values []string) map[string]any {
	channels, pins := 0, map[string]any{}
	for _, v := range values {
		name, ref, ok := strings.Cut(v, "@")
		if !ok {
			channels++
			continue
		}
		if name == "" || ref == "" {
			fatal("--%s %q: expected name@version or name@channel", side, v)
		}
		if _, dup := pins[name]; dup {
			// Silently keeping one would compare a version the user did not choose.
			fatal("--%s names %q twice; a side carries one version per process", side, name)
		}
		if n, err := strconv.Atoi(ref); err == nil {
			pins[name] = n
		} else {
			pins[name] = ref
		}
	}
	switch {
	case channels > 0 && len(pins) > 0:
		fatal("--%s mixes a channel with name@version entries; a side is one or the other", side)
	case channels > 1:
		fatal("--%s names more than one channel; a side carries one version per process", side)
	case channels == 1:
		return map[string]any{"channel": values[0]}
	case len(pins) > 0:
		return map[string]any{"versions": pins}
	}
	return nil
}

func runCompatCmd(server string, args []string) {
	fs := flag.NewFlagSet("compat", flag.ExitOnError)
	var files, fromFlag, toFlag multiFlag
	fs.Var(&files, "f", "definition file to compare against --from (YAML or JSON); repeat for multiple files")
	fs.Var(&fromFlag, "from", "the side instances are running now: a channel, or name@version (repeatable)")
	fs.Var(&toFlag, "to", "the side to compare against: a channel, or name@version (repeatable)")
	serverFlag := addServerFlag(fs, server)
	jsonFlag := fs.Bool("json", false, "print the raw report")
	var ignore multiFlag
	fs.Var(&ignore, "ignore", "excuse a check from the exit code: only `contract` is accepted, since the "+
		"upgrade check answers for rows this deployment already owns. It changes neither what is "+
		"compared nor what is printed")
	pos := leadingArgs(fs, args)

	var from, to map[string]any
	process := ""
	switch {
	case len(files) > 0:
		if len(toFlag) > 0 {
			fatal("-f already names the target side; drop --to")
		}
		if len(fromFlag) == 0 {
			fatal("--from is required with -f: naming only one side hides which two documents were compared")
		}
		defs, err := loadDefs(files)
		if err != nil {
			fatal("%v", err)
		}
		from, to = parseSelector("from", fromFlag), map[string]any{"definitions": defs}
		if len(pos) == 1 {
			process = pos[0]
		}
	case len(pos) == 3:
		// Sugar for the single-process case. The server closes each side over the child
		// versions that version was registered against, so this still compares the graph.
		fromV, err := strconv.Atoi(pos[1])
		toV, err2 := strconv.Atoi(pos[2])
		if err != nil || err2 != nil {
			fatal("usage: genctl compat <process> <from-version> <to-version>")
		}
		process = pos[0]
		from = map[string]any{"versions": map[string]any{process: fromV}}
		to = map[string]any{"versions": map[string]any{process: toV}}
	default:
		if len(fromFlag) == 0 || len(toFlag) == 0 {
			fatal("usage: genctl compat <process> <from> <to> | compat -f <file> --from <sel> | compat --from <sel> --to <sel> [<process>]")
		}
		from, to = parseSelector("from", fromFlag), parseSelector("to", toFlag)
		if len(pos) == 1 {
			process = pos[0]
		}
	}

	body := map[string]any{"from": from, "to": to}
	if process != "" {
		body["process"] = process
	}
	// Forwarded as written: the server owns the vocabulary and the gating, so a token the
	// CLI pre-validated would be a second reading to keep true.
	if len(ignore) > 0 {
		body["ignore"] = []string(ignore)
	}

	if *jsonFlag {
		var raw json.RawMessage
		if err := call(*serverFlag+"/definitions/compat", http.MethodPost, body, &raw); err != nil {
			fatal("%v", err)
		}
		printIndented(raw)
		// --json is a RENDERING, not a mode: it must gate exactly as the report does, or a
		// pipeline that adds it to capture the findings stops failing on them.
		var resp compatReport
		if err := json.Unmarshal(raw, &resp); err != nil {
			fatal("decode compat report: %v", err)
		}
		exitOnBreak(resp)
		return
	}

	var resp compatReport
	if err := call(*serverFlag+"/definitions/compat", http.MethodPost, body, &resp); err != nil {
		fatal("%v", err)
	}
	printCompatReport(resp)
	exitOnBreak(resp)
}

// The compat report as the server sends it. Nothing is parsed out of prose: a finding
// arrives addressed, because a bracket-quoted key may contain a space and no reader can
// split on it. specs/compat-command.md §6d.
type compatReport struct {
	Compatible bool            `json:"compatible"`
	Passes     bool            `json:"passes"`
	Processes  []compatProcess `json:"processes"`
}

type compatProcess struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	// Side and Reason are set only on an unanalysable row.
	Side   string `json:"side"`
	Reason string `json:"reason"`
	// Upgrade is what this deployment's own rows can survive; Contract is what the outside
	// world was written against. Two questions, so two columns.
	Upgrade  compatVerdict `json:"upgrade"`
	Contract compatVerdict `json:"contract"`
	Changed  []compatSlot  `json:"changed"`
	Added    []string      `json:"added"`
	Issues   []compatIssue `json:"issues"`
}

type compatVerdict struct {
	Compatible bool `json:"compatible"`
}

// compatSlot is a slot the author edited. Affects is the question it bears on; empty means
// no check covers it, which is the whole reason the channel exists.
type compatSlot struct {
	Address string   `json:"address"`
	Task    string   `json:"task"`
	Affects []string `json:"affects"`
}

// compatIssue is a value that broke: the schema that was compared, and the path isSubset
// reported inside it.
type compatIssue struct {
	Member  string `json:"member"`
	Address string `json:"address"`
	Task    string `json:"task"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Gating  bool   `json:"gating"`
}

// versionLabel renders a resolved version, or "(new)" for a side carrying none — a
// submitted document, which has no version until it is applied.
func versionLabel(v int) string {
	if v == 0 {
		return "(new)"
	}
	return fmt.Sprintf("v%d", v)
}

// Verdict words. Two questions, and a word each: whether the rows this deployment owns can
// continue, and whether the process still honours what the outside world was written
// against. Folding them into one word was the defect this replaced.
const (
	verdictUpgradable = "upgradable"
	verdictCompatible = "compatible"
	verdictUnchanged  = "unchanged"
	verdictBreaking   = "breaking"
	verdictIgnored    = "ignored"
	verdictNew        = "new"
	// A version that failed its own inference was compared against nothing, so it is
	// breaking-by-default: an answer indistinguishable from "checked, and fine" is worse
	// than no report.
	verdictUnanalysable = "unanalysable"
)

// A row's status, as the server spells it. Distinct from the verdict words above even where
// the two coincide: one says whether the verdicts mean anything, the other is a verdict.
const (
	statusNothingToCompare = "nothing_to_compare"
	statusNew              = "new"
	statusUnanalysable     = "unanalysable"
)

// statusWord is the whole answer for a process carrying no verdicts, and "" for one that was
// compared. It stands for both questions because it is a property of the process rather than
// of either check — unlike a verdict, which must never speak for the member it is not (§6c).
func statusWord(status string) string {
	switch status {
	case statusNothingToCompare:
		return verdictUnchanged
	case statusNew:
		return verdictNew
	case statusUnanalysable:
		return verdictUnanalysable
	}
	return ""
}

// verdictPhrase is a process's whole answer: every member lands in exactly one fate, and none
// is ever left out — an absent member reads as a question that went unanswered (§6c).
func verdictPhrase(p compatProcess) string {
	if word := statusWord(p.Status); word != "" {
		return word
	}
	var breaking, ignored, sound []string
	for _, m := range []struct{ member, word string }{
		{"upgrade", verdictUpgradable}, {"contract", verdictCompatible},
	} {
		v := p.Contract
		if m.member == "upgrade" {
			v = p.Upgrade
		}
		switch {
		case v.Compatible:
			sound = append(sound, m.word)
		case gates(p, m.member):
			breaking = append(breaking, m.member)
		default:
			ignored = append(ignored, m.member)
		}
	}
	return fates(breaking, ignored, sound)
}

// fates is the phrasing both levels share: `,` joins members and `; ` joins fates, because
// one separator doing both jobs leaves `breaking: upgrade, contract` and `breaking: upgrade,
// ignored: contract` telling apart only by lookahead. Problems come first (§6c).
func fates(breaking, ignored, sound []string) string {
	var clauses []string
	for _, f := range []struct {
		head    string
		members []string
	}{
		{verdictBreaking + ": ", breaking}, {verdictIgnored + ": ", ignored}, {"", sound},
	} {
		if len(f.members) > 0 {
			clauses = append(clauses, f.head+strings.Join(f.members, ", "))
		}
	}
	return strings.Join(clauses, "; ")
}

// versionRange names an arrow only where two versions were compared: a row with nothing to
// compare involves ONE version, and printing a second implies a comparison that never ran.
func versionRange(p compatProcess) string {
	if p.Status == statusNothingToCompare || p.Status == statusNew {
		if p.From != 0 {
			return versionLabel(p.From)
		}
		return versionLabel(p.To)
	}
	return versionLabel(p.From) + " → " + versionLabel(p.To)
}

// pad right-pads to a RUNE count: a version range holds an arrow, and %-*s counts bytes.
func pad(s string, w int) string {
	return s + strings.Repeat(" ", w-utf8.RuneCountInString(s))
}

func gates(p compatProcess, member string) bool {
	for _, i := range p.Issues {
		if i.Member == member && i.Gating {
			return true
		}
	}
	return false
}

// printCompatReport prints each process ONCE, the verdict heading the findings it derives
// from. No header row: a block sits between two processes, and a header a screen up answers
// nothing.
func printCompatReport(r compatReport) {
	var nameW, versionW int
	for _, p := range r.Processes {
		nameW = max(nameW, utf8.RuneCountInString(p.Name))
		versionW = max(versionW, utf8.RuneCountInString(versionRange(p)))
	}

	// A blank line separates a block from the process below it and nothing else: a run of
	// processes with no findings is a list, and spacing it out hides that it is one.
	blank := false
	for _, p := range r.Processes {
		if blank {
			fmt.Println()
		}
		fmt.Printf("%s  %s  %s\n",
			pad(p.Name, nameW), pad(versionRange(p), versionW), verdictPhrase(p))
		lines := detailLines(p)
		for _, line := range lines {
			fmt.Println(line)
		}
		blank = len(lines) > 0
	}
}

// exitOnBreak is the gate, and it is deliberately not part of printing: both renderings
// answer the same question, so both must fail the same way (§6d).
func exitOnBreak(r compatReport) {
	if !r.Passes {
		os.Exit(1)
	}
}

// row is one line of the detail block: an address, the phrase saying what it costs, and the
// findings under it. A row is either a slot that changed or a place something broke, never
// both — together they would claim the edit caused the break (§6b).
type row struct {
	address string
	phrase  string
	lines   []string
}

// rowsFor walks findings first, then the changed slots — already only the ones no finding
// accounts for, the server having dropped the rest (§6b). Nothing is filtered here.
func rowsFor(p compatProcess) []row {
	var out []row
	at := map[string]int{}
	for _, i := range p.Issues {
		if _, seen := at[i.Address]; !seen {
			at[i.Address] = len(out)
			out = append(out, row{address: i.Address})
		}
		r := &out[at[i.Address]]
		line := i.Message
		if i.Path != "" {
			line = i.Path + ": " + i.Message
		}
		if !contains(r.lines, line) {
			r.lines = append(r.lines, line)
		}
	}
	// One difference that fails both questions is two findings on the wire, because they
	// gate separately — but it is one line, named for both.
	for i := range out {
		out[i].phrase = breakPhrase(p, out[i].address)
	}
	for _, s := range p.Changed {
		out = append(out, row{address: s.Address, phrase: changedPhrase(s)})
	}
	for _, task := range p.Added {
		out = append(out, row{address: task, phrase: "(added)"})
	}
	return out
}

// breakPhrase names every member that broke at this address, in the grammar the process line
// uses. Unlike that line, a row claims nothing beyond its own address, so a member that broke
// elsewhere is simply absent (§6b) — and one reads `ignored` only where EVERY finding under
// it is excused, which keeps a gating break visible under a finer selection.
func breakPhrase(p compatProcess, address string) string {
	var breaking, ignored []string
	for _, member := range []string{"upgrade", "contract"} {
		var found, gating bool
		for _, i := range p.Issues {
			if i.Address != address || i.Member != member {
				continue
			}
			found = true
			gating = gating || i.Gating
		}
		switch {
		case !found:
		case gating:
			breaking = append(breaking, member)
		default:
			ignored = append(ignored, member)
		}
	}
	return "(" + fates(breaking, ignored, nil) + ")"
}

// changedPhrase distinguishes the two things a clean change can mean, which is the only
// reason slot categories are carried at all: `ok` says a check looked and passed, `not
// judged` that none covers it. `ok` is scoped to its own address and claims nothing wider.
func changedPhrase(s compatSlot) string {
	if len(s.Affects) == 0 {
		return "(not judged)"
	}
	return "(ok)"
}

func detailLines(p compatProcess) []string {
	if p.Status == statusUnanalysable {
		return []string{fmt.Sprintf("  %s side: %s", p.Side, p.Reason)}
	}
	rows := rowsFor(p)

	// The address column is padded here rather than by a tabwriter: the finding lines
	// between two addresses carry no columns, and a tabwriter ends its alignment block at
	// every one of them — so each address would size itself and none would line up.
	width := 0
	for _, r := range rows {
		width = max(width, utf8.RuneCountInString(r.address))
	}
	var out []string
	for _, r := range rows {
		out = append(out, "  "+pad(r.address, width)+"  "+r.phrase)
		for _, line := range r.lines {
			out = append(out, "    "+line)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
