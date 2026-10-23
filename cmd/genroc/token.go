package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"genroc/internal/api"
	"genroc/internal/db"
)

// `genroc token …` works against the DATABASE, not the API: the break-glass path for a lost
// last admin token, trusted via filesystem access. specs/api-auth.md §5.3, path 2.
func runTokenCmd(args []string) {
	if len(args) == 0 {
		tokenUsage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("token "+sub, flag.ExitOnError)
	// Same defaults as the server's, so inside its container this finds the server's database.
	dbPath := fs.String("db", envOr("GENROC_DB", "genroc.db"), "SQLite database file path ($GENROC_DB)")
	pgDSN := fs.String("pg", os.Getenv("GENROC_PG"), "PostgreSQL DSN ($GENROC_PG). When set, --db is ignored.")
	label := fs.String("label", "", "a name for this token, shown in listings and recorded as the actor")
	perms := fs.String("perms", "", "comma-separated permissions: admin, deploy, operate, read, worker")

	switch sub {
	case "create", "list", "revoke":
	default:
		fmt.Fprintf(os.Stderr, "genroc token: unknown subcommand %q\n", sub)
		tokenUsage()
		os.Exit(2)
	}
	ids := parseInterspersed(fs, rest)
	if sub != "revoke" && len(ids) > 0 {
		fmt.Fprintf(os.Stderr, "genroc token %s: takes no arguments, got %q\n", sub, ids)
		os.Exit(2)
	}

	database, err := openTokenDB(*dbPath, *pgDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "genroc token: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	ctx := context.Background()
	switch sub {
	case "create":
		if *perms == "" {
			fmt.Fprintln(os.Stderr, "genroc token create: --perms is required (e.g. --perms admin)")
			os.Exit(2)
		}
		list, err := parsePerms(*perms)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genroc token create: %v\n", err)
			os.Exit(2)
		}
		tok, err := database.MintToken(ctx, *label, list, 0, db.ActorTokenCreate)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genroc token create: %v\n", err)
			os.Exit(1)
		}
		// The secret goes to STDOUT so it can be captured; everything else to stderr, so
		// `TOKEN=$(genroc token create --perms admin)` yields the credential alone.
		fmt.Fprintf(os.Stderr, "created %s  perms=%s\n  shown once:\n", tok.ID, strings.Join(list, ","))
		fmt.Println(tok.Secret)
	case "list":
		rows, err := database.ListTokens(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "genroc token list: %v\n", err)
			os.Exit(1)
		}
		printTokens(rows)
	case "revoke":
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "usage: genroc token revoke <id> [--db path | --pg dsn]")
			os.Exit(2)
		}
		for _, id := range ids {
			if err := database.RevokeToken(ctx, id, db.ActorTokenRevoke); err != nil {
				fmt.Fprintf(os.Stderr, "genroc token revoke %s: %v\n", id, err)
				os.Exit(1)
			}
			fmt.Printf("revoked: %s\n", id)
		}
	}
}

// parseInterspersed takes flags after the ids too, as the usage line writes them: flag.Parse stops
// at the first positional, so `revoke <id> --db x` read `--db` as an id and opened ./genroc.db.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			return positional
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func openTokenDB(dbPath, pgDSN string) (*db.DB, error) {
	if pgDSN != "" {
		return db.OpenPostgres(pgDSN, 0)
	}
	return db.OpenSQLite(dbPath, "")
}

func parsePerms(csv string) ([]string, error) {
	return api.ValidPerms(splitPerms(csv))
}

func splitPerms(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printTokens(rows []db.APIToken) {
	if len(rows) == 0 {
		fmt.Println("no tokens")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLABEL\tPERMS\tACTOR\tCREATED\tLAST USED\tEXPIRES\tSTATUS")
	now := time.Now().UnixMilli()
	for _, r := range rows {
		// Expiry is a status, not just a column: a lapsed token reported as "live" is exactly
		// the row an operator would skip while wondering why the caller gets 401.
		status := "live"
		switch {
		case r.RevokedAt != 0:
			status = "revoked"
		case r.ExpiresAt != 0 && r.ExpiresAt <= now:
			status = "expired"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, dash(r.Label), strings.Join(r.Perms, ","), dash(r.Actor),
			stamp(r.CreatedAt), stamp(r.LastUsedAt), stamp(r.ExpiresAt), status)
	}
	w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stamp renders a millisecond timestamp, and a dash for "never" — a blank cell and a zero time
// read the same in a table someone is scanning for a stale credential.
func stamp(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}

func tokenUsage() {
	fmt.Fprint(os.Stderr, `genroc token — credential management against the database.

  genroc token create --perms <list> [--label <name>] [--db path | --pg dsn]
  genroc token list                                   [--db path | --pg dsn]
  genroc token revoke <id>...                         [--db path | --pg dsn]

Permissions: admin, deploy, operate, read, worker.

This works without the server running and without a credential — it needs only access to the
database, which is the correct root of trust for a break-glass path. Day to day, use
`+"`genctl token`"+`, which goes over the API.
`)
}
