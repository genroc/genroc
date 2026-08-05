// genctl is a command-line gateway to a running genroc server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest]
//	genctl validate -f file.yaml [-f file2.yaml ...]
//	genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
//	genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl instances [--status <status>] [--error-code <code>] [--sort updated|created] [--since <when>] [--until <when>] [--json]
//	genctl definitions [--sort created|name] [--since <when>] [--until <when>] [--json]
//	genctl external-tasks [--process <name>] [--version <n>] [--task <id>] [--since <when>] [--until <when>] [--json]
//	genctl get      <instance-id> [--resolve] [--json]
//	genctl logs     [--level <level>] [--since <when>] [--until <when>] [--time clock|full] [--recursive] [--resolve] [--mode basic|detail|json] <instance-id>
//
// Every list endpoint sorts newest-first. No list command takes --limit: each is capped
// instead — 20 rows, 200 for logs — and says on stderr when the cap dropped anything, so
// nothing is truncated in silence. The cap is a guard against dumping an unbounded table,
// not a page size.
//
// --since lifts it. It takes a duration back from now (2h, 45m) or a timestamp, and is a
// CLI-side helper only: it resolves to unix millis and goes out as the endpoint's
// created_after or updated_after — whichever column the active sort keys on — together
// with order=asc. So past the cap the read walks forward from that point toward now,
// printing pages as they arrive, in the same direction it displays.
//
// --until is its other end, same forms, sent as *_before. The two make the half-open
// window [since, until), so adjacent windows never repeat the row on their boundary. On
// its own --until leaves the cap in place, which reads as "the newest N before then" —
// what was happening at that moment.
//
// Rows print oldest→newest either way, so the most recent is at the bottom, nearest the
// prompt (like tail). logs additionally carries a day separator per date in its time
// column; --time full puts the date on every row instead.
//
// Times display in — and --since/--until are read in — the local zone ($TZ), so a
// timestamp read off a row can be passed straight back. --mode json is the exception: it
// forwards the server's UTC RFC3339 verbatim, so machine output never depends on who ran
// the command. A *duration* resolves against this machine's clock rather than the
// server's; see parseWhen for when that distinction bites.
//
//	genctl pause    <instance-id>
//	genctl resume   <instance-id>
//	genctl retry    [--force] <instance-id>
//	genctl last
//
// get/logs/pause/resume/retry/signal require an instance id; pass @last for the most
// recently started instance (recorded by run). `genctl last` prints that id.
//
//	genctl compat   <process> <from> <to>
//	genctl compat   -f file.yaml [-f ...] --from <channel>
//	genctl compat   --from <channel> --to <channel> [<process>]
//
// compat answers two questions about a pair of versions — could an instance running the
// older one continue under the newer, and does the newer still produce what consumers of
// the older were written against — and reports them as one verdict per process:
// upgradable, breaking, nothing changed, or new. --allow-breaking-output tolerates the
// second question failing. It is a shape check: a change of meaning (dollars to cents)
// compares equal, so the changed-slot list under the table is the deliverable.
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
//     `--<field>` flags mapped 1:1 to the endpoint's query params.
//   - List bounds. No `--limit`. Fetch through fetchOrdered[T](url, limit, dir, emit):
//     pass listCap and the rows arrive as the newest N, flipped to oldest-first; pass 0
//     and they stream forward page by page. applyWindow sets the query's *_after/*_before
//     bounds and returns which of the two to pass, so naming where to begin is the one way
//     past the cap. Always report fetchOrdered's capped result through noteCapped — a cap
//     nobody can raise must never truncate silently.
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
//   - `logs` caps at 200 rather than listCap, and renders as it streams (the others build
//     a tabwriter, which sizes its columns from every row and so cannot).
//   - `definitions` offers `--sort name` for the alphabetical order, and is the one list
//     whose capped read keeps the *first* N rather than the newest — see fetchOrdered's
//     firstFirst. `--since`/`--until` still bound created_at under it, as filters over the
//     window rather than the point the walk starts from.
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
	case "compat":
		runCompatCmd(server, args)
	case "instances":
		runInstancesCmd(server, args)
	case "definitions":
		runDefinitionsCmd(server, args)
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

// instanceIDAndFlags parses an instance subcommand where the id may sit before or after
// the flags (`get <id> --json` and `pause --server X <id>` both work). The id must be
// explicit — a concrete id or "@last" (see resolveInstanceID).
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
  genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest]
  genctl validate -f file.yaml [-f file2.yaml ...]
  genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
  genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [-q]
  genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
  genctl instances [--status <status>] [--error-code <code>] [--sort updated|created] [--since <when>] [--until <when>] [--json]
  genctl definitions [--sort created|name] [--since <when>] [--until <when>] [--json]
  genctl external-tasks [--process <name>] [--version <n>] [--task <id>] [--since <when>] [--until <when>] [--json]
  genctl get      <instance-id> [--resolve] [--json]
  genctl logs     [--level <level>] [--since <when>] [--until <when>] [--time clock|full] [--recursive] [--resolve] [--mode basic|detail|json] <instance-id>
  genctl pause    <instance-id>
  genctl resume   <instance-id>
  genctl retry    [--force] <instance-id>
  genctl last
  genctl compat   <process> <from> <to>
  genctl compat   -f file.yaml [-f ...] --from <channel>
  genctl compat   --from <channel> --to <channel> [<process>]
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
  --since   read forward from here — a duration back from now (2h, 45m) or a timestamp
            (2006-01-02, "2006-01-02 15:04"). Without it a list shows its newest N
            (20; logs 200) and notes on stderr if that dropped any. No list takes
            --limit: --since is what reaches further back.
  --until   the far end of the window, same forms. [--since, --until) is half-open, so
            adjacent windows never repeat a row. Alone it keeps the cap, giving the
            newest N before that instant.
  --sort    instances: created (default) or updated; definitions: created or name.
            --since bounds whichever column the sort keys on.
  --to      compat: the channel to compare against. --from is its other end and is
            never defaulted: naming one side hides which two documents were compared.
  --allow-breaking-output
            compat: treat a broken output contract as upgradable. The process output
            changed shape, but no running instance is affected — so this is the flag
            for "I have dealt with the consumers". Affects the verdict and the exit
            code, never --json.
  --time    logs: the time column — clock (15:04:05, the default, with a
            "--- 2006-01-02 +02:00 ---" separator at each day change) or full
            (2006-01-02 15:04:05 +02:00 on every row, fixed-width, no separators)

Time zones:
  Timestamps display in the local zone and --since is read in it, so a time you read
  off a row is one you can pass back. Both follow $TZ: TZ=UTC genctl logs <id>
  Zones print as a numeric offset, never an abbreviation — "CST" is two zones fourteen
  hours apart, the same reason a definition's tz takes an IANA name or an offset only.
  --mode json is the exception — it passes the server's UTC RFC3339 through untouched,
  so machine output never depends on who ran the command.
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
