// genctl is a command-line gateway to a running genroc server. Usage lives only in help.go's
// `commandDocs`; conventions and their exceptions in cmd/genctl/CLAUDE.md.
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

// Set at build time: -ldflags "-X main.version=0.1.0 -X main.commit=abc1234". The commit is not
// optional on a rolling channel: "edge" names a moving target.
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
	// Set before dispatch; env wins over the config file, as for `server`.
	authToken = os.Getenv("GENROC_TOKEN")
	if authToken == "" {
		authToken = cfg.Token
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Asked-for help goes to stdout and exits 0, so `genctl -h | less` works.
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
	case "generate":
		runGenerateCmd(args)
	case "schema":
		runSchemaCmd(args)
	case "lsp":
		runLSPCmd(args)
	case "run":
		runRunCmd(server, args)
	case "token":
		runTokenCmd(server, args)
	case "signal":
		runSignalCmd(server, args)
	case "object":
		runObjectCmd(server, args)
	case "get":
		runGetCmd(server, args)
	case "detail":
		runDetailCmd(server, args)
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
	case "cancel":
		runCancelCmd(server, args)
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

func addServerFlag(fs *flag.FlagSet, def string) *string {
	return fs.String("server", def, "genroc server base URL ($GENROC_SERVER)")
}

// instanceIDAndFlags is for a command that reads ONE instance. A second positional is refused,
// not dropped: the assertion commands take id lists, so `get a b` is a thing people type.
func instanceIDAndFlags(fs *flag.FlagSet, args []string) string {
	ids := instanceIDsAndFlags(fs, args)
	if len(ids) > 1 {
		fatal("%s reads one instance, and %d ids were named", fs.Name(), len(ids))
	}
	return ids[0]
}

// instanceIDsAndFlags is the parse for pause/resume/cancel/retry. Ids may sit either side of the
// flags and each resolves on its own, so `@last` may appear among them.
func instanceIDsAndFlags(fs *flag.FlagSet, args []string) []string {
	pos := leadingArgs(fs, args)
	if len(pos) == 0 {
		pos = []string{""} // resolveInstanceID carries the message naming what is missing
	}
	// EVERY positional is shape-checked before the first call: a table pasted in where ids were
	// meant would otherwise pause whichever cell happens to parse as an id.
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
			"  an instance id is an opaque digit-led token, or @last%s", quoteSome(bad, 3), not, listHint(len(bad)))
	}
	ids := make([]string, len(pos))
	for i, ref := range pos {
		ids[i] = resolveInstanceID(ref)
	}
	return ids
}

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

// listHint is offered only for several bad arguments — a list substituted in without -q. One
// is a typo, and guessing at a typo is noise.
func listHint(bad int) string {
	if bad < 2 {
		return ""
	}
	return "\n  if you substituted a list in, `genctl instances -q` prints ids and nothing else"
}

// eachInstance fails the command only on a refusal: an id already in the asserted state is
// forgiven, so a half-applied line converges when re-run. specs/id-list-commands.md.
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
			// Asked, not stopped: a task in flight runs to its next boundary.
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
