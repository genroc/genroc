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

// Shared paragraphs: a rule governing several commands is written once.
const (
	definitionFiles = "Files: -f takes several values and repeats; without -f, the `definitions:` list in the\n" +
		"nearest .genroc is used. Paths are globbed (** matches any depth); a directory is refused."

	listWindow = `Shows the newest 20 rows (logs: 200), oldest first, and says on stderr when rows were left
out. --since and --until take a duration (2h, 45m) or a timestamp (2006-01-02 15:04), read
in the local zone ($TZ).`

	instanceRefs = `An instance is named by its id (6fah8w2p) or @last, the one run started most recently.`
)

var commandDocs = map[string]commandDoc{
	"apply": {
		summary: "register definitions; --check-only checks them and stores nothing",
		usage:   []string{"apply [-f <path|glob> ...] [--channel latest] [--check-only] [--json]"},
		detail: "Validates every definition before registering any. An unchanged definition creates no new\n" +
			"version. Each line says what happened on --channel: `new` version, `existing` version, or\n" +
			"`current` (nothing moved). Resolvers (`$<resolver>:`) run first, with --check-only too.\n\n" +
			definitionFiles,
	},
	"types": {
		summary: "write the type declarations a resolver's scripts import",
		usage:   []string{"types [-f <path|glob> ...]"},
		detail: `Writes the declarations each resolver generates, so an editor has them before the first
apply. Needs no server.

` + definitionFiles,
	},
	"schema": {
		summary: "what a slot's type is, and what an expression there can read",
		usage: []string{
			"schema type    <process> [address] [-e <expression>] [-f <path|glob> ...] [--json]",
			"schema context <process> [address] [-e <expression>] [-f <path|glob> ...] [--json]",
		},
		detail: "`type` prints a slot's schema; `context` prints what an expression there can read.\n" +
			"Addresses: input, output, tasks.<id>.output, tasks.<id>.action.input, raises[\"code\"],\n" +
			"optionally continuing into the schema (tasks[\"step one\"].output.items). With no address,\n" +
			"lists every slot. -e types one expression at that address. Needs no server.\n\n" +
			definitionFiles,
	},
	"compat": {
		summary: "compare two versions and report what a move would break",
		usage: []string{
			"compat --from <sel> [-f <path|glob> ...]",
			"compat --from <sel> --to <sel> [--process <name>] [--ignore contract] [--json]",
			"compat <instance-id> --to <version|channel>",
		},
		detail: "A side is a channel or name@version pins, not both. With -f, the local files are the\n" +
			"target. `compat <id> --to N` checks one instance; `upgrade` is the same with the move.\n\n" +
			"Exits 1 on a break. --ignore contract still prints contract breaks but does not fail.\n\n" +
			definitionFiles,
	},
	"definitions": {
		summary: "list registered definitions",
		usage:   []string{"definitions [--sort created|name] [--since <when>] [--until <when>] [--json]"},
		detail: `With --sort name, shows the first 20 alphabetically rather than the newest.

` + listWindow,
	},

	"run": {
		summary: "start an instance",
		usage:   []string{"run <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]"},
		detail: `Input comes from --input (a literal, or - for stdin), -f, or --set k=v (dotted keys nest,
values are type-inferred); --set wins. Runs the latest version unless --channel or --version.

  id=$(genctl run hello -q)`,
	},
	"instances": {
		summary: "list instances (roots only unless --children)",
		usage: []string{
			"instances [--process <name>] [--version <n>] [--status <status>] [--error-code <code>]",
			"          [--phase <phase>] [--task <task-id>] [--children] [--sort updated|created]",
			"          [--since <when>] [--until <when>] [--json | -q]",
		},
		detail: `Lists root instances; --children adds child instances and a PARENT column. A phase is
shown after the status (running·external).

  genctl instances --status failed
  genctl instances --phase external
  genctl pause $(genctl instances -q --status running)

` + listWindow,
	},
	"get": {
		summary: "show one instance and what it produced",
		usage:   []string{"get <instance-id> [--resolve] [--json]"},
		detail: instanceRefs + `

Prints the status, the error and the ` + "`output:`" + ` block. ` + "`detail`" + ` adds the internal state.
--resolve inlines large values that print as refs.`,
	},
	"detail": {
		summary: "show everything stored on one instance",
		usage:   []string{"detail <instance-id> [--resolve] [--json]"},
		detail: instanceRefs + `

Everything ` + "`get`" + ` prints, plus parent, children, lease, epochs and the full state.
--resolve inlines large values that print as refs.`,
	},
	"lsp": {
		summary: "run the language server an editor talks to over stdio",
		usage:   []string{"lsp [--stdio]"},
		detail: "Language server for `*.genroc.yaml` over stdin/stdout, started by an editor. Reports the\n" +
			"same errors as `apply`. --stdio is accepted and ignored.\n\n" +
			"VS Code: the extension in editors/vscode. Neovim: `genctl lsp` as the `cmd` for `yaml`.",
	},
	"logs": {
		summary: "print an instance's log trail",
		usage: []string{
			"logs [--level <level>] [--since <when>] [--until <when>] [--time clock|full]",
			"     [--flat] [--mode basic|detail] [--json] <instance-id>",
		},
		detail: "A root id shows its whole tree with an ID column; --flat shows the root's rows only.\n" +
			"--json prints JSONL in UTC. Refs are not resolved; `genctl object <ref>` prints one.\n\n" +
			instanceRefs + "\n\n" + listWindow,
	},
	"pause":  {summary: "stop an instance from advancing", usage: []string{"pause <instance-id> [<instance-id> ...]"}, detail: assertionHelp},
	"resume": {summary: "let a paused instance advance again", usage: []string{"resume <instance-id> [<instance-id> ...]"}, detail: assertionHelp},
	"cancel": {summary: "stop an instance for good", usage: []string{"cancel <instance-id> [<instance-id> ...]"},
		detail: "Final: a cancelled instance cannot be resumed or retried. Use pause to stop temporarily.\n\n" +
			assertionHelp},
	"retry": {
		summary: "retry a failed instance's current task",
		usage:   []string{"retry [--force] <instance-id> [<instance-id> ...]"},
		detail: `--force retries an only_once task, which may already have taken effect.

` + assertionHelp,
	},
	"upgrade": {
		summary: "move instances to another version",
		usage: []string{
			"upgrade <process> --from <version|channel> --to <version|channel> [--status running,paused,failed] [--json]",
			"upgrade <instance-id> [<instance-id> ...] --to <version|channel> [--json]",
		},
		detail: "By process, moves the instances on --from (narrowed by --status); by id, moves those.\n" +
			"An instance moves only if the new version is compatible with where it is; `genctl compat`\n" +
			"checks without moving.\n\n" +
			instanceRefs,
	},
	"signal": {
		summary: "deliver an outcome to an instance's external task by id",
		usage: []string{
			"signal <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [--code C --message M] [-q]",
		},
		detail: `Answers an external task without claiming it. Sent before the task is waiting, it is
buffered until it is. --code/--message sends an error instead, handled by the task's
on_error. No result flags sends an empty result, allowed only without a result_schema.

` + instanceRefs,
	},
	"object": {
		summary: "print a stored object by ref",
		usage:   []string{"object <ref>"},
		detail:  `Prints a large value that other output shows as a ref.`,
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
		detail: "A channel is a named pointer to a version of a process; apply moves `latest`.\n" +
			"promote copies every pointer of one channel to another; --process limits it to that\n" +
			"process and its children. status lists members whose children are pinned to versions the\n" +
			"channel no longer points at.",
	},

	"token": {
		summary: "manage API credentials",
		usage: []string{
			"token create --perms <list> [--label <name>] [-q]",
			"token generate | token list [--json] | token revoke <id>...",
		},
		detail: "Perms: admin, deploy, operate, read, worker.\n\n" +
			"create needs an admin token. generate makes a secret offline, for the first token.\n" +
			"Without any token: `genroc token`, run against the database.",
	},
	"init": {
		summary: "scaffold a project, or mint a new UI password",
		usage: []string{
			"init [dir] [--eval-node] [--auth] [--postgres] [--version <tag>] [-y]",
			"init password [email]",
		},
		detail: "Writes definitions/, a .genroc and optionally a compose.yaml, asking which parts you\n" +
			"want; -y (or no terminal) takes the defaults. `init password` mints a new UI password.",
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

const assertionHelp = `Takes several ids. An id already in the target state prints "already" and does not fail,
so the command is safe to re-run. Exits 1 only if an id was refused; the rest still run.

` + instanceRefs

// The map, in the order it prints. A command is here or it is not reachable: main's dispatch
// and this listing are pinned to each other by TestEveryCommandIsDocumented.
var helpGroups = []struct {
	title string
	names []string
}{
	{"Definitions", []string{"apply", "types", "schema", "compat", "definitions"}},
	{"Instances", []string{"run", "instances", "get", "detail", "logs", "pause", "resume", "cancel", "retry", "upgrade", "signal", "object"}},
	{"Channels", []string{"channel"}},
	{"Setup", []string{"init", "config", "token", "lsp"}},
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
