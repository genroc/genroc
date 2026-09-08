package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"genroc/internal/model"
	"genroc/internal/numeric"
)

func runApplyCmd(server string, args []string) {
	fs := newFlagSet("apply", args)
	fs.String("f", "", "definition file or glob; an existing path is never globbed. Takes several, "+
		"and repeats")
	serverFlag := addServerFlag(fs, server)
	channelFlag := fs.String("channel", "latest", "channel to apply definitions to")
	// A check runs everything an apply does except the write — resolvers included, since a
	// document that does not resolve is one that would not apply.
	checkOnly := fs.Bool("check-only", false, "report whether the definitions are valid, "+
		"registering nothing")
	asJSON := fs.Bool("json", false, "print the server's answer: the inferred schemas under "+
		"--check-only, what was registered otherwise")
	files, rest := takeFileValues(args)
	if pos := parseArgs(fs, rest); len(pos) > 0 {
		fatal("%s: unexpected argument. Definitions are named with -f, which takes several:\n"+
			"  genctl %s -f %s", pos[0], fs.Name(), strings.Join(pos, " "))
	}
	files, pathErr := definitionPaths(files)
	if pathErr != nil {
		fatal("%v", pathErr)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: no files given, and no `definitions:` in .genroc")
		os.Exit(1)
	}

	defs, err := resolvedDefs(files)
	if err != nil {
		fatal("%v", err)
	}

	endpoint, method, payload := "/api/definitions/batch", http.MethodPut, any(map[string]any{
		"channel":     *channelFlag,
		"definitions": defs,
	})
	if *checkOnly {
		endpoint, method, payload = "/api/definitions/validate", http.MethodPost, defs
	}
	var raw json.RawMessage
	if err := call(*serverFlag+endpoint, method, payload, &raw); err != nil {
		fatal("%v", err)
	}
	if *asJSON {
		printIndented(raw)
		return
	}

	if *checkOnly {
		var schemas []struct {
			Process string `json:"process"`
		}
		if err := json.Unmarshal(raw, &schemas); err != nil {
			fatal("check: %v", err)
		}
		for _, s := range schemas {
			fmt.Printf("valid: %s\n", s.Process)
		}
		return
	}

	var resp []struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Saved   bool   `json:"saved"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		fatal("apply: %v", err)
	}
	for _, r := range resp {
		status := "saved"
		if !r.Saved {
			status = "unchanged"
		}
		fmt.Printf("%s: %s@v%d\n", status, r.Name, r.Version)
	}
}

// runTypesCmd generates the declarations a resolver's authoring layer needs, without
// building or applying anything. It exists because the editor needs them to exist BEFORE an
// apply ever runs - without it an author's file is red until they apply once.
//
// It contacts no server: the types are inferred locally (inferSchemas), so this runs on every
// edit whether or not one is reachable.
func runTypesCmd(args []string) {
	fs := newFlagSet("types", args)
	fs.String("f", "", "definition file or glob; an existing path is never globbed. Takes several, "+
		"and repeats")
	files, rest := takeFileValues(args)
	if pos := parseArgs(fs, rest); len(pos) > 0 {
		fatal("%s: unexpected argument. Definitions are named with -f, which takes several:\n"+
			"  genctl %s -f %s", pos[0], fs.Name(), strings.Join(pos, " "))
	}
	files, pathErr := definitionPaths(files)
	if pathErr != nil {
		fatal("%v", pathErr)
	}
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: no files given, and no `definitions:` in .genroc")
		os.Exit(1)
	}

	docs, err := loadSourceDocs(files)
	if err != nil {
		fatal("%v", err)
	}
	n, err := resolveDocs(docs, "types")
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
		missingSubcommand("channel")
	}
	if hasHelpArg(args[:1]) {
		helpFor("channel")
		return
	}

	sub, args := args[0], args[1:]
	fs := newFlagSet("channel "+sub, args)
	serverFlag := addServerFlag(fs, server)
	// promote's own selectors. Declared here rather than in a nested flag set because the
	// stdlib parses one set per call, and every other subcommand takes its arguments
	// positionally.
	fromFlag := fs.String("from", "", "promote: source channel")
	toFlag := fs.String("to", "", "promote: target channel")
	processFlag := fs.String("process", "", "promote: limit to this process and its dependency subtree")
	fs.Parse(args)
	rest := fs.Args()

	switch sub {
	case "list":
		if len(rest) < 1 {
			fatal("usage: genctl channel list <process>")
		}
		type channelRow struct {
			Channel   string `json:"channel"`
			Version   int    `json:"version"`
			UpdatedAt string `json:"updated_at"`
			Actor     string `json:"actor"`
		}
		listURL := *serverFlag + "/api/channels?name=" + url.QueryEscape(rest[0])
		resp, err := listAll[channelRow](listURL)
		if err != nil {
			fatal("%v", err)
		}
		for _, e := range resp {
			// A pointer moved before attribution landed has no actor, and one moved by an
			// unauthenticated caller has none either -- both print the move without a name
			// rather than an empty "by".
			trail := ""
			if e.UpdatedAt != "" {
				trail = "   moved " + shortTime(e.UpdatedAt)
			}
			if e.Actor != "" {
				trail += " by " + e.Actor
			}
			fmt.Printf("%s -> v%d%s\n", e.Channel, e.Version, trail)
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

	case "promote":
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

	case "status":
		channelStatus(*serverFlag, rest)

	default:
		fatal("unknown channel subcommand %q", sub)
	}
}

// channelStatus reports the child references a channel's members baked at a version the
// channel no longer points at -- a coherence report, not a listing, which is why it prints
// nothing per clean member.
func channelStatus(server string, rest []string) {
	channel := "latest"
	if len(rest) > 0 {
		channel = rest[0]
	}

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
	if err := call(server+"/api/channels/status", http.MethodPost,
		map[string]any{"channel": channel}, &resp); err != nil {
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
		fmt.Printf("channel %q is coherent\n", channel)
	}
}

func runRunCmd(server string, args []string) {
	fs := newFlagSet("run", args)
	serverFlag := addServerFlag(fs, server)
	channelFlag := fs.String("channel", "", "resolve the version via this channel")
	versionFlag := fs.Int("version", 0, "pin an explicit process version")
	inputFlag := fs.String("input", "", "input as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read input from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set an input field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "print only the new instance id, e.g. id=$(genctl run NAME -q)")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	// The process name is the sole positional, before or after the flags.
	pos := leadingArgs(fs, args)
	if len(pos) == 0 {
		fatal("usage: genctl run <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]")
	}
	if len(pos) > 1 {
		fatal("run starts one process, and %d were named", len(pos))
	}
	process := pos[0]

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

// runResolveCmd submits an outcome for an external task, addressed either way it can be: by
// the queue token a worker claimed it with, or by instance id + --task. The second may arrive
// BEFORE the task arms, in which case the server buffers it FIFO -- which is why the line it
// prints names what happened rather than just the id.
//
// One command because the two are one submission: same payload flags, same error channel,
// same conforming against what the task declares. A token is `<id>.<epoch>`, an instance ref
// a bare id or @last, so the argument says which endpoint it is for.
func runResolveCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl resolve <token> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]\n" +
			"       genctl resolve <instance-id> --task <task-id> [same flags]")
	}

	fs := newFlagSet("resolve", args)
	serverFlag := addServerFlag(fs, server)
	taskFlag := fs.String("task", "", "with an instance id: the external task to deliver to")
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result/payload from a file (path)")
	codeFlag := fs.String("code", "", "answer on the ERROR channel with this code (lower_snake_case, no dots)")
	messageFlag := fs.String("message", "", "with --code: human-readable cause; lands on error.message")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result/payload field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	// The reference is the sole positional, before or after flags; @last resolves here.
	ref := instanceIDOrToken(fs, args)

	// Which of the two the argument is decides the endpoint, so a half-named address is
	// refused rather than sent: neither server call can do anything useful with it.
	byInstance := isInstanceRef(ref)
	switch {
	case byInstance && *taskFlag == "":
		fatal("resolve %s: an instance id needs --task <task-id>; a queue token addresses the task by itself", ref)
	case !byInstance && *taskFlag != "":
		fatal("resolve %s: --task names a task on an INSTANCE, and this is not an instance id — "+
			"a queue token already names one task", ref)
	}

	// A missing --result/-f/--set means an empty result: valid for a task with no
	// result_schema, and rejected by the server otherwise (surfaced below).
	payload, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	endpoint, target := "/api/external-tasks/resolve", map[string]any{"token": ref}
	if byInstance {
		id := resolveInstanceID(ref)
		endpoint, target = "/api/external-tasks/signal", map[string]any{"instance_id": id, "task_id": *taskFlag}
		ref = id
	}

	var resp struct {
		Resolved bool `json:"resolved"`
		Buffered bool `json:"buffered"`
	}
	if err := call(*serverFlag+endpoint, http.MethodPost, outcomeBody(target, payload, *codeFlag, *messageFlag), &resp); err != nil {
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

	line := "resolved: " + ref
	if byInstance {
		state := "delivered"
		if resp.Buffered {
			state = "buffered"
		}
		line = fmt.Sprintf("resolved: %s  task=%s  (%s)", ref, *taskFlag, state)
	}
	if *codeFlag != "" {
		line += " (error " + *codeFlag + ")"
	}
	fmt.Println(line)
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

// instanceView decodes both single-instance endpoints. One struct because the two must agree
// on every field they share: a name that drifts between them reads as an absent value, not as
// an error.
type instanceView struct {
	ID         string `json:"id"`
	Process    string `json:"process"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
	WaitState  string `json:"wait_state"`
	Task       string `json:"task"`
	RetryCount int    `json:"retry_count"`
	// The error this instance REPORTS. The one it CAUGHT is a state slot, and so reaches
	// `detail` only.
	ErrorCode    string         `json:"error_code"`
	ErrorMessage string         `json:"error_message"`
	ErrorData    any            `json:"error_data"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	Output       any            `json:"output"`
	State        map[string]any `json:"state"`
	Objects      []objectEntry  `json:"objects"`
}

func runGetCmd(server string, args []string) {
	fs := newFlagSet("get", args)
	serverFlag := addServerFlag(fs, server)
	jsonFlag := fs.Bool("json", false, "print the raw JSON response")
	resolveFlag := fs.Bool("resolve", false, "fetch the values listed under \"objects\" and put them back where they belong")
	id := instanceIDAndFlags(fs, args)

	// The status endpoint: what the instance reports OUTWARD -- its `output:` block and the
	// error it ended on. The engine's own slots are `detail`, so the everyday read cannot
	// hand back internals nobody asked for.
	inst, raw := fetchInstance(*serverFlag, "/api/instances/"+url.PathEscape(id), *resolveFlag)
	if *jsonFlag {
		printIndented(raw)
		return
	}
	printInstanceHead(inst)
	// The payload the failing clause attached -- the machine-readable half of the error whose
	// prose the head printed. Before the output, because on a failed instance there is none.
	if inst.ErrorData != nil {
		fmt.Println("\nError data:")
		fmt.Println(yamlBlock(withObjectRefs(inst.ErrorData, inst.Objects, "error_data")))
	}
	if inst.Output != nil {
		fmt.Println("\nOutput:")
		fmt.Println(yamlBlock(withObjectRefs(inst.Output, inst.Objects, "output")))
	}
}

func runDetailCmd(server string, args []string) {
	fs := newFlagSet("detail", args)
	serverFlag := addServerFlag(fs, server)
	jsonFlag := fs.Bool("json", false, "print the raw JSON response")
	resolveFlag := fs.Bool("resolve", false, "fetch the values listed under \"objects\" and put them back where they belong")
	id := instanceIDAndFlags(fs, args)

	u := "/api/instances/" + url.PathEscape(id) + "/detail"
	if *resolveFlag {
		// The server splices what fits and leaves the rest listed, so ask it first and then
		// fetch whatever it could not carry: two round trips at most for the small case, and
		// the big values still never pass through a response nobody sized.
		u += "?resolve=true"
	}
	inst, raw := fetchInstance(*serverFlag, u, *resolveFlag)
	if *jsonFlag {
		printIndented(raw)
		return
	}
	printInstanceHead(inst)
	if len(inst.State) > 0 {
		fmt.Println("\nState:")
		fmt.Println(yamlBlock(withObjectRefs(inst.State, inst.Objects, "state")))
	}
}

// One fetch for both views: --resolve has to mean the same thing in each, and the text one
// needs the `objects` section a decode into instanceView alone would drop.
func fetchInstance(server, path string, resolve bool) (instanceView, json.RawMessage) {
	var raw json.RawMessage
	if err := callGet(server+path, &raw); err != nil {
		fatal("%v", err)
	}
	if resolve {
		raw = spliceObjects(server, raw)
	}
	var inst instanceView
	// numeric.Decode, not json.Unmarshal: a large literal must survive the display path.
	// specs/number-precision.md.
	if err := numeric.Decode(raw, &inst); err != nil {
		fatal("decode: %v", err)
	}
	return inst, raw
}

func printInstanceHead(inst instanceView) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", inst.ID)
	fmt.Fprintf(w, "Process:\t%s@v%d\n", inst.Process, inst.Version)
	fmt.Fprintf(w, "Status:\t%s\n", inst.Status)
	// Where the process is, printed right under what is happening to it -- and on a
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
}

func runInstancesCmd(server string, args []string) {
	fs := newFlagSet("instances", args)
	serverFlag := addServerFlag(fs, server)
	statusFlag := fs.String("status", "", "filter by status (running, completed, failing, failed, raised, pausing, paused, cancelling, cancelled)")
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
	fs := newFlagSet("definitions", args)
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
		Actor     string   `json:"actor"`
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows := 0
	capped, err := fetchOrdered(u, limit, dir, func(page []defRow) error {
		for _, r := range page {
			if rows == 0 {
				fmt.Fprintln(w, "NAME\tVERSION\tREGISTERED\tBY\tRAISES")
			}
			rows++
			// A dash for a version deployed before attribution landed, so an absent actor
			// reads as "never recorded" rather than as an empty column.
			fmt.Fprintf(w, "%s\tv%d\t%s\t%s\t%s\n",
				r.Name, r.Version, shortTime(r.CreatedAt), dashIfEmpty(r.Actor),
				strings.Join(r.Raises, ", "))
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
	fs := newFlagSet("pause", args)
	serverFlag := addServerFlag(fs, server)
	ids := instanceIDsAndFlags(fs, args)

	eachInstance(ids, "paused", func(id string) (model.Outcome, error) {
		return assert(*serverFlag + "/api/instances/" + url.PathEscape(id) + "/pause")
	})
}

func runResumeCmd(server string, args []string) {
	fs := newFlagSet("resume", args)
	serverFlag := addServerFlag(fs, server)
	ids := instanceIDsAndFlags(fs, args)

	eachInstance(ids, "resumed", func(id string) (model.Outcome, error) {
		return assert(*serverFlag + "/api/instances/" + url.PathEscape(id) + "/resume")
	})
}

func runCancelCmd(server string, args []string) {
	fs := newFlagSet("cancel", args)
	serverFlag := addServerFlag(fs, server)
	ids := instanceIDsAndFlags(fs, args)

	eachInstance(ids, "cancelled", func(id string) (model.Outcome, error) {
		return assert(*serverFlag + "/api/instances/" + url.PathEscape(id) + "/cancel")
	})
}

func runRetryCmd(server string, args []string) {
	fs := newFlagSet("retry", args)
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

// parseArgs parses flags that appear ANYWHERE among the positional arguments, and returns the
// positionals. flag.Parse stops at the first non-flag, so `--channel prod` after a positional
// would go unparsed. Parsing one positional at a time and resuming is the way round it.
func parseArgs(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return pos
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// leadingArgs is parseArgs; kept as a name because compat reads better with it.
func leadingArgs(fs *flag.FlagSet, args []string) []string { return parseArgs(fs, args) }
