package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// `genctl -h` is the MAP -- one line per command, and nothing a reader has to skip past to
// find the command they want. `genctl <cmd> -h` is the page: the grammar, the prose that one
// command needs, and its flags printed from its own flag set, so a flag is described where it
// is declared and nowhere else.

type commandDoc struct {
	summary string   // the one line in the map
	usage   []string // grammar, without the leading "genctl "
	detail  string   // paragraphs; printed by `genctl <cmd> -h` only
}

// Shared paragraphs: a rule governing several commands is written once, because the version
// someone reads has to be the version that is true.
const (
	definitionFiles = "Files: -f takes several values and stops at the next flag; with no -f, `definitions:` in\n" +
		"the nearest .genroc. An existing path is used literally, anything else is globbed (**\n" +
		"matches any depth). A directory is refused."

	listWindow = `Oldest to newest, so the newest is nearest the prompt. No --limit: each list shows its
newest N (20; logs 200) and says on stderr when that dropped rows. --since reaches further
back -- a duration (2h, 45m) or a timestamp -- and --until is its far end; [since, until)
is half-open. Times display in, and are read in, the local zone ($TZ).`

	instanceRefs = `An instance id is a UUID, or @last for the most recently started one (recorded by run).`
)

var commandDocs = map[string]commandDoc{
	"apply": {
		summary: "register definitions; --check-only checks them and stores nothing",
		usage:   []string{"apply [-f <path|glob> ...] [--channel latest] [--check-only] [--json]"},
		detail: "A batch is one logical change: all are validated before any is written, and a child that\n" +
			"exists only in the batch resolves against it. Identical bytes mint no new version.\n" +
			"`$<resolver>:` leaves resolve first, on --check-only too. --json is the server's answer.\n\n" +
			definitionFiles,
	},
	"types": {
		summary: "write the type declarations a resolver's scripts import",
		usage:   []string{"types [-f <path|glob> ...]"},
		detail: `Runs each phase-2 resolver in "types" mode and writes what it generates, so an editor has
the declarations before an apply ever runs. Needs no server: the types are inferred here.

` + definitionFiles,
	},
	"schema": {
		summary: "what a slot's type is, and what an expression there can read",
		usage: []string{
			"schema type    <process> [address] [-e <expression>] [-f <path|glob> ...] [--json]",
			"schema context <process> [address] [-e <expression>] [-f <path|glob> ...] [--json]",
		},
		detail: "`type` is what shape a slot IS, `context` what an expression there may READ. Same\n" +
			"addresses -- input, output, tasks.<id>.output, tasks.<id>.action.input, raises[\"a.code\"]\n" +
			"-- and an address may continue into the schema; quote a non-identifier key:\n" +
			"tasks[\"step one\"].output. With no address, each lists every slot it answers for. -e types\n" +
			"one expression at that address. Neither needs a server.\n\n" +
			definitionFiles,
	},
	"compat": {
		summary: "compare two versions and report what a move would break",
		usage: []string{
			"compat --from <sel> [-f <path|glob> ...]",
			"compat --from <sel> --to <sel> [--process <name>] [--ignore contract] [--json]",
			"compat <instance-id> --to <version|channel>",
		},
		detail: "A side is one channel OR name@version pins (a version may itself be a channel); mixing\n" +
			"the two is refused, and --from is never defaulted. With -f the local files are the target\n" +
			"side. An instance id names the from side by itself: `compat <id> --to N` is the question\n" +
			"`upgrade <id> --to N` answers by moving.\n\n" +
			"Exits non-zero on a break, so it drops into a pipeline. --ignore contract drops that one\n" +
			"check from the exit code and nothing else -- the break is still printed, as \"(ignored)\".\n\n" +
			definitionFiles,
	},
	"definitions": {
		summary: "list registered definitions",
		usage:   []string{"definitions [--sort created|name] [--since <when>] [--until <when>] [--json]"},
		detail: `The one list whose cap keeps the FIRST N rather than the newest, since --sort name walks
an alphabet rather than a history.

` + listWindow,
	},

	"run": {
		summary: "start an instance",
		usage:   []string{"run <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]"},
		detail: `Input from --input (a literal, or - for stdin), -f, or --set k=v -- dotted keys nest,
values are type-inferred, and --set overrides the others. Latest version unless --channel
or --version. -q prints only the new id: id=$(genctl run NAME -q).`,
	},
	"instances": {
		summary: "list instances (roots only unless --children)",
		usage: []string{
			"instances [--process <name>] [--version <n>] [--status <status>] [--error-code <code>]",
			"          [--children] [--sort updated|created] [--since <when>] [--until <when>] [--json | -q]",
		},
		detail: `Roots only -- one row per tree, which is the unit pause/resume/retry and upgrade act on.
--children adds them back and turns on a PARENT column, since nothing else on a row tells
the two apart. -q prints bare ids, and nothing at all when empty, for nesting:

  genctl pause $(genctl instances -q --status running)

` + listWindow,
	},
	"get": {
		summary: "show one instance",
		usage:   []string{"get <instance-id> [--resolve] [--json]"},
		detail: instanceRefs + ` A second id is refused rather than dropped.

--resolve fetches the values listed under "objects" and puts them back inline; without it a
large value prints as a ref.`,
	},
	"logs": {
		summary: "print an instance's log trail",
		usage: []string{
			"logs [--level <level>] [--since <when>] [--until <when>] [--time clock|full]",
			"     [--recursive] [--mode basic|detail|json] <instance-id>",
		},
		detail: "--mode: basic is a line per entry, detail adds the payloads, json is JSONL and the one\n" +
			"output that keeps the server's UTC RFC3339. --recursive follows the tree into children.\n" +
			"Refs are never resolved here -- a trail is scanned, not read; `genctl object <ref>`\n" +
			"fetches one. --time full puts the date on every row instead of a per-day separator.\n\n" +
			instanceRefs + "\n\n" + listWindow,
	},
	"pause":  {summary: "stop an instance from advancing", usage: []string{"pause <instance-id> [<instance-id> ...]"}, detail: assertionHelp},
	"resume": {summary: "let a paused instance advance again", usage: []string{"resume <instance-id> [<instance-id> ...]"}, detail: assertionHelp},
	"retry": {
		summary: "retry a failed instance's current task",
		usage:   []string{"retry [--force] <instance-id> [<instance-id> ...]"},
		detail: assertionHelp + `

--force overrides only_once protection, where a retried task may already have taken effect.`,
	},
	"upgrade": {
		summary: "move instances to another version",
		usage: []string{
			"upgrade <process> --from <version|channel> --to <version|channel> [--status running,paused,failed] [--json]",
			"upgrade <instance-id> [<instance-id> ...] --to <version|channel> [--json]",
		},
		detail: "A process name sweeps its fleet and needs --from, the selector saying which rows move.\n" +
			"Ids move those trees instead, one call each, and refuse --from/--status: an id selects\n" +
			"already. An instance moves only where the new version is compatible with where it is\n" +
			"parked -- `genctl compat` asks the same question without moving anything.\n\n" +
			instanceRefs,
	},
	"resolve": {
		summary: "answer an external task, by queue token or by instance id",
		usage: []string{
			"resolve <token> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]",
			"resolve <instance-id> --task <task-id> [same flags]",
		},
		detail: `One submission, addressed the two ways it can be. A worker claims a task off the queue --
there is no listing endpoint -- and answers with the token that claim returned. Anyone else
uses the instance id and --task, which BUFFERS the outcome if the task is not armed yet;
the confirmation line says which happened.

--code/--message answers on the ERROR channel instead of with a result, routed through the
task's on_error rules like any other call error. No result flags at all means an empty
outcome: valid for a task declaring no result_schema, refused otherwise.

` + instanceRefs,
	},
	"object": {
		summary: "print a stored object by ref",
		usage:   []string{"object <ref>"},
		detail: `A value too large to inline is stored once and referenced. get --resolve puts them back;
logs never does, so this fetches the one payload you want.`,
	},

	"channel": {
		summary: "move, read and check the pointers versions are deployed behind",
		usage: []string{
			"channel list    <process>",
			"channel set     <process> <channel> <version>",
			"channel delete  <process> <channel>",
			"channel promote --from <channel> --to <channel> [--process <name>]",
			"channel status  [<channel>]",
		},
		detail: "A channel is a named pointer to a version, per process; `latest` is the one apply moves.\n" +
			"promote copies every pointer from one channel to another, so a deployment moves as one;\n" +
			"--process narrows it to that process and its dependency subtree. status is a coherence\n" +
			"report: it prints only members whose child references are baked at a version the channel\n" +
			"no longer points at.",
	},

	"token": {
		summary: "manage API credentials",
		usage: []string{
			"token create --perms <list> [--label <name>] [-q]",
			"token generate | token list [--json] | token revoke <id>...",
		},
		detail: "Perms: admin, deploy, operate, read, worker.\n\n" +
			"create registers a credential over the API and so needs an admin one of its own.\n" +
			"generate mints a secret OFFLINE -- no server, no credential -- which is how the first\n" +
			"one can exist at all. Break-glass equivalent: `genroc token`, run against the database.",
	},
	"init": {
		summary: "scaffold a project, or mint a new UI password",
		usage: []string{
			"init [dir] [--eval-node] [--no-auth] [--postgres] [--version <tag>] [-y]",
			"init password [email]",
		},
		detail: "Writes a project that applies and runs: definitions/, a .genroc naming them, optionally\n" +
			"a compose.yaml. It asks which parts you want; -y takes the defaults, as does a pipe or a\n" +
			"CI job. `init password` mints a replacement for the UI login init printed.",
	},
	"config": {
		summary: "read and write ~/.config/genroc/config.yaml",
		usage:   []string{"config get <key> | set <key> <value> | unset <key>"},
		detail: `Keys (the file is mode 0600):

  server    genroc server base URL                    ($GENROC_SERVER wins)
  token     API credential, a genroc_sk_* value       ($GENROC_TOKEN wins)

--server on a command overrides both.`,
	},
}

const assertionHelp = `Takes several ids and acts on every one, one call each. These are ASSERTIONS: an id already
in the state prints "already" and does NOT fail, so a line that was only half applied can be
run again as-is. Only a refusal exits 1, and it stops neither the ids after it nor the code.

` + instanceRefs

// The map, in the order it prints. A command is here or it is not reachable: main's dispatch
// and this listing are pinned to each other by TestEveryCommandIsDocumented.
var helpGroups = []struct {
	title string
	names []string
}{
	{"Definitions", []string{"apply", "types", "schema", "compat", "definitions"}},
	{"Instances", []string{"run", "instances", "get", "logs", "pause", "resume", "retry", "upgrade", "resolve", "object"}},
	{"Channels", []string{"channel"}},
	{"Setup", []string{"init", "config", "token"}},
}

// usage writes to w: stderr when it accompanies an error, stdout when it IS the answer
// (`genctl -h`), so help can be piped without redirecting stderr.
func usage() { usageTo(os.Stderr) }

func usageTo(w io.Writer) {
	fmt.Fprintln(w, "Usage: genctl <command> [arguments]")
	for _, g := range helpGroups {
		fmt.Fprintf(w, "\n%s\n", g.title)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, name := range g.names {
			fmt.Fprintf(tw, "  %s\t%s\n", name, commandDocs[name].summary)
		}
		tw.Flush()
	}
	fmt.Fprint(w, `
  genctl <command> -h    the grammar, the flags and the rules for one command
  genctl -v              this binary's version

Environment: $GENROC_SERVER, $GENROC_TOKEN, $TZ -- or `+"`genctl config`"+` on disk; --server wins.
`)
}

// newFlagSet is every command's flag set, and the reason `<cmd> -h` answers with more than a
// list of flags. args is captured so Usage knows WHY it was called: help asked for goes to
// stdout, help accompanying a parse error follows the error to stderr.
func newFlagSet(name string, args []string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		w := fs.Output()
		if hasHelpArg(args) {
			w = os.Stdout
		}
		printCommandHelp(w, name, fs)
	}
	return fs
}

func hasHelpArg(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}

// helpFor prints what a command's own parser cannot: a subcommand dispatcher reads a
// positional before any flag set exists, so `genctl channel -h` never reaches flag.Parse.
// The flags then belong to the subcommand -- `genctl channel promote -h` prints those.
func helpFor(name string) {
	printCommandHelp(os.Stdout, name, flag.NewFlagSet(name, flag.ExitOnError))
}

// missingSubcommand is the same page where it ACCOMPANIES an error: stderr, exit 1, so a
// script's stdout carries no help text and the shell sees the failure.
func missingSubcommand(name string) {
	printCommandHelp(os.Stderr, name, flag.NewFlagSet(name, flag.ExitOnError))
	os.Exit(1)
}

// printCommandHelp renders one command: grammar, prose, then its flags as the flag set
// declares them. The set is the only source for the flags, so a renamed flag cannot leave a
// stale line behind in the prose.
func printCommandHelp(w io.Writer, name string, fs *flag.FlagSet) {
	doc, ok := commandDocs[strings.Fields(name)[0]]
	if !ok {
		usageTo(w)
		return
	}
	fmt.Fprintln(w, "Usage:")
	for _, u := range doc.usage {
		// A line starting with a space CONTINUES the one above it, so a long grammar wraps
		// under itself instead of reading as a second way to call the command.
		if strings.HasPrefix(u, " ") {
			fmt.Fprintln(w, "         "+strings.TrimLeft(u, " "))
			continue
		}
		fmt.Fprintln(w, "  genctl "+u)
	}
	if doc.detail != "" {
		fmt.Fprintf(w, "\n%s\n", doc.detail)
	}
	printFlags(w, fs)
}

func printFlags(w io.Writer, fs *flag.FlagSet) {
	seen := false
	fs.VisitAll(func(*flag.Flag) { seen = true })
	if !seen {
		return
	}
	fmt.Fprintln(w, "\nFlags:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fs.VisitAll(func(f *flag.Flag) {
		dash := "--"
		if len(f.Name) == 1 {
			dash = "-"
		}
		text := f.Usage
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			text += fmt.Sprintf(" (default %s)", f.DefValue)
		}
		for i, line := range wrapText(text, 62) {
			if i == 0 {
				fmt.Fprintf(tw, "  %s%s\t%s\n", dash, f.Name, line)
				continue
			}
			fmt.Fprintf(tw, "  \t%s\n", line)
		}
	})
	tw.Flush()
}

// wrapText keeps a long flag description inside the terminal without the flag package's
// hanging-indent form, which puts the name and its explanation on different lines.
func wrapText(s string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines, line = append(lines, line), word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
