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
	"time"
	"unicode/utf8"

	"genroc/internal/logview"

	"gopkg.in/yaml.v3"

	"crypto/rand"
	"encoding/base64"
	"genroc/internal/model"
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

	defs, err := resolvedDefs(files, *serverFlag)
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
	if err := call(*serverFlag+"/api/definitions/batch", http.MethodPut, body, &resp); err != nil {
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

	defs, err := resolvedDefs(files, *serverFlag)
	if err != nil {
		fatal("%v", err)
	}

	var raw json.RawMessage
	if err := call(*serverFlag+"/api/definitions/validate", http.MethodPost, defs, &raw); err != nil {
		fatal("%v", err)
	}
	printIndented(raw)
}

// runTypesCmd generates the declarations a resolver's authoring layer needs, without
// building or applying anything. It exists because the editor needs them to exist BEFORE an
// apply ever runs - without it an author's file is red until they apply once.
func runTypesCmd(server string, args []string) {
	fs := flag.NewFlagSet("types", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "f", "definition file (YAML or JSON); repeat for multiple files")
	serverFlag := addServerFlag(fs, server)
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: -f is required")
		os.Exit(1)
	}

	docs, err := loadSourceDocs(files)
	if err != nil {
		fatal("%v", err)
	}
	n, err := resolveDocs(docs, *serverFlag, "types")
	if err != nil {
		fatal("%v", err)
	}
	if n == 0 {
		fmt.Println("no imports found - nothing to generate")
		return
	}
	fmt.Printf("generated types for %d import(s)\n", n)
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
		listURL := *serverFlag + "/api/channels?name=" + url.QueryEscape(rest[0])
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
		if err := call(*serverFlag+"/api/channels", http.MethodPut,
			map[string]any{"name": rest[0], "channel": rest[1], "version": v}, nil); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set: %s@%s -> v%d\n", rest[0], rest[1], v)

	case "delete":
		if len(rest) < 2 {
			fatal("usage: genctl channel delete <process> <channel>")
		}
		if err := call(*serverFlag+"/api/channels", http.MethodDelete,
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
	if err := call(*serverFlag+"/api/channels/promote", http.MethodPost, body, &resp); err != nil {
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
	if err := call(*serverFlag+"/api/channels/status", http.MethodPost,
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
	if err := call(*serverFlag+"/api/instances", http.MethodPost, body, &resp); err != nil {
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
		fatal("usage: genctl resolve <token> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]")
	}
	token := args[0]

	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result/payload from a file (path)")
	codeFlag := fs.String("code", "", "answer on the ERROR channel with this code (lower_snake_case, no dots)")
	messageFlag := fs.String("message", "", "with --code: human-readable cause; lands on error.message")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result/payload field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(args[1:])

	// A missing --result/-f/--set means an empty result: valid for a task with no
	// result_schema, and rejected by the server otherwise (surfaced below).
	payload, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := outcomeBody(map[string]any{"token": token}, payload, *codeFlag, *messageFlag)

	var resp struct {
		Resolved bool `json:"resolved"`
	}
	if err := call(*serverFlag+"/api/external-tasks/resolve", http.MethodPost, body, &resp); err != nil {
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
	if *codeFlag != "" {
		fmt.Printf("resolved: %s (error %s)\n", token, *codeFlag)
		return
	}
	fmt.Printf("resolved: %s\n", token)
}

// outcomeBody puts the payload on the channel --code selects: the error half when a code is
// given, the result half otherwise. Shared by resolve and signal so the two spell one
// submission the same way.
func outcomeBody(body map[string]any, payload any, code, message string) map[string]any {
	if code == "" {
		body["result"] = payload
		return body
	}
	if message == "" {
		fatal("--message is required with --code")
	}
	fail := map[string]any{"code": code, "message": message}
	if payload != nil {
		fail["data"] = payload
	}
	body["error"] = fail
	return body
}

// runSignalCmd delivers an outcome to an instance's external task by id + --task (not a queue
// token like resolve): resolved now if the task is armed, else buffered FIFO until armed.
func runSignalCmd(server string, args []string) {
	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	taskFlag := fs.String("task", "", "the external task id to signal")
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result/payload from a file (path)")
	codeFlag := fs.String("code", "", "answer on the ERROR channel with this code (lower_snake_case, no dots)")
	messageFlag := fs.String("message", "", "with --code: human-readable cause; lands on error.message")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result/payload field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	// The instance id is the sole positional (before or after flags); resolves @last.
	id := instanceIDAndFlags(fs, args)

	if *taskFlag == "" {
		fatal("usage: genctl signal <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]")
	}

	payload, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := outcomeBody(map[string]any{"instance_id": id, "task_id": *taskFlag}, payload, *codeFlag, *messageFlag)

	var resp struct {
		Delivered bool `json:"delivered"`
		Buffered  bool `json:"buffered"`
	}
	if err := call(*serverFlag+"/api/external-tasks/signal", http.MethodPost, body, &resp); err != nil {
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
	resolveFlag := fs.Bool("resolve", false, "fetch the values listed under \"objects\" and put them back where they belong")
	id := instanceIDAndFlags(fs, args)

	// The detail endpoint: `get` shows what the instance HOLDS, which is state -- the status
	// endpoint carries only what an instance reports outward.
	u := *serverFlag + "/api/instances/" + url.PathEscape(id) + "/detail"
	if *resolveFlag {
		// The server splices what fits and leaves the rest listed, so ask it first and then
		// fetch whatever it could not carry: two round trips at most for the small case, and
		// the big values still never pass through a response nobody sized.
		u += "?resolve=true"
	}
	if *jsonFlag {
		var raw json.RawMessage
		if err := callGet(u, &raw); err != nil {
			fatal("%v", err)
		}
		if *resolveFlag {
			raw = spliceObjects(*serverFlag, raw)
		}
		printIndented(raw)
		return
	}

	var inst struct {
		ID         string `json:"id"`
		Process    string `json:"process"`
		Version    int    `json:"version"`
		Status     string `json:"status"`
		WaitState  string `json:"wait_state"`
		Task       string `json:"task"`
		RetryCount int    `json:"retry_count"`
		// The error this instance REPORTS. The one it CAUGHT is a context key, printed below
		// with the rest of the state it stopped holding.
		ErrorCode    string         `json:"error_code"`
		ErrorMessage string         `json:"error_message"`
		CreatedAt    string         `json:"created_at"`
		UpdatedAt    string         `json:"updated_at"`
		State        map[string]any `json:"state"`
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
	if inst.ErrorMessage != "" {
		fmt.Fprintf(w, "Error:\t%s\n", inst.ErrorMessage)
	}
	if inst.ErrorCode != "" {
		fmt.Fprintf(w, "Code:\t%s\n", inst.ErrorCode)
	}
	w.Flush()

	if len(inst.State) > 0 {
		fmt.Println("\nState:")
		b, _ := json.MarshalIndent(inst.State, "", "  ")
		os.Stdout.Write(b)
		os.Stdout.Write([]byte("\n"))
	}
}

func runInstancesCmd(server string, args []string) {
	fs := flag.NewFlagSet("instances", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	statusFlag := fs.String("status", "", "filter by status (running, completed, failing, failed, raised, pausing, paused)")
	codeFlag := fs.String("error-code", "", "filter by exact error code (e.g. card_declined, http.500)")
	processFlag := fs.String("process", "", "filter by exact process name, across every version")
	versionFlag := fs.Int("version", 0, "filter by exact process version; with --process, that process at that version")
	sortFlag := fs.String("sort", "created", "sort key: created or updated (most recently active)")
	sinceFlag := fs.String("since", "", "read forward from this point: a duration back from now (2h, 45m) or a timestamp (2006-01-02, 2006-01-02 15:04); bounds whichever column --sort selects")
	untilFlag := fs.String("until", "", "stop at this point (same forms as --since); on its own it keeps the cap, giving the newest rows before that instant")
	jsonFlag := fs.Bool("json", false, "print the raw items as a JSON array")
	childrenFlag := fs.Bool("children", false, "include child instances; by default the listing is roots only, one row per tree")
	quietFlag := fs.Bool("quiet", false, "print only instance ids, one per line — the form to nest in another command")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(args)

	if *quietFlag && *jsonFlag {
		// Both are machine forms and they disagree about the shape; picking one silently
		// would give a script the other one's output.
		fatal("--json and -q are two machine-readable forms of this list; pass one")
	}

	q := url.Values{}
	if *childrenFlag {
		q.Set("children", "true")
	}
	if *codeFlag != "" {
		q.Set("error_code", *codeFlag)
	}
	if *statusFlag != "" {
		q.Set("status", *statusFlag)
	}
	if *processFlag != "" {
		q.Set("process", *processFlag)
	}
	if *versionFlag != 0 {
		q.Set("version", strconv.Itoa(*versionFlag))
	}
	q.Set("sort", *sortFlag)
	// The only list with a choice of sort, so the only one where --since has a column to
	// pair with: "updated" bounds updated_at, anything else the default created_at.
	sinceCol := "created_at"
	if *sortFlag == "updated" {
		sinceCol = "updated_at"
	}
	limit := applyWindow(q, *sinceFlag, *untilFlag, sinceCol, listCap)
	u := *serverFlag + "/api/instances?" + q.Encode()
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

	// -q: ids only, so `genctl pause $(genctl instances -q --status running)` passes the
	// list straight to a lifecycle command. Nothing else may reach stdout on this path —
	// an empty list must print NOTHING, because "no instances" would arrive at the outer
	// command as two arguments. The cap notice stays on stderr, where it already was: a
	// truncated list here silently pauses 20 of 50.
	if *quietFlag {
		type idRow struct {
			ID string `json:"id"`
		}
		capped, err := fetchOrdered(u, limit, newestFirst, func(page []idRow) error {
			for _, r := range page {
				fmt.Println(r.ID)
			}
			return nil
		})
		if err != nil {
			fatal("%v", err)
		}
		note(capped)
		return
	}

	type instanceRow struct {
		ID           string `json:"id"`
		ParentID     string `json:"parent_id"`
		Process      string `json:"process"`
		Version      int    `json:"version"`
		Status       string `json:"status"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
	}

	// A tabwriter sizes its columns from everything written before Flush, so this one
	// cannot stream: it buffers whichever way the rows arrive, and the header is written
	// lazily so an empty list says so instead of printing a bare header.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows := 0
	capped, err := fetchOrdered(u, limit, newestFirst, func(page []instanceRow) error {
		for _, r := range page {
			if rows == 0 {
				fmt.Fprintln(w, "ID\tSTATUS\tPROCESS"+parentCol("\tPARENT", *childrenFlag)+"\tUPDATED\tCREATED\tCODE\tERROR")
			}
			rows++
			errMsg := r.ErrorMessage
			if len(errMsg) > 50 {
				errMsg = errMsg[:47] + "..."
			}
			// The PARENT column appears only with --children: without it every row is a root
			// and the column would be a wasted width, but WITH it nothing else on the row says
			// which of the two a line is.
			fmt.Fprintf(w, "%s\t%s\t%s@v%d%s\t%s\t%s\t%s\t%s\n",
				r.ID, r.Status, r.Process, r.Version,
				parentCol("\t"+dashIfEmpty(r.ParentID), *childrenFlag),
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
	u := *serverFlag + "/api/definitions?" + q.Encode()
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

// Caps for a list command that names no start point (--since/--from lifts the cap by
// saying where to begin — one control per list). logs is larger because a trail is
// read as a trail, not scanned as a table.
const (
	logTailDefault = 200
	listCap        = 20
)

func parentCol(cell string, children bool) string {
	if children {
		return cell
	}
	return ""
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

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
		Data     json.RawMessage `json:"data"`
		Meta     map[string]any  `json:"meta"`
		Objects  []objectEntry   `json:"objects"`
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
			rec := logview.Record{Event: l.Event, Task: l.Task, Msg: l.Message, Code: l.Code, Data: logData(l.Data, l.Objects), Meta: l.Meta}
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
	ids := instanceIDsAndFlags(fs, args)

	eachInstance(ids, "paused", func(id string) (model.Outcome, error) {
		return assert(*serverFlag + "/api/instances/" + url.PathEscape(id) + "/pause")
	})
}

func runResumeCmd(server string, args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	ids := instanceIDsAndFlags(fs, args)

	eachInstance(ids, "resumed", func(id string) (model.Outcome, error) {
		return assert(*serverFlag + "/api/instances/" + url.PathEscape(id) + "/resume")
	})
}

func runRetryCmd(server string, args []string) {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	forceFlag := fs.Bool("force", false, "override only_once retry protection")
	ids := instanceIDsAndFlags(fs, args)

	eachInstance(ids, "retried", func(id string) (model.Outcome, error) {
		u := *serverFlag + "/api/instances/" + url.PathEscape(id) + "/retry"
		if *forceFlag {
			u += "?force=true"
		}
		return assert(u)
	})
}

func runLastCmd(args []string) {
	fmt.Println(resolveInstanceID("@last"))
}

func loadDefs(files []string) ([]any, error) {
	docs, err := loadSourceDocs(files)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(docs))
	for i, d := range docs {
		out[i] = d.doc
	}
	return out, nil
}

// loadSourceDocs is loadDefs keeping the file each document came from: a directive's path
// resolves against it, and an error has to name it.
func loadSourceDocs(files []string) ([]sourceDoc, error) {
	var all []sourceDoc
	for _, path := range files {
		docs, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, d := range docs {
			all = append(all, sourceDoc{doc: d, file: path})
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no process definitions found in provided files")
	}
	return all, nil
}

// resolvedDefs loads, resolves every import directive, and hands back the plain documents
// the API takes. By this point no directive remains — the server has no resolver.
func resolvedDefs(files []string, server string) ([]any, error) {
	docs, err := loadSourceDocs(files)
	if err != nil {
		return nil, err
	}
	if _, err := resolveDocs(docs, server, "build"); err != nil {
		return nil, err
	}
	out := make([]any, len(docs))
	for i, d := range docs {
		out[i] = d.doc
	}
	return out, nil
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
		fmt.Fprintln(os.Stderr, "Usage: genctl config get <key>          (server, token)")
		fmt.Fprintln(os.Stderr, "       genctl config set <key> <value>")
		fmt.Fprintln(os.Stderr, "       genctl config unset <key>")
		os.Exit(1)
	}
	sub, key := args[0], args[1]
	switch sub {
	case "get":
		cfg := loadConfig()
		val, err := configValue(cfg, key)
		if err != nil {
			fatal("%v", err)
		}
		if val == "" {
			fmt.Println("(not set)")
			return
		}
		// A credential is never printed back. `get` is what someone runs to check a setting,
		// often with a colleague watching or a terminal being recorded, and the value is
		// recoverable from the file by whoever owns it anyway.
		if key == "token" {
			fmt.Printf("(set: %s)\n", maskToken(val))
			return
		}
		fmt.Println(val)
	case "set":
		if len(args) < 3 {
			fatal("usage: genctl config set <key> <value>")
		}
		val := args[2]
		cfg := loadConfig()
		switch key {
		case "server":
			cfg.Server = val
		case "token":
			cfg.Token = val
		default:
			fatal("unknown config key %q (server, token)", key)
		}
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		path, _ := configFilePath()
		shown := val
		if key == "token" {
			shown = maskToken(val)
		}
		fmt.Printf("set %s = %s  (%s)\n", key, shown, path)
	case "unset":
		cfg := loadConfig()
		switch key {
		case "server":
			cfg.Server = ""
		case "token":
			cfg.Token = ""
		default:
			fatal("unknown config key %q (server, token)", key)
		}
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		fmt.Printf("unset %s\n", key)
	default:
		fatal("unknown config subcommand %q (get, set, unset)", sub)
	}
}

func configValue(cfg genrocConfig, key string) (string, error) {
	switch key {
	case "server":
		return cfg.Server, nil
	case "token":
		return cfg.Token, nil
	}
	return "", fmt.Errorf("unknown config key %q (server, token)", key)
}

// maskToken shows enough to tell two credentials apart and not enough to use one. The prefix
// is not secret — it is the same on every token — so only the tail is elided.
func maskToken(t string) string {
	const keep = 6
	if len(t) <= len(tokenPrefix)+keep {
		return "…"
	}
	return t[:len(tokenPrefix)+keep] + "…"
}

// tokenPrefix mirrors db.TokenPrefix. Duplicated rather than imported: genctl is a client and
// must not depend on the server's internal packages.
const tokenPrefix = "genroc_sk_"

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

// compatSidesForInstance reads both sides off a row: the side it RUNS is its own process at
// its own version, so only the target is named. The row's process also scopes the report --
// a --to channel carries every process on it. specs/version-compatibility.md s6.
func compatSidesForInstance(server, id string, fromFlag, toFlag, files multiFlag) (map[string]any, map[string]any, string) {
	if len(fromFlag) > 0 {
		fatal("an instance id already names the side it is running; drop --from")
	}
	if len(files) > 0 && len(toFlag) > 0 {
		fatal("-f already names the target side; drop --to")
	}
	var row instanceRow
	if err := callGet(server+"/api/instances/"+id, &row); err != nil {
		fatal("%v", err)
	}
	from := map[string]any{"versions": map[string]any{row.Process: row.Version}}
	switch {
	case len(files) > 0:
		defs, err := resolvedDefs(files, server)
		if err != nil {
			fatal("%v", err)
		}
		return from, map[string]any{"definitions": defs}, row.Process
	case len(toFlag) > 1:
		fatal("--to names %d targets; after an instance id it names one version or channel for %s",
			len(toFlag), row.Process)
	case len(toFlag) == 1:
		if n, err := strconv.Atoi(toFlag[0]); err == nil {
			return from, map[string]any{"versions": map[string]any{row.Process: n}}, row.Process
		}
		return from, map[string]any{"channel": toFlag[0]}, row.Process
	}
	fatal("usage: genctl compat <instance-id> --to <version|channel> | genctl compat <instance-id> -f <file>")
	return nil, nil, ""
}

func runCompatCmd(server string, args []string) {
	fs := flag.NewFlagSet("compat", flag.ExitOnError)
	var files, fromFlag, toFlag multiFlag
	fs.Var(&files, "f", "definition file to compare against --from (YAML or JSON); repeat for multiple files")
	fs.Var(&fromFlag, "from", "the side instances are running now: a channel, or name@version (repeatable). "+
		"An instance id names this side by itself")
	fs.Var(&toFlag, "to", "the side to compare against: a channel, or name@version (repeatable); "+
		"after an instance id, a bare version or channel")
	serverFlag := addServerFlag(fs, server)
	jsonFlag := fs.Bool("json", false, "print the raw report")
	var ignore multiFlag
	fs.Var(&ignore, "ignore", "excuse a check from the exit code: only `contract` is accepted, since the "+
		"upgrade check answers for rows this deployment already owns. It changes neither what is "+
		"compared nor what is printed")
	pos := leadingArgs(fs, args)

	for _, p := range pos {
		if isInstanceRef(p) && len(pos) > 1 {
			// A side carries one version per process (parseSelector's rule), and a second row is
			// a second version -- of the same process, or of one this report is not scoped to.
			fatal("compat takes one instance id: two rows are two comparisons, and a side carries " +
				"one version per process")
		}
	}

	var from, to map[string]any
	process := ""
	switch {
	case len(pos) == 1 && isInstanceRef(pos[0]):
		from, to, process = compatSidesForInstance(*serverFlag, resolveInstanceID(pos[0]), fromFlag, toFlag, files)
	case len(files) > 0:
		if len(toFlag) > 0 {
			fatal("-f already names the target side; drop --to")
		}
		if len(fromFlag) == 0 {
			fatal("--from is required with -f: naming only one side hides which two documents were compared")
		}
		// Resolved, exactly as apply resolves: an unresolved `$import:` leaf is a literal
		// string next to the code a stored version holds, so every site that has one compares
		// changed and the row can never read `unchanged`.
		defs, err := resolvedDefs(files, *serverFlag)
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
			fatal("usage: genctl compat <process> <from> <to>\n" +
				"       genctl compat -f <file> --from <sel>\n" +
				"       genctl compat --from <sel> --to <sel> [<process>]\n" +
				"       genctl compat <instance-id> --to <version|channel>")
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
		if err := call(*serverFlag+"/api/definitions/compat", http.MethodPost, body, &raw); err != nil {
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
	if err := call(*serverFlag+"/api/definitions/compat", http.MethodPost, body, &resp); err != nil {
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

// objectEntry is one row of a response's `objects` section: where a value was cut from, and the
// handle to fetch it with.
type objectEntry struct {
	Path []any  `json:"path"`
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// logData renders a log payload for one row: whatever the entry carried inline, with each
// externalized piece shown as its {ref,size} handle in the place it was cut from. logs never
// fetches -- a trail is scanned, and these payloads are large by definition; `genctl object
// <ref>` gets the one that matters.
func logData(raw json.RawMessage, objects []objectEntry) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	if len(objects) > 0 {
		wrapper := map[string]any{"data": value}
		for _, o := range objects {
			place(wrapper, o.Path, map[string]any{"ref": o.Ref, "size": o.Size})
		}
		value = wrapper["data"]
	}
	if str, ok := value.(string); ok {
		return str
	}
	b, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// spliceObjects fetches every value a response listed under `objects` and puts it back at the
// path it named, which is the whole of what a recipient owes the objects protocol.
//
// The paths are arrays of keys, so walking one needs no parser and no unescaping — that is why
// they are arrays and not JSON Pointers. Client-side because the server materializing every
// value behind a query parameter is an unbounded response nobody asked the size of.
func spliceObjects(server string, raw json.RawMessage) json.RawMessage {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return raw
	}
	entries, _ := body["objects"].([]any)
	if len(entries) == 0 {
		return raw
	}
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		ref, _ := entry["ref"].(string)
		path, _ := entry["path"].([]any)
		if ref == "" || len(path) == 0 {
			continue
		}
		var resp struct {
			Data string `json:"data"`
		}
		if err := callGet(server+"/api/objects/"+url.PathEscape(ref), &resp); err != nil {
			fatal("fetch object %s: %v", ref, err)
		}
		var value any
		if err := json.Unmarshal([]byte(resp.Data), &value); err != nil {
			value = resp.Data // not JSON (a raw log payload): put it back as the string it is
		}
		place(body, path, value)
	}
	delete(body, "objects")
	out, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return out
}

// place walks path and writes value at the end of it. A step that does not exist is skipped
// rather than created: the path came from this same response, so a miss means the response and
// its objects section disagree, and inventing structure would hide that.
func place(root any, path []any, value any) {
	cur := root
	for i, seg := range path {
		last := i == len(path)-1
		switch node := cur.(type) {
		case map[string]any:
			key, ok := seg.(string)
			if !ok {
				return
			}
			if last {
				node[key] = value
				return
			}
			cur = node[key]
		case []any:
			idx, ok := seg.(float64) // JSON numbers decode as float64
			if !ok || int(idx) < 0 || int(idx) >= len(node) {
				return
			}
			if last {
				node[int(idx)] = value
				return
			}
			cur = node[int(idx)]
		default:
			return
		}
	}
}

// runObjectCmd fetches one externalized value by the ref a response listed for it. The escape
// hatch that lets `logs` print an id instead of a payload: a trail is scanned, and the one entry
// you care about is fetched on purpose.
func runObjectCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl object <ref>")
	}
	ref := args[0]
	fs := flag.NewFlagSet("object", flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	fs.Parse(args[1:])

	var resp struct {
		Data string `json:"data"`
	}
	if err := callGet(*serverFlag+"/api/objects/"+url.PathEscape(ref), &resp); err != nil {
		fatal("%v", err)
	}
	fmt.Println(resp.Data)
}

// ── tokens ──────────────────────────────────────────────────────────────────────

// `genctl token` manages API credentials over the API, which means it needs an admin
// credential of its own. The break-glass equivalent that needs none is `genroc token`, run
// against the database by whoever can read it. specs/api-auth.md §5.3.
func runTokenCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl token create --perms <list> [--label <name>] | token generate | token list | token revoke <id>...")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("token "+sub, flag.ExitOnError)
	serverFlag := addServerFlag(fs, server)
	labelFlag := fs.String("label", "", "a name for this token, shown in listings")
	permsFlag := fs.String("perms", "", "comma-separated: admin, deploy, operate, read, worker")
	jsonFlag := fs.Bool("json", false, "print the raw items as a JSON array")
	quietFlag := fs.Bool("quiet", false, "on create, print only the token")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(rest)

	switch sub {
	case "generate":
		// Offline: no server, no credential. This is what makes "the operator supplies the
		// token" possible at all — `create` needs an authenticated server, so it cannot make
		// the FIRST one. Generated here, the secret never originates inside genroc, never
		// reaches its logs, and never rests in its container.
		secret, err := generateTokenSecret()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(secret)
	case "create":
		if *permsFlag == "" {
			fatal("token create: --perms is required (e.g. --perms deploy,read)")
		}
		var perms []string
		for _, p := range strings.Split(*permsFlag, ",") {
			if p = strings.TrimSpace(p); p != "" {
				perms = append(perms, p)
			}
		}
		var resp struct {
			ID    string   `json:"id"`
			Token string   `json:"token"`
			Perms []string `json:"perms"`
		}
		body := map[string]any{"label": *labelFlag, "perms": perms}
		if err := call(*serverFlag+"/api/tokens", http.MethodPost, body, &resp); err != nil {
			fatal("%v", err)
		}
		// The secret alone on stdout, so it composes: TOKEN=$(genctl token create --perms read -q).
		if *quietFlag {
			fmt.Println(resp.Token)
			return
		}
		fmt.Fprintf(os.Stderr, "created %s  perms=%s\n  shown once:\n", resp.ID, strings.Join(resp.Perms, ","))
		fmt.Println(resp.Token)
	case "list":
		// Decoded once as raw items so --json echoes the server verbatim, then per item for
		// the table. Two fields sharing a `json:"items"` tag would silently decode to nothing.
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := callGet(*serverFlag+"/api/tokens", &page); err != nil {
			fatal("%v", err)
		}
		if *jsonFlag {
			printJSONItems(page.Items)
			return
		}
		if len(page.Items) == 0 {
			fmt.Println("no tokens")
			return
		}
		type tokenRow struct {
			ID         string   `json:"id"`
			Label      string   `json:"label"`
			Perms      []string `json:"perms"`
			CreatedAt  string   `json:"created_at"`
			LastUsedAt string   `json:"last_used_at"`
			RevokedAt  string   `json:"revoked_at"`
			ExpiresAt  string   `json:"expires_at"`
		}
		rows := make([]tokenRow, 0, len(page.Items))
		for _, raw := range page.Items {
			var t tokenRow
			if err := json.Unmarshal(raw, &t); err != nil {
				fatal("decode token: %v", err)
			}
			rows = append(rows, t)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tLABEL\tPERMS\tCREATED\tLAST USED\tEXPIRES\tSTATUS")
		now := time.Now().UTC()
		for _, t := range rows {
			// Expiry is a status, not just a column: a lapsed token reported as "live" is the
			// row an operator skips while wondering why the caller gets 401.
			status := "live"
			switch {
			case t.RevokedAt != "":
				status = "revoked"
			case t.ExpiresAt != "":
				if at, err := time.Parse(time.RFC3339, t.ExpiresAt); err == nil && !at.After(now) {
					status = "expired"
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, orDash(t.Label),
				strings.Join(t.Perms, ","), shortTime(t.CreatedAt), orDash(shortTime(t.LastUsedAt)),
				orDash(shortTime(t.ExpiresAt)), status)
		}
		w.Flush()
	case "revoke":
		if fs.NArg() == 0 {
			fatal("usage: genctl token revoke <id>...")
		}
		for _, id := range fs.Args() {
			var resp struct {
				Revoked bool `json:"revoked"`
			}
			if err := call(*serverFlag+"/api/tokens/"+url.PathEscape(id), http.MethodDelete, nil, &resp); err != nil {
				fatal("%v", err)
			}
			fmt.Printf("revoked: %s\n", id)
		}
	default:
		fatal("token: unknown subcommand %q (create, list, revoke)", sub)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// generateTokenSecret mirrors the server's db.NewTokenSecret. Duplicated rather than imported:
// genctl is a client and must not depend on the server's internal packages. The SERVER
// validates the format regardless (db.ValidateTokenSecret), so this is a convenience, not the
// guarantee — an operator can always set the env var by hand.
func generateTokenSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}
