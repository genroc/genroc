// genctl is a command-line gateway to a running genroc server, inspired by kubectl. It reads
// process definition files (YAML or JSON, multi-document via ---) and forwards them to the
// server in a single API call.
//
// THE USER-FACING SURFACE IS help.go. Every command's grammar, its prose and its one-line
// summary live in `commandDocs`; `genctl <cmd> -h` prints that page plus the flags the
// command's own flag set declares. A usage line repeated here would be a second copy to keep
// true. Conventions and the deliberate exceptions: cmd/genctl/CLAUDE.md.
//
// Two notes about the CODE rather than the surface:
//
// --since/--until are CLI-side helpers only. Each resolves to unix millis and goes out as the
// endpoint's created_after/updated_after -- whichever column the active sort keys on -- with
// order=asc, so past the cap the read walks forward toward now, printing pages as they
// arrive, in the direction it displays. A *duration* resolves against this machine's clock
// rather than the server's; see parseWhen for when that distinction bites.
//
// compat answers two questions about a pair of versions and gives each its own column:
// UPGRADE, could an instance running the older one continue under the newer; CONTRACT, does
// the newer still produce what consumers of the older were written against. It is a shape
// check -- a change of meaning (dollars to cents) compares equal -- so the per-slot detail
// under the table is the deliverable.
//
// Environment: GENROC_SERVER (default http://localhost:8448), GENROC_TOKEN, TZ.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"genroc/internal/model"
)

// Set at build time: -ldflags "-X main.version=0.1.0 -X main.commit=abc1234". A binary that
// cannot say what it is makes every bug report start with a guess -- and on a rolling channel
// the version alone is not enough, because "edge" names a moving target.
var (
	version = "dev"
	commit  = ""
)

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
	// Set before dispatch, so every request carries it without threading a parameter through
	// each command. Env wins over the config file, the same precedence `server` uses.
	authToken = os.Getenv("GENROC_TOKEN")
	if authToken == "" {
		authToken = cfg.Token
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Asking for help is not an error: it goes to stdout and exits 0, so `genctl -h | less`
	// works and a script checking the status does not see a failure.
	switch cmd {
	case "-h", "--help", "help":
		usageTo(os.Stdout)
		return
	case "-v", "--version", "version":
		fmt.Println(versionString())
		return
	}

	switch cmd {
	case "apply":
		runApplyCmd(server, args)
	case "types":
		runTypesCmd(args)
	case "schema":
		runSchemaCmd(args)
	case "run":
		runRunCmd(server, args)
	case "token":
		runTokenCmd(server, args)
	case "resolve":
		runResolveCmd(server, args)
	case "object":
		runObjectCmd(server, args)
	case "get":
		runGetCmd(server, args)
	case "channel":
		runChannelCmd(server, args)
	case "compat":
		runCompatCmd(server, args)
	case "upgrade":
		runUpgradeCmd(server, args)
	case "instances":
		runInstancesCmd(server, args)
	case "definitions":
		runDefinitionsCmd(server, args)
	case "logs":
		runLogsCmd(server, args)
	case "pause":
		runPauseCmd(server, args)
	case "resume":
		runResumeCmd(server, args)
	case "retry":
		runRetryCmd(server, args)
	case "init":
		runInitCmd(args)
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

// instanceIDOrToken parses resolve's one positional, which is a queue token OR an instance id.
// It shape-checks neither: a token is the server's to recognise, and which of the two this is
// decides the endpoint, so the caller reads the shape itself.
func instanceIDOrToken(fs *flag.FlagSet, args []string) string {
	pos := leadingArgs(fs, args)
	if len(pos) == 0 {
		fatal("resolve needs a queue token, or an instance id with --task")
	}
	if len(pos) > 1 {
		fatal("resolve submits one outcome, and %d were named", len(pos))
	}
	return pos[0]
}

// instanceIDsAndFlags is the same parse for pause/resume/retry, which act on every id
// named. Ids may sit before or after the flags and each resolves on its own, so `@last`
// may appear among them (see resolveInstanceID).
func instanceIDsAndFlags(fs *flag.FlagSet, args []string) []string {
	pos := leadingArgs(fs, args)
	if len(pos) == 0 {
		pos = []string{""} // resolveInstanceID carries the message naming what is missing
	}
	// EVERY positional is shape-checked before the first call goes out. A list that is not
	// id-shaped is a malformed command, not a job that half applies — the distinction is
	// the same conflict-vs-mistake one the outcomes draw, moved one step earlier: what can
	// be known without asking the server must not be discovered halfway through mutating.
	// The case this exists for is a table pasted in where ids were meant (`instances`
	// without -q), which otherwise pauses whichever cell happens to parse as a UUID while
	// reporting a "not found" for every other word on the screen.
	var bad []string
	for _, ref := range pos {
		if ref != "" && !isInstanceRef(ref) {
			bad = append(bad, ref)
		}
	}
	if len(bad) > 0 {
		not := "is not an instance id"
		if len(bad) > 1 {
			not = "are not instance ids"
		}
		fatal("%s %s — nothing was sent.\n"+
			"  an instance id is a UUID, or @last%s", quoteSome(bad, 3), not, listHint(len(bad)))
	}
	ids := make([]string, len(pos))
	for i, ref := range pos {
		ids[i] = resolveInstanceID(ref)
	}
	return ids
}

// quoteSome renders at most n of the arguments, so a whole mistyped table reports as one
// line rather than one line per cell.
func quoteSome(args []string, n int) string {
	shown := args
	if len(shown) > n {
		shown = shown[:n]
	}
	quoted := make([]string, len(shown))
	for i, a := range shown {
		quoted[i] = strconv.Quote(a)
	}
	out := strings.Join(quoted, ", ")
	if len(args) > len(shown) {
		out += fmt.Sprintf(" and %d more", len(args)-len(shown))
	}
	return out
}

// listHint points at the one mistake that produces a screenful of non-ids: a list command
// substituted in without -q, so the table's headers and cells arrive as arguments. Offered
// only for several bad arguments — one is a typo, and guessing at a typo is noise.
func listHint(bad int) string {
	if bad < 2 {
		return ""
	}
	return "\n  if you substituted a list in, `genctl instances -q` prints ids and nothing else"
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genctl: "+format+"\n", args...)
	os.Exit(1)
}
