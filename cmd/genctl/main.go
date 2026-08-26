// genctl is a command-line gateway to a running genroc server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest]
//	genctl validate -f file.yaml [-f file2.yaml ...]
//	genctl types    -f file.yaml [-f file2.yaml ...]
//	genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
//	genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]
//	genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl instances [--status <status>] [--error-code <code>] [--sort updated|created] [--since <when>] [--until <when>] [--json]
//	genctl definitions [--sort created|name] [--since <when>] [--until <when>] [--json]
//	genctl external-tasks [--process <name>] [--version <n>] [--task <id>] [--since <when>] [--until <when>] [--json]
//	genctl upgrade  <process> --from <version|channel> --to <version|channel> [--status running,paused,failed] [--json]
//	genctl upgrade  <instance-id> [<instance-id> ...] --to <version|channel> [--json]
//	genctl get      <instance-id> [--resolve] [--json]
//	genctl object   <ref>
//	genctl logs     [--level <level>] [--since <when>] [--until <when>] [--time clock|full] [--recursive] [--mode basic|detail|json] <instance-id>
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
//	genctl pause    <instance-id> [<instance-id> ...]
//	genctl resume   <instance-id> [<instance-id> ...]
//	genctl retry    [--force] <instance-id> [<instance-id> ...]
//	genctl last
//
// get/logs/pause/resume/retry/signal require an instance id; pass @last for the most
// recently started instance (recorded by run). `genctl last` prints that id. upgrade takes
// either form: a process name sweeps its fleet, ids move those trees.
//
// pause/resume/retry act on every id named, one call each. They are assertions, so an id
// already in the state asserted prints "already" and does NOT fail the command — which is
// what lets a line that was only half applied be run again as-is. Only a refusal exits 1,
// and it stops neither the ids after it nor the exit code. specs/id-list-commands.md.
//
//	genctl compat   <process> <from> <to>
//	genctl compat   -f file.yaml [-f ...] --from <channel>
//	genctl compat   --from <channel> --to <channel> [<process>]
//	genctl compat   <instance-id> --to <version|channel> | <instance-id> -f file.yaml
//
// compat answers two questions about a pair of versions and gives each its own column:
// UPGRADE, could an instance running the older one continue under the newer; CONTRACT,
// does the newer still produce what consumers of the older were written against.
// --ignore contract excuses the second from the exit code, and nothing excuses the first.
// It is a shape check: a change of meaning (dollars to cents) compares equal, so the
// per-slot detail under the table is the deliverable.
//
// An instance id in place of the process reads both sides off the row — its process at the
// version it is on — so `compat <id> --to N` is the question `upgrade <id> --to N` answers
// by moving.
//
//	genctl channel list   <process>
//	genctl channel set    <process> <channel> <version>
//	genctl channel delete <process> <channel>
//	genctl promote  --from <channel> --to <channel> [--process <name>]
//	genctl status   --channel <channel>
//	genctl config   get <key>
//	genctl config   set <key> <value>
//
// A `$<resolver>: <path>` leaf in a source file is replaced by a string a binary named in
// the project's genroc.yaml produces, before anything is sent. Every command that reads a
// source file resolves first — apply, validate, and compat's -f, which compares what an
// apply WOULD store; types generates the resolver's declarations without building or
// applying. See specs/source-resolution.md.
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

	"genroc/internal/model"
)

// Command conventions (naming, --server, table/--json output, the no---limit list
// bounds rule) and the deliberate exceptions: cmd/genctl/CLAUDE.md.
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
	case "types":
		runTypesCmd(server, args)
	case "run":
		runRunCmd(server, args)
	case "resolve":
		runResolveCmd(server, args)
	case "object":
		runObjectCmd(server, args)
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
	case "upgrade":
		runUpgradeCmd(server, args)
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

// instanceIDAndFlags parses an instance subcommand that reads ONE instance (`get <id>
// --json`, `logs --server X <id>`). A second positional is refused rather than dropped:
// the assertion commands next door take id lists, so `get a b` is a thing people type,
// and an id silently discarded reads as if it had been shown.
func instanceIDAndFlags(fs *flag.FlagSet, args []string) string {
	ids := instanceIDsAndFlags(fs, args)
	if len(ids) > 1 {
		fatal("%s reads one instance, and %d ids were named", fs.Name(), len(ids))
	}
	return ids[0]
}

// instanceIDsAndFlags is the same parse for pause/resume/retry, which act on every id
// named. Ids may sit before or after the flags and each resolves on its own, so `@last`
// may appear among them (see resolveInstanceID).
func instanceIDsAndFlags(fs *flag.FlagSet, args []string) []string {
	pos := leadingArgs(fs, args)
	if len(pos) == 0 {
		pos = []string{""} // resolveInstanceID carries the message naming what is missing
	}
	ids := make([]string, len(pos))
	for i, ref := range pos {
		ids[i] = resolveInstanceID(ref)
	}
	return ids
}

// eachInstance runs one lifecycle assertion per id and reports each answer under its own
// id. Only a refusal fails the command: an id already in the asserted state is reported
// and forgiven, which is what lets a partially applied line converge when it is run again.
// specs/id-list-commands.md.
func eachInstance(ids []string, done string, do func(id string) (model.Outcome, error)) {
	var applied, already, refused int
	for _, id := range ids {
		outcome, err := do(id)
		switch {
		case err != nil:
			refused++
			fmt.Fprintf(os.Stderr, "genctl: %s: %v\n", id, err)
		case outcome == model.OutcomeUnchanged:
			already++
			fmt.Printf("already: %s\n", id)
		case outcome == model.OutcomeAccepted:
			applied++
			// Asked, not stopped: a task already in flight runs to its next boundary, so
			// saying "paused" here would claim the tree had come to rest.
			fmt.Printf("%s: %s  (draining a task already in flight)\n", done, id)
		default:
			applied++
			fmt.Printf("%s: %s\n", done, id)
		}
	}
	if len(ids) > 1 {
		fmt.Fprintf(os.Stderr, "\n%d named: %d %s, %d already, %d refused\n",
			len(ids), applied, done, already, refused)
	}
	if refused > 0 {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest]
  genctl validate -f file.yaml [-f file2.yaml ...]
  genctl types    -f file.yaml [-f file2.yaml ...]
  genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
  genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]
  genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]
  genctl instances [--status <status>] [--error-code <code>] [--sort updated|created] [--since <when>] [--until <when>] [--json]
  genctl definitions [--sort created|name] [--since <when>] [--until <when>] [--json]
  genctl external-tasks [--process <name>] [--version <n>] [--task <id>] [--since <when>] [--until <when>] [--json]
  genctl upgrade  <process> --from <version|channel> --to <version|channel> [--status running,paused,failed] [--json]
  genctl upgrade  <instance-id> [<instance-id> ...] --to <version|channel> [--json]
  genctl get      <instance-id> [--resolve] [--json]
  genctl object   <ref>
  genctl logs     [--level <level>] [--since <when>] [--until <when>] [--time clock|full] [--recursive] [--mode basic|detail|json] <instance-id>
  genctl pause    <instance-id> [<instance-id> ...]
  genctl resume   <instance-id> [<instance-id> ...]
  genctl retry    [--force] <instance-id> [<instance-id> ...]
  genctl last
  genctl compat   <process> <from> <to>
  genctl compat   -f file.yaml [-f ...] --from <channel>
  genctl compat   --from <channel> --to <channel> [<process>]
  genctl compat   <instance-id> --to <version|channel> | <instance-id> -f file.yaml
  genctl channel list   <process>
  genctl channel set    <process> <channel> <version>
  genctl channel delete <process> <channel>
  genctl promote  --from <channel> --to <channel> [--process <name>]
  genctl status   --channel <channel>
  genctl config   get <key>
  genctl config   set <key> <value>

Flags:
  -f        apply: definition file(s), YAML or JSON, multi-doc --- (repeatable);
            run/resolve/signal: read the input/result/payload from a file (path — tab-completes)
  --input   process input: a JSON/YAML literal, or - for stdin
  --result  external-task result (resolve/signal): a JSON/YAML literal, or - for stdin
  --task    the external task id to signal
  --code    resolve/signal: answer on the ERROR channel with this code; the result
            flags then carry the failure payload, conformed against raises[code]
  --message required with --code; lands on error.message
  --set     input/result/payload field key=value (repeatable; dotted keys nest, values type-inferred)
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
  --ignore  compat: excuse a check from the exit code. Only "contract" is accepted —
            the upgrade check answers for rows this deployment already owns, so nothing
            waves it through. It changes neither what is compared nor what is printed:
            the break is still reported, marked "(ignored)".
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
  --resolve get: fetch the values listed under "objects" and put them back where
            they belong. logs never resolves - a trail is scanned, not read, and
            those payloads are large by definition; it prints the ref instead, and
            "genctl object <ref>" fetches the one you want.
  -q        with run, print only the new instance id (id=$(genctl run NAME -q));
            with resolve/signal, suppress the confirmation line

Instance id:
  get/logs/pause/resume/retry/signal require an instance id; pass @last for the most
  recently started instance (recorded by run), or run "genctl last" to print it.
  pause/resume/retry take several, one call each. They assert a state, so an id already
  in it prints "already" and does not fail the command; run the same line again to
  finish one that was only half applied. A refusal stops neither the ids after it nor
  the exit code, which is 1 if any id was refused.
  upgrade takes either a process name (sweeps the fleet, needs --from) or instance ids
  (moves those trees, one call each; --from is the sweep's selector, so it is not
  needed). compat takes one id too, and reads the from side off the row: compat <id>
  --to N is the question upgrade <id> --to N answers by moving. It takes only one,
  because a comparison carries one version per process.

External tasks:
  external-tasks lists the queue of instances waiting on an external result, and who
  holds a live claim on each (CLAIMED BY; "-" means claimable).
  resolve takes a task's resolve token (the "<instance-id>.<nonce>" TOKEN column
  from that list); signal addresses a task by instance id + --task and buffers the
  result if the task is not armed yet. Both answer on the error channel with
  --code/--message instead of a result: the code is routed through the task's
  on_error rules like any other call error, and buffers the same way.

Config keys:
  server    genroc server base URL`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genctl: "+format+"\n", args...)
	os.Exit(1)
}
