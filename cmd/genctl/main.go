// genctl is a command-line gateway to a running genroc server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
//	genctl validate -f file.yaml [-f file2.yaml ...]
//	genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
//	genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl instances [--status <status>] [--sort updated|created] [--limit <n>] [--json]
//	genctl external-tasks [--process <name>] [--version <n>] [--task <id>] [--limit <n>] [--json]
//	genctl get      <instance-id> [--resolve] [--json]
//	genctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--recursive] [--resolve] [--mode basic|detail|json] <instance-id>
//
// List commands (instances, external-tasks, logs) show the newest --limit items,
// printed oldest→newest so the most recent row is at the bottom, nearest the prompt
// (like tail). --limit pages the server as needed to gather that many.
//
//	genctl pause    <instance-id>
//	genctl resume   <instance-id>
//	genctl retry    [--force] <instance-id>
//	genctl last
//
// get/logs/pause/resume/retry/signal require an instance id; pass @last for the most
// recently started instance (recorded by run). `genctl last` prints that id.
//
//	genctl channel list   <process>
//	genctl channel set    <process> <channel> <version>
//	genctl channel delete <process> <channel>
//	genctl promote  --from <channel> --to <channel> [--process <name>]
//	genctl status   --channel <channel>
//	genctl config   get <key>
//	genctl config   set <key> <value>
//
// Environment:
//
//	GENROC_SERVER  base URL of the genroc server (default: http://localhost:8448)
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// genctl command conventions
//
// Keep new list/get commands consistent so the surface stays predictable:
//
//   - Naming. A resource collection is the plural noun (`instances`,
//     `external-tasks`); a single item takes its id/key as the first positional
//     (`get <id>`). Add a get only when there is something to show beyond the row.
//   - Server & errors. Every command takes `--server` (overrides $GENROC_SERVER and
//     the config file). All failures go through fatal() ("genctl: ..."); surface a
//     server-side validation message via serverErrorDetail/resultValidationError.
//   - List output. Default to a tabwriter table with an UPPERCASE header and
//     shortTime() for timestamps; print "no <things>" when empty. Filters are
//     `--<field>` flags mapped 1:1 to the endpoint's query params. `--limit <n>` is
//     the one paging knob: fetch the newest n via listNewest[T] (which follows the
//     cursor across pages), then slices.Reverse for display so the newest row is at
//     the bottom, nearest the prompt (tail-style).
//   - Single-item output. Default to a `Key:\tvalue` tabwriter block using
//     longTime() for timestamps.
//   - --json is the one machine-readable form. On a list it prints the raw items as
//     a JSON array via printJSONItems (lossless, same newest-last order as the table);
//     on a single item it prints the raw server object (get: callGet into
//     json.RawMessage, then indent). Never invent a per-command machine format.
//
// Deliberate exceptions (special-purpose, not resource list/get — leave them):
//   - `logs` keeps `--mode basic|detail|json`; it has three views and its json is
//     JSONL (one object per line, streaming), not a {items,page} array.
//   - `channel list` prints plain `name -> vN` pointer lines (a projection, not a
//     resource object), and `status` is a coherence report, not a listing.
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cfg := loadConfig()
	server := os.Getenv("GENROC_SERVER")
	if server == "" {
		server = cfg.Server
	}
	if server == "" {
		server = "http://localhost:8448"
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "apply":
		runApplyCmd(server, args)
	case "validate":
		runValidateCmd(server, args)
	case "run":
		runRunCmd(server, args)
	case "resolve":
		runResolveCmd(server, args)
	case "signal":
		runSignalCmd(server, args)
	case "get":
		runGetCmd(server, args)
	case "channel":
		runChannelCmd(server, args)
	case "promote":
		runPromoteCmd(server, args)
	case "status":
		runStatusCmd(server, args)
	case "instances":
		runInstancesCmd(server, args)
	case "external-tasks":
		runExternalTasksCmd(server, args)
	case "logs":
		runLogsCmd(server, args)
	case "pause":
		runPauseCmd(server, args)
	case "resume":
		runResumeCmd(server, args)
	case "retry":
		runRetryCmd(server, args)
	case "last":
		runLastCmd(args)
	case "config":
		runConfigCmd(args)
	default:
		fmt.Fprintf(os.Stderr, "genctl: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// addServerFlag registers the shared --server flag ($GENROC_SERVER) on fs,
// defaulting to def. Every subcommand talks to the server, so this keeps the flag
// name and help text defined in one place.
func addServerFlag(fs *flag.FlagSet, def string) *string {
	return fs.String("server", def, "genroc server base URL ($GENROC_SERVER)")
}

// instanceIDAndFlags parses an instance subcommand's args, where the instance id
// may sit before or after the flags. A leading non-flag token is taken as the id
// (so `get <id> --json` keeps working); otherwise a trailing positional is used (so
// `pause --server X <id>` works too). The id must be given explicitly — a concrete
// id or "@last"; a missing one is an error (see resolveInstanceID).
func instanceIDAndFlags(fs *flag.FlagSet, args []string) string {
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	fs.Parse(args)
	if id == "" {
		id = fs.Arg(0)
	}
	return resolveInstanceID(id)
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
  genctl validate -f file.yaml [-f file2.yaml ...]
  genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
  genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [-q]
  genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
  genctl instances [--status <status>] [--sort updated|created] [--limit <n>] [--json]
  genctl external-tasks [--process <name>] [--version <n>] [--task <id>] [--limit <n>] [--json]
  genctl get      <instance-id> [--resolve] [--json]
  genctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--recursive] [--resolve] [--mode basic|detail|json] <instance-id>
  genctl pause    <instance-id>
  genctl resume   <instance-id>
  genctl retry    [--force] <instance-id>
  genctl last
  genctl channel list   <process>
  genctl channel set    <process> <channel> <version>
  genctl channel delete <process> <channel>
  genctl promote  --from <channel> --to <channel> [--process <name>]
  genctl status   --channel <channel>
  genctl config   get <key>
  genctl config   set <key> <value>

Flags:
  -f        apply: definition file(s), YAML or JSON, multi-doc --- (repeatable);
            run/resolve/signal: read the input/result from a file (path — tab-completes)
  --input   process input: a JSON/YAML literal, or - for stdin
  --result  external-task result (resolve/signal): a JSON/YAML literal, or - for stdin
  --task    the external task id to signal
  --set     input/result field key=value (repeatable; dotted keys nest, values type-inferred)
  --server  genroc server URL (overrides $GENROC_SERVER and config file)
  --limit   list commands: how many of the newest items to show; the CLI pages the
            server as needed to gather that many (printed oldest→newest, newest last)
  --json    machine-readable output: a list (instances/external-tasks) prints its
            raw items as a JSON array; get prints the raw instance object
  --resolve get/logs: inline externalized context values/payloads instead of
            {ref, size} references
  -q        with run, print only the new instance id (id=$(genctl run NAME -q));
            with resolve/signal, suppress the confirmation line

Instance id:
  get/logs/pause/resume/retry/signal require an instance id; pass @last for the most
  recently started instance (recorded by run), or run "genctl last" to print it.

External tasks:
  external-tasks lists the queue of instances waiting on an external result.
  resolve takes a task's resolve token (the "<instance-id>.<nonce>" TOKEN column
  from that list); signal addresses a task by instance id + --task and buffers the
  result if the task is not armed yet.

Config keys:
  server    genroc server base URL`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genctl: "+format+"\n", args...)
	os.Exit(1)
}
